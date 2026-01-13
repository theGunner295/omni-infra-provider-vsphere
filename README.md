# Omni Infrastructure Provider for vSphere

Can be used to automatically provision Talos nodes in `vSphere`.

## Running Infrastructure Provider

Create the configuration file for the provider:

```yaml
vsphere:
  uri: https://<vsphere IP or dns name>/sdk
  user: <vsphere user>
  password: <vsphere pass>
  insecureSkipVerify: true
```

### Using Docker

Copy the provider credentials created in omni to an `.env` file.

```env
# your omni instance URL
OMNI_ENDPOINT=https://<OMNI_INSTANCE_NAME>.<REGION>.omni.siderolabs.io
# base64 encoded key as shown by omni
OMNI_SERVICE_ACCOUNT_KEY=<PROVIDER_KEY>
```

Run in docker with:

```bash
docker run --name omni-infra-provider-vsphere --rm -it -e USER=user --env-file /tmp/omni-provider-vsphere.env -v /tmp/omni-provider-vsphere.yaml:/config.yaml ghcr.io/siderolabs/omni-infra-provider-vsphere --config-file /config.yaml
```

## Prerequisites to Use

Before using the vSphere provider to create machines, you will need to import an OVA as a template for the provider to clone from.
This should be generated from [https://factory.talos.dev](https://factory.talos.dev).
Select the VMWare image and add the vmtoolsd system extension (and any other desired extensions).
You **should not** add in kernel args to do joining to Omni.
These will be set by the provider in the talos.config guestinfo section when creating the VM.
In the future, the provider will may support seeding the environment with this image.

## Use

See [test/](./test/) for some examples, but generally:

- Create a machine class with `omnictl apply -f machineclass.yaml`
- Create a cluster that uses the machine class with `omnictl cluster template sync -f cluster-template.yaml`

### Configuration Options

The provider supports several configuration options for organizing and managing vSphere resources:

**Cluster and Resource Pool:**
- **`cluster`**: Specify a vSphere cluster name (uses default resource pool)
- **`resource_pool`**: Specify a resource pool path
- **Both together**: Use `cluster` + `resource_pool` to reference a pool within a cluster

Examples:
```yaml
# Simple: Use cluster's default resource pool
cluster: "Cluster01"

# Within cluster: Use specific pool in cluster
cluster: "Cluster01"
resource_pool: "Resources/Production"

# Full path: Specify complete path
resource_pool: "Cluster01/Resources/Production"
```

**Template Organization:**
- **`template_folder`**: Organize templates in folders (e.g., `"Templates/Talos"`)

**VM Organization:**
- **`vm_folder`**: Create VMs in specific folders (e.g., `"VMs/Production/K8s"`)

See [test/machineclass.yaml](./test/machineclass.yaml) for a complete example.

## Development

See `make help` for general build info.

Build an image:

```shell
make generate image-omni-infra-provider-vsphere-linux-amd64
```
