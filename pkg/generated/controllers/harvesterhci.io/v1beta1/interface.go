/*
Copyright 2024 Rancher Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by main. DO NOT EDIT.

package v1beta1

import (
	v1beta1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/rancher/lasso/pkg/controller"
	"github.com/rancher/wrangler/v3/pkg/generic"
	"github.com/rancher/wrangler/v3/pkg/schemes"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func init() {
	schemes.Register(v1beta1.AddToScheme)
}

type Interface interface {
	Addon() AddonController
	KeyPair() KeyPairController
	Preference() PreferenceController
	ResourceQuota() ResourceQuotaController
	ScheduleVMBackup() ScheduleVMBackupController
	Setting() SettingController
	SupportBundle() SupportBundleController
	Upgrade() UpgradeController
	UpgradeLog() UpgradeLogController
	Version() VersionController
	VirtualMachineBackup() VirtualMachineBackupController
	VirtualMachineImage() VirtualMachineImageController
	VirtualMachineRestore() VirtualMachineRestoreController
	VirtualMachineTemplate() VirtualMachineTemplateController
	VirtualMachineTemplateVersion() VirtualMachineTemplateVersionController
}

func New(controllerFactory controller.SharedControllerFactory) Interface {
	return &version{
		controllerFactory: controllerFactory,
	}
}

type version struct {
	controllerFactory controller.SharedControllerFactory
}

func (v *version) Addon() AddonController {
	return generic.NewController[*v1beta1.Addon, *v1beta1.AddonList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "Addon"}, "addons", true, v.controllerFactory)
}

func (v *version) KeyPair() KeyPairController {
	return generic.NewController[*v1beta1.KeyPair, *v1beta1.KeyPairList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "KeyPair"}, "keypairs", true, v.controllerFactory)
}

func (v *version) Preference() PreferenceController {
	return generic.NewController[*v1beta1.Preference, *v1beta1.PreferenceList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "Preference"}, "preferences", true, v.controllerFactory)
}

func (v *version) ResourceQuota() ResourceQuotaController {
	return generic.NewController[*v1beta1.ResourceQuota, *v1beta1.ResourceQuotaList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "ResourceQuota"}, "resourcequotas", true, v.controllerFactory)
}

func (v *version) ScheduleVMBackup() ScheduleVMBackupController {
	return generic.NewController[*v1beta1.ScheduleVMBackup, *v1beta1.ScheduleVMBackupList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "ScheduleVMBackup"}, "schedulevmbackups", true, v.controllerFactory)
}

func (v *version) Setting() SettingController {
	return generic.NewNonNamespacedController[*v1beta1.Setting, *v1beta1.SettingList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "Setting"}, "settings", v.controllerFactory)
}

func (v *version) SupportBundle() SupportBundleController {
	return generic.NewController[*v1beta1.SupportBundle, *v1beta1.SupportBundleList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "SupportBundle"}, "supportbundles", true, v.controllerFactory)
}

func (v *version) Upgrade() UpgradeController {
	return generic.NewController[*v1beta1.Upgrade, *v1beta1.UpgradeList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "Upgrade"}, "upgrades", true, v.controllerFactory)
}

func (v *version) UpgradeLog() UpgradeLogController {
	return generic.NewController[*v1beta1.UpgradeLog, *v1beta1.UpgradeLogList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "UpgradeLog"}, "upgradelogs", true, v.controllerFactory)
}

func (v *version) Version() VersionController {
	return generic.NewController[*v1beta1.Version, *v1beta1.VersionList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "Version"}, "versions", true, v.controllerFactory)
}

func (v *version) VirtualMachineBackup() VirtualMachineBackupController {
	return generic.NewController[*v1beta1.VirtualMachineBackup, *v1beta1.VirtualMachineBackupList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "VirtualMachineBackup"}, "virtualmachinebackups", true, v.controllerFactory)
}

func (v *version) VirtualMachineImage() VirtualMachineImageController {
	return generic.NewController[*v1beta1.VirtualMachineImage, *v1beta1.VirtualMachineImageList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "VirtualMachineImage"}, "virtualmachineimages", true, v.controllerFactory)
}

func (v *version) VirtualMachineRestore() VirtualMachineRestoreController {
	return generic.NewController[*v1beta1.VirtualMachineRestore, *v1beta1.VirtualMachineRestoreList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "VirtualMachineRestore"}, "virtualmachinerestores", true, v.controllerFactory)
}

func (v *version) VirtualMachineTemplate() VirtualMachineTemplateController {
	return generic.NewController[*v1beta1.VirtualMachineTemplate, *v1beta1.VirtualMachineTemplateList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "VirtualMachineTemplate"}, "virtualmachinetemplates", true, v.controllerFactory)
}

func (v *version) VirtualMachineTemplateVersion() VirtualMachineTemplateVersionController {
	return generic.NewController[*v1beta1.VirtualMachineTemplateVersion, *v1beta1.VirtualMachineTemplateVersionList](schema.GroupVersionKind{Group: "harvesterhci.io", Version: "v1beta1", Kind: "VirtualMachineTemplateVersion"}, "virtualmachinetemplateversions", true, v.controllerFactory)
}
