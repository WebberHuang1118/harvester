package pvchelper

import (
	"fmt"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/harvester/harvester/pkg/restore/engine"
)

const (
	volumeSnapshotKind = "VolumeSnapshot"
)

// BuildPVCFromSnapshot creates a PVC spec from a VolumeSnapshot
func BuildPVCFromSnapshot(
	namespace string,
	pvcName string,
	vsName string,
	labels map[string]string,
	annotations map[string]string,
	pvcSpec corev1.PersistentVolumeClaimSpec,
) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: pvcSpec.AccessModes,
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: ptr.To(snapshotv1.SchemeGroupVersion.Group),
				Kind:     volumeSnapshotKind,
				Name:     vsName,
			},
			Resources:        pvcSpec.Resources,
			StorageClassName: pvcSpec.StorageClassName,
			VolumeMode:       pvcSpec.VolumeMode,
		},
	}
}

// CheckPVCStatus validates the PVC status and returns appropriate errors
func CheckPVCStatus(pvc *corev1.PersistentVolumeClaim) error {
	if pvc.Status.Phase == corev1.ClaimPending {
		return engine.ErrRetryLater
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		return fmt.Errorf("PVC %s/%s in status %q", pvc.Namespace, pvc.Name, pvc.Status.Phase)
	}

	return nil
}

// BuildRestoreAnnotations creates annotations map for restored PVC, filtering out unwanted annotations
func BuildRestoreAnnotations(sourceAnnotations map[string]string, restoreName string, restoreAnnotationKey string) map[string]string {
	annotations := make(map[string]string)

	for key, value := range sourceAnnotations {
		if !ShouldSkipAnnotation(key) {
			annotations[key] = value
		}
	}

	annotations[restoreAnnotationKey] = restoreName
	return annotations
}

// ShouldSkipAnnotation checks if an annotation should be filtered out
func ShouldSkipAnnotation(key string) bool {
	skipPrefixes := []string{"pv.kubernetes.io"}
	for _, prefix := range skipPrefixes {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
