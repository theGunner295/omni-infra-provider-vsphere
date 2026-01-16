// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package provider implements vsphere infra provider core.
package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/siderolabs/omni/client/pkg/infra/provision"
	"github.com/siderolabs/omni/client/pkg/omni/resources/infra"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	"go.uber.org/zap"

	"github.com/siderolabs/omni-infra-provider-vsphere/internal/pkg/provider/resources"
)

const (
	GiB = uint64(1024 * 1024 * 1024)
)

// isAuthenticationError checks if an error is related to authentication/session expiry.
func isAuthenticationError(err error) bool {
	if err == nil {
		return false
	}

	errMsg := err.Error()
	// Check for common authentication-related error messages
	return strings.Contains(errMsg, "NotAuthenticated") ||
		strings.Contains(errMsg, "not authenticated") ||
		strings.Contains(errMsg, "session is not authenticated") ||
		strings.Contains(errMsg, "The session is not authenticated")
}

// ensureAuthenticated checks if the session is active and attempts to re-login if needed.
func (p *Provisioner) ensureAuthenticated(ctx context.Context) error {
	manager := session.NewManager(p.vsphereClient.Client)
	
	// Check if session is active
	active, err := manager.SessionIsActive(ctx)
	if err != nil {
		p.logger.Warn("failed to check session status", zap.Error(err))
		// Continue to attempt re-login
		active = false
	}
	
	if !active {
		p.logger.Info("vSphere session is not active, attempting to re-login")
		
		// Logout first to clear any stale session
		_ = manager.Logout(ctx)
		
		// Re-login using stored credentials
		if p.vsphereURL == nil || p.vsphereURL.User == nil {
			return fmt.Errorf("vSphere credentials not available for re-authentication")
		}
		
		err = manager.Login(ctx, p.vsphereURL.User)
		if err != nil {
			p.logger.Error("failed to re-login to vSphere", zap.Error(err))
			return fmt.Errorf("vSphere session expired and re-login failed: %w", err)
		}
		
		p.logger.Info("successfully re-authenticated to vSphere")
	}
	
	return nil
}

// checkAndHandleAuthError wraps operations that might fail due to authentication issues.
func (p *Provisioner) checkAndHandleAuthError(ctx context.Context, err error, operation string) error {
	if err == nil {
		return nil
	}
	
	// Check if it's a SOAP fault with NotAuthenticated
	var soapFault *soap.Fault
	if errors.As(err, &soapFault) {
		if _, ok := soapFault.VimFault().(types.NotAuthenticated); ok {
			p.logger.Warn("authentication error detected, attempting to re-authenticate",
				zap.String("operation", operation),
				zap.Error(err))
			
			if authErr := p.ensureAuthenticated(ctx); authErr != nil {
				return fmt.Errorf("%s failed with authentication error and re-auth failed: %w (original: %v)", operation, authErr, err)
			}
			
			// Return a retryable error to trigger retry
			return provision.NewRetryErrorf(time.Second*5, "%s failed due to authentication, retry after re-auth: %w", operation, err)
		}
	}
	
	// Check for authentication error in message
	if isAuthenticationError(err) {
		p.logger.Warn("authentication error detected in error message, attempting to re-authenticate",
			zap.String("operation", operation),
			zap.Error(err))
		
		if authErr := p.ensureAuthenticated(ctx); authErr != nil {
			return fmt.Errorf("%s failed with authentication error and re-auth failed: %w (original: %v)", operation, authErr, err)
		}
		
		// Return a retryable error to trigger retry
		return provision.NewRetryErrorf(time.Second*5, "%s failed due to authentication, retry after re-auth: %w", operation, err)
	}
	
	return err
}

// Provisioner implements Talos emulator infra provider.
type Provisioner struct {
	vsphereClient *govmomi.Client
	logger        *zap.Logger
	vsphereURL    *url.URL
}

// NewProvisioner creates a new provisioner.
func NewProvisioner(vsphereClient *govmomi.Client, logger *zap.Logger, vsphereURL *url.URL) *Provisioner {
	return &Provisioner{
		vsphereClient: vsphereClient,
		logger:        logger,
		vsphereURL:    vsphereURL,
	}
}

