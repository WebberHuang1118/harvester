package restic

import (
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
)

const (
	ImageEnvVar = "HARVESTER_IMAGE"

	PasswordKey = "RESTIC_PASSWORD"

	// repoSubpath isolates restic data under a dedicated prefix inside the
	// shared backup bucket so it doesn't collide with Longhorn / Kopia data.
	repoSubpath = "restic"

	// Labels stamped on Jobs we create so engine-side Job watchers can map
	// a Job event back to the owning VMBackup / VMRestore and enqueue it.
	LabelVMBackupNamespace  = "harvesterhci.io/vm-backup-namespace"
	LabelVMBackupName       = "harvesterhci.io/vm-backup-name"
	LabelVMRestoreNamespace = "harvesterhci.io/vm-restore-namespace"
	LabelVMRestoreName      = "harvesterhci.io/vm-restore-name"
)

// Image returns the harvester container image used for restic backup/restore
// jobs. The image must be injected via HARVESTER_IMAGE (set on the apiserver
// Deployment) so jobs run the same build as the controller. We refuse to fall
// back to a hardcoded default — a stale default is easy to ship by accident.
func Image() (string, error) {
	image := os.Getenv(ImageEnvVar)
	if image == "" {
		return "", fmt.Errorf("%s environment variable is not set", ImageEnvVar)
	}
	return image, nil
}

func Repository(target *settings.BackupTarget) (string, error) {
	if target == nil {
		return "", fmt.Errorf("backup target is nil")
	}

	switch target.Type {
	case settings.S3BackupType:
		endpoint := strings.TrimRight(target.Endpoint, "/")
		if endpoint == "" {
			return "", fmt.Errorf("s3 backup target endpoint is empty")
		}
		if target.BucketName == "" {
			return "", fmt.Errorf("s3 backup target bucket name is empty")
		}
		return fmt.Sprintf("s3:%s/%s/%s", endpoint, target.BucketName, repoSubpath), nil
	default:
		return "", fmt.Errorf("restic engine currently supports %s backup targets only, got %s", settings.S3BackupType, target.Type)
	}
}

func Env(secretName, repository string) []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			Name: util.AWSAccessKey,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  util.AWSAccessKey,
			}},
		},
		{
			Name: util.AWSSecretKey,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  util.AWSSecretKey,
			}},
		},
		{Name: "RESTIC_REPOSITORY", Value: repository},
		{
			Name: "RESTIC_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  PasswordKey,
			}},
		},
	}
}

func SnapshotTag(backupName string) string {
	return fmt.Sprintf("sn=%s", backupName)
}

func NamespaceTag(namespace string) string {
	return fmt.Sprintf("ns=%s", namespace)
}

func VMBackupTag(vmBackupName string) string {
	return fmt.Sprintf("vmb=%s", vmBackupName)
}
