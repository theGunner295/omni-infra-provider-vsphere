// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package provider

// Data is the provider custom machine config.
type Data struct {
	Datacenter     string `yaml:"datacenter"`
	Cluster        string `yaml:"cluster"`         // Cluster name (alternative to resource_pool)
	ResourcePool   string `yaml:"resource_pool"`   // Resource pool path (for advanced use)
	Datastore      string `yaml:"datastore"`
	Network        string `yaml:"network"`
	Template       string `yaml:"template"`         // VM template name to clone from
	TemplateFolder string `yaml:"template_folder"` // Folder path where template is located
	VMFolder       string `yaml:"vm_folder"`       // Folder path where VMs should be created
	DiskSize       uint64 `yaml:"disk_size"`       // GiB
	CPU            uint   `yaml:"cpu"`
	Memory         uint   `yaml:"memory"`          // MiB
}