// ProvisionSteps implements infra.Provisioner.
//
//nolint:gocognit,gocyclo,cyclop,maintidx
func (p *Provisioner) ProvisionSteps() []provision.Step[*resources.Machine] {
	return []provision.Step[*resources.Machine]{
		provision.NewStep(
			"createVM",
			func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
				// Unmarshal provider-specific configuration
				var data Data
				if err := pctx.UnmarshalProviderData(&data); err != nil {
					return fmt.Errorf("failed to unmarshal provider data: %w", err)
				}

				vmName := pctx.GetRequestID()

				logFields := []zap.Field{
					zap.String("name", vmName),
					zap.String("datacenter", data.Datacenter),
					zap.String("network", data.Network),
					zap.String("datastore", data.Datastore),
					zap.Uint("cpu", data.CPU),
					zap.Uint("memory", data.Memory),
					zap.Uint64("disk_size", data.DiskSize),
				}
				if data.Cluster != "" {
					logFields = append(logFields, zap.String("cluster", data.Cluster))
				}
				if data.ResourcePool != "" {
					logFields = append(logFields, zap.String("resource_pool", data.ResourcePool))
				}
				if data.TemplateFolder != "" {
					logFields = append(logFields, zap.String("template_folder", data.TemplateFolder))
				}
				if data.VMFolder != "" {
					logFields = append(logFields, zap.String("vm_folder", data.VMFolder))
				}
				logger.Info("creating VM", logFields...)

				// Set up the finder with datacenter context
				finder := find.NewFinder(p.vsphereClient.Client, true)

				// Find the datacenter
				dc, err := finder.Datacenter(ctx, data.Datacenter)
				if err != nil {
					err = p.checkAndHandleAuthError(ctx, err, "find datacenter")
					return provision.NewRetryErrorf(time.Second*10, "failed to find datacenter %q: %w", data.Datacenter, err)
				}

				finder.SetDatacenter(dc)

				// Find the folder where VMs will be created
				var folder *object.Folder
				if data.VMFolder != "" {
					folder, err = finder.Folder(ctx, data.VMFolder)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find VM folder %q: %w", data.VMFolder, err)
					}
					logger.Info("using custom VM folder", zap.String("folder", data.VMFolder))
				} else {
					folder, err = finder.DefaultFolder(ctx)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find default VM folder: %w", err)
					}
				}

				// Find the resource pool - support cluster, resource_pool, or both
				var resourcePool *object.ResourcePool
				if data.Cluster != "" && data.ResourcePool != "" {
					// Use specific resource pool within a cluster
					cluster, err := finder.ClusterComputeResource(ctx, data.Cluster)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find cluster %q: %w", data.Cluster, err)
					}
					// Set the cluster as the context for finding the resource pool
					finder.SetDatacenter(dc)
					// Resource pools in a cluster are under the Resources folder
					resourcePoolPath := cluster.InventoryPath + "/Resources/" + data.ResourcePool
					resourcePool, err = finder.ResourcePool(ctx, resourcePoolPath)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find resource pool %q in cluster %q: %w", data.ResourcePool, data.Cluster, err)
					}
					logger.Info("using resource pool in cluster", zap.String("cluster", data.Cluster), zap.String("resource_pool", data.ResourcePool))
				} else if data.Cluster != "" {
					// Use cluster's default resource pool
					cluster, err := finder.ClusterComputeResource(ctx, data.Cluster)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find cluster %q: %w", data.Cluster, err)
					}
					resourcePool, err = cluster.ResourcePool(ctx)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to get default resource pool from cluster %q: %w", data.Cluster, err)
					}
					logger.Info("using cluster default resource pool", zap.String("cluster", data.Cluster))
				} else if data.ResourcePool != "" {
					// Use explicit resource pool path (full path including cluster)
					resourcePool, err = finder.ResourcePool(ctx, data.ResourcePool)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find resource pool %q: %w", data.ResourcePool, err)
					}
					logger.Info("using explicit resource pool path", zap.String("resource_pool", data.ResourcePool))
				} else {
					return provision.NewRetryErrorf(time.Second*10, "either cluster or resource_pool (or both) must be specified")
				}

				// Find the template VM - support template folders
				var template *object.VirtualMachine
				if data.TemplateFolder != "" {
					// Look for template in specific folder
					templatePath := data.TemplateFolder + "/" + data.Template
					template, err = finder.VirtualMachine(ctx, templatePath)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find template %q in folder %q: %w", data.Template, data.TemplateFolder, err)
					}
					logger.Info("found template in folder", zap.String("template", data.Template), zap.String("folder", data.TemplateFolder))
				} else {
					// Look for template in datacenter context
					template, err = finder.VirtualMachine(ctx, data.Template)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to find template %q: %w", data.Template, err)
					}
				}

				// Find the datastore
				datastore, err := finder.Datastore(ctx, data.Datastore)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to find datastore %q: %w", data.Datastore, err)
				}

				// Build clone spec
				resourcePoolRef := resourcePool.Reference()
				datastoreRef := datastore.Reference()

				// Prepare join config userdata
				joinConfigBytes := []byte(pctx.ConnectionParams.JoinConfig)
				joinConfigB64 := base64.StdEncoding.EncodeToString(joinConfigBytes)

				// Find the network
				var network object.NetworkReference
				network, err = finder.Network(ctx, data.Network)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to find network %q: %w", data.Network, err)
				}

				// Get template devices to configure network and disk
				devices, err := template.Device(ctx)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to get template devices: %w", err)
				}

				// Prepare device changes for network and disk
				var deviceChanges []types.BaseVirtualDeviceConfigSpec

				// Configure network adapter
				netCards := devices.SelectByType((*types.VirtualEthernetCard)(nil))
				if len(netCards) > 0 {
					// Modify the first network card to use the specified network
					card := netCards[0].(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()
					card.Backing, err = network.EthernetCardBackingInfo(ctx)
					if err != nil {
						return provision.NewRetryErrorf(time.Second*10, "failed to create network backing: %w", err)
					}
					card.Connectable = &types.VirtualDeviceConnectInfo{
						StartConnected:    true,
						AllowGuestControl: true,
						Connected:         false,
					}
					deviceChanges = append(deviceChanges, &types.VirtualDeviceConfigSpec{
						Operation: types.VirtualDeviceConfigSpecOperationEdit,
						Device:    netCards[0],
					})
				}

				// Configure disk resize if DiskSize is specified
				if data.DiskSize > 0 {
					disks := devices.SelectByType((*types.VirtualDisk)(nil))
					if len(disks) > 0 {
						disk := disks[0].(*types.VirtualDisk)
						// Convert GiB to bytes
						disk.CapacityInBytes = int64(data.DiskSize) * 1024 * 1024 * 1024
						disk.CapacityInKB = int64(data.DiskSize) * 1024 * 1024
						deviceChanges = append(deviceChanges, &types.VirtualDeviceConfigSpec{
							Operation: types.VirtualDeviceConfigSpecOperationEdit,
							Device:    disk,
						})
					}
				}

				// Clone the VM from template
				cloneSpec := types.VirtualMachineCloneSpec{
					Location: types.VirtualMachineRelocateSpec{
						Pool:      &resourcePoolRef,
						Datastore: &datastoreRef,
					},
					Config: &types.VirtualMachineConfigSpec{
						NumCPUs:       int32(data.CPU),
						MemoryMB:      int64(data.Memory),
						DeviceChange:  deviceChanges,
						ExtraConfig: []types.BaseOptionValue{
							&types.OptionValue{Key: "disk.enableUUID", Value: "TRUE"},
							&types.OptionValue{Key: "guestinfo.talos.config", Value: joinConfigB64},
						},
					},
					PowerOn: false,
				}

				task, err := template.Clone(ctx, folder, vmName, cloneSpec)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to clone VM from template: %w", err)
				}

				// Wait for the task to complete
				if err := task.Wait(ctx); err != nil {
					return provision.NewRetryErrorf(time.Second*10, "VM creation task failed: %w", err)
				}

				// Store VM name, datacenter, and UUID in state
				pctx.State.TypedSpec().Value.VmName = vmName
				pctx.State.TypedSpec().Value.Datacenter = data.Datacenter

				return nil
			},
		),
		provision.NewStep(
			"powerOnVM",
			func(ctx context.Context, logger *zap.Logger, pctx provision.Context[*resources.Machine]) error {
				vmName := pctx.State.TypedSpec().Value.VmName
				if vmName == "" {
					return provision.NewRetryErrorf(time.Second*10, "waiting for VM to be created")
				}

				// Unmarshal provider-specific configuration
				var data Data
				if err := pctx.UnmarshalProviderData(&data); err != nil {
					return fmt.Errorf("failed to unmarshal provider data: %w", err)
				}

				finder := find.NewFinder(p.vsphereClient.Client, true)

				// Find the datacenter
				dc, err := finder.Datacenter(ctx, data.Datacenter)
				if err != nil {
					err = p.checkAndHandleAuthError(ctx, err, "find datacenter")
					return provision.NewRetryErrorf(time.Second*10, "failed to find datacenter %q: %w", data.Datacenter, err)
				}

				finder.SetDatacenter(dc)

				// Find the VM
				vm, err := finder.VirtualMachine(ctx, vmName)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to find VM %q: %w", vmName, err)
				}

				// Check power state
				powerState, err := vm.PowerState(ctx)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to get VM power state: %w", err)
				}

				if powerState == types.VirtualMachinePowerStatePoweredOn {
					logger.Info("VM is already powered on", zap.String("name", vmName))

					return nil
				}

				logger.Info("powering on VM", zap.String("name", vmName))

				// Power on the VM
				task, err := vm.PowerOn(ctx)
				if err != nil {
					return provision.NewRetryErrorf(time.Second*10, "failed to power on VM: %w", err)
				}

				if err := task.Wait(ctx); err != nil {
					return provision.NewRetryErrorf(time.Second*10, "power on task failed: %w", err)
				}

				logger.Info("VM powered on successfully", zap.String("name", vmName))

				return nil
			},
		),
	}
}

