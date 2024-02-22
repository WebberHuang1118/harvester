package util

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/settings"
)

const (
	AnnStorageProvisioner     = "volume.kubernetes.io/storage-provisioner"
	AnnBetaStorageProvisioner = "volume.beta.kubernetes.io/storage-provisioner"
	LonghornDataLocality      = "dataLocality"

	LonghornEncrypted                  = "encrypted"
	LonghornNodePublishSecretName      = "csi.storage.k8s.io/node-publish-secret-name"
	LonghornNodePublishSecretNameSpace = "csi.storage.k8s.io/node-publish-secret-namespace"
	LonghornNodeStageSecretName        = "csi.storage.k8s.io/node-stage-secret-name"
	LonghornNodeStageSecretNameSpace   = "csi.storage.k8s.io/node-stage-secret-namespace"
	LonghornProvisionerSecretName      = "csi.storage.k8s.io/provisioner-secret-name"
	LonghornProvisionerSecretNameSpace = "csi.storage.k8s.io/provisioner-secret-namespace"
)

var (
	PersistentVolumeClaimsKind = schema.GroupVersionKind{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"}
)

// GetProvisionedPVCProvisioner do not use this function when the PVC is just created
func GetProvisionedPVCProvisioner(pvc *corev1.PersistentVolumeClaim) string {
	provisioner, ok := pvc.Annotations[AnnBetaStorageProvisioner]
	if !ok {
		provisioner = pvc.Annotations[AnnStorageProvisioner]
	}
	return provisioner
}

// LoadCSIDriverConfig loads the CSI driver configuration from settings.
func LoadCSIDriverConfig(settingCache ctlharvesterv1.SettingCache) (map[string]settings.CSIDriverInfo, error) {
	csiDriverConfigSetting, err := settingCache.Get(settings.CSIDriverConfigSettingName)
	if err != nil {
		return nil, fmt.Errorf("can't get %s setting, err: %w", settings.CSIDriverConfigSettingName, err)
	}
	csiDriverConfigSettingValue := csiDriverConfigSetting.Default
	if csiDriverConfigSetting.Value != "" {
		csiDriverConfigSettingValue = csiDriverConfigSetting.Value
	}
	csiDriverConfig := make(map[string]settings.CSIDriverInfo)
	if err := json.Unmarshal([]byte(csiDriverConfigSettingValue), &csiDriverConfig); err != nil {
		return nil, fmt.Errorf("can't parse %s setting, err: %w", settings.CSIDriverConfigSettingName, err)
	}
	return csiDriverConfig, nil
}
