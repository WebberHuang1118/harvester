package kopia

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

	PasswordKey = "KOPIA_PASSWORD"
	BucketKey   = "KOPIA_BUCKET"
	EndpointKey = "KOPIA_ENDPOINT"
	ConfigPath  = "/tmp/kopia.config"
)

// Image returns the harvester container image used for kopia backup/restore
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

type Repository struct {
	Bucket   string
	Endpoint string
}

func S3Repository(target *settings.BackupTarget) (*Repository, error) {
	if target == nil {
		return nil, fmt.Errorf("backup target is nil")
	}

	switch target.Type {
	case settings.S3BackupType:
		endpoint := strings.TrimRight(target.Endpoint, "/")
		if endpoint == "" {
			return nil, fmt.Errorf("s3 backup target endpoint is empty")
		}
		if target.BucketName == "" {
			return nil, fmt.Errorf("s3 backup target bucket name is empty")
		}
		return &Repository{
			Bucket:   target.BucketName,
			Endpoint: endpoint,
		}, nil
	default:
		return nil, fmt.Errorf("kopia engine currently supports %s backup targets only, got %s", settings.S3BackupType, target.Type)
	}
}

func Env(secretName string, repository *Repository) []corev1.EnvVar {
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
		{
			Name: PasswordKey,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  PasswordKey,
			}},
		},
		{Name: BucketKey, Value: repository.Bucket},
		{Name: EndpointKey, Value: repository.Endpoint},
	}
}

func ConnectOrCreateCommand() string {
	return fmt.Sprintf(
		"kopia repository connect s3 --config-file=%[1]s --bucket=\"$%[2]s\" --endpoint=\"$%[3]s\" --access-key=\"$%[4]s\" --secret-access-key=\"$%[5]s\" || "+
			"kopia repository create s3 --config-file=%[1]s --bucket=\"$%[2]s\" --endpoint=\"$%[3]s\" --access-key=\"$%[4]s\" --secret-access-key=\"$%[5]s\"",
		ConfigPath,
		BucketKey,
		EndpointKey,
		util.AWSAccessKey,
		util.AWSSecretKey,
	)
}

func ConnectCommand() string {
	return fmt.Sprintf(
		"kopia repository connect s3 --config-file=%[1]s --bucket=\"$%[2]s\" --endpoint=\"$%[3]s\" --access-key=\"$%[4]s\" --secret-access-key=\"$%[5]s\"",
		ConfigPath,
		BucketKey,
		EndpointKey,
		util.AWSAccessKey,
		util.AWSSecretKey,
	)
}

func SnapshotTag(backupName string) string {
	return fmt.Sprintf("sn:%s", backupName)
}

func NamespaceTag(namespace string) string {
	return fmt.Sprintf("ns:%s", namespace)
}