// Deprovision implements infra.Provisioner.
func (p *Provisioner) Deprovision(ctx context.Context, logger *zap.Logger, machine *resources.Machine, machineRequest *infra.MachineRequest) error {
	vmName := machineRequest.Metadata().ID()

	if vmName == "" {
		return fmt.Errorf("empty vmName")
	}

	logger.Info("deprovisioning VM", zap.String("name", vmName))

	// Get datacenter from machine state
	datacenter := machine.TypedSpec().Value.Datacenter
	if datacenter == "" {
		// If there is no datacenter info, it could mean that machine
		// provisioning failed early and we shouldn't attempt to
		// remove the machine from vsphere.
		logger.Info("datacenter not found in machine state")

		return nil
	}

	finder := find.NewFinder(p.vsphereClient.Client, true)

	// Find the datacenter
	dc, err := finder.Datacenter(ctx, datacenter)
	if err != nil {
		err = p.checkAndHandleAuthError(ctx, err, "find datacenter")
		return fmt.Errorf("failed to find datacenter %q: %w", datacenter, err)
	}

	finder.SetDatacenter(dc)

	// Find the VM
	vm, err := finder.VirtualMachine(ctx, vmName)
	if err != nil {
		// Only ignore "not found" errors - VM already deleted
		var notFoundErr *find.NotFoundError
		if errors.As(err, &notFoundErr) {
			logger.Info("VM not found, already removed", zap.String("name", vmName))

			return nil
		}

		return fmt.Errorf("failed to find VM %q: %w", vmName, err)
	}

	logger.Info("found VM", zap.String("name", vmName))

	// Check power state and power off if needed
	powerState, err := vm.PowerState(ctx)
	if err != nil {
		return fmt.Errorf("failed to get VM power state: %w", err)
	}

	if powerState == types.VirtualMachinePowerStatePoweredOn {
		logger.Info("powering off VM", zap.String("name", vmName))

		var task *object.Task

		task, err = vm.PowerOff(ctx)
		if err != nil {
			return fmt.Errorf("failed to power off VM: %w", err)
		}

		if err = task.Wait(ctx); err != nil {
			return fmt.Errorf("power off task failed: %w", err)
		}

		logger.Info("VM powered off", zap.String("name", vmName))
	}

	// Destroy (delete) the VM
	logger.Info("destroying VM", zap.String("name", vmName))

	task, err := vm.Destroy(ctx)
	if err != nil {
		return fmt.Errorf("failed to destroy VM: %w", err)
	}

	if err = task.Wait(ctx); err != nil {
		return fmt.Errorf("destroy task failed: %w", err)
	}

	logger.Info("VM destroyed successfully", zap.String("name", vmName))

	return nil
}
