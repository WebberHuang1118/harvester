package restic

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
)

const (
	ImageEnvVar = "HARVESTER_IMAGE"

	PasswordKey = "RESTIC_PASSWORD"

	CacheDirEnvVar = "RESTIC_CACHE_DIR"
	CacheDir       = "/restic-cache"
	CacheVolume    = "restic-cache"

	// repoSubpath isolates restic data under a dedicated prefix inside the
	// shared backup bucket so it doesn't collide with Longhorn / Kopia data.
	repoSubpath = "restic"

	// Labels stamped on Jobs we create so engine-side Job watchers can map
	// a Job event back to the owning VMBackup / VMRestore and enqueue it.
	LabelVMBackupNamespace  = "harvesterhci.io/vm-backup-namespace"
	LabelVMBackupName       = "harvesterhci.io/vm-backup-name"
	LabelVMRestoreNamespace = "harvesterhci.io/vm-restore-namespace"
	LabelVMRestoreName      = "harvesterhci.io/vm-restore-name"
	LabelResticJob          = "harvesterhci.io/restic-job"
	LabelValueTrue          = "true"

	SnapshotCheckContainerName   = "check"
	SnapshotCheckMissingExitCode = 42
)

type SnapshotCheckResult string

const (
	SnapshotCheckPending SnapshotCheckResult = "pending"
	SnapshotCheckFound   SnapshotCheckResult = "found"
	SnapshotCheckMissing SnapshotCheckResult = "missing"
	SnapshotCheckFailed  SnapshotCheckResult = "failed"
)

type JobRuntime struct {
	Image        string
	Labels       map[string]string
	Env          []corev1.EnvVar
	Resources    corev1.ResourceRequirements
	VolumeMounts []corev1.VolumeMount
	Volumes      []corev1.Volume
	Command      string
}

type SnapshotCheckJobOptions struct {
	Name            string
	Namespace       string
	Labels          map[string]string
	OwnerReferences []metav1.OwnerReference
	Runtime         *JobRuntime
	Tags            []string
}

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

func RepositoryFromSetting() (string, error) {
	target, err := settings.DecodeBackupTarget(settings.BackupTargetSet.Get())
	if err != nil {
		return "", err
	}
	return Repository(target)
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
	env := []corev1.EnvVar{
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
	if !NoCache() {
		env = append(env, corev1.EnvVar{Name: CacheDirEnvVar, Value: CacheDir})
	}
	return env
}

func Command() string {
	if NoCache() {
		return "restic --no-cache"
	}
	return "restic"
}

func NoCache() bool {
	enabled, err := strconv.ParseBool(settings.ResticNoCache.Get())
	if err == nil {
		return enabled
	}
	enabled, _ = strconv.ParseBool(settings.ResticNoCache.GetDefault())
	return enabled
}

func JobLabels(labels map[string]string) map[string]string {
	result := map[string]string{
		LabelResticJob: LabelValueTrue,
	}
	for k, v := range labels {
		result[k] = v
	}
	return result
}

func NewJobRuntime(
	ctx context.Context,
	clientset kubernetes.Interface,
	secretName, repository string,
	labels map[string]string,
) (*JobRuntime, bool, error) {
	image, err := Image()
	if err != nil {
		return nil, false, err
	}
	hasCapacity, err := JobHasCapacity(ctx, clientset)
	if err != nil {
		return nil, false, err
	}
	if !hasCapacity {
		return nil, false, nil
	}
	cacheVolume, cacheMount, err := CacheVolumeAndMount()
	if err != nil {
		return nil, false, err
	}
	resources, err := JobResources()
	if err != nil {
		return nil, false, err
	}

	runtime := &JobRuntime{
		Image:     image,
		Labels:    labels,
		Env:       Env(secretName, repository),
		Resources: resources,
		Command:   Command(),
	}
	if cacheVolume != nil {
		runtime.Volumes = append(runtime.Volumes, *cacheVolume)
	}
	if cacheMount != nil {
		runtime.VolumeMounts = append(runtime.VolumeMounts, *cacheMount)
	}
	return runtime, true, nil
}

func CheckSnapshotJob(
	ctx context.Context,
	clientset kubernetes.Interface,
	jobName string,
	getJob func(name string) (*batchv1.Job, error),
	createJob func(name string) error,
) (SnapshotCheckResult, error) {
	job, err := getJob(jobName)
	if err == nil {
		return SnapshotCheckJobResult(ctx, clientset, job)
	}
	if !apierrors.IsNotFound(err) {
		return SnapshotCheckPending, err
	}
	if err := createJob(jobName); err != nil {
		return SnapshotCheckPending, err
	}
	return SnapshotCheckPending, nil
}

func SnapshotCheckJobResult(ctx context.Context, clientset kubernetes.Interface, job *batchv1.Job) (SnapshotCheckResult, error) {
	if job.Status.Failed > 0 {
		return failedSnapshotCheckResult(ctx, clientset, job)
	}
	if job.Status.Succeeded == 0 {
		return SnapshotCheckPending, nil
	}
	return SnapshotCheckFound, nil
}

func failedSnapshotCheckResult(ctx context.Context, clientset kubernetes.Interface, job *batchv1.Job) (SnapshotCheckResult, error) {
	exitCode, foundExitCode, err := SnapshotCheckJobExitCode(ctx, clientset, job)
	if err != nil {
		return SnapshotCheckPending, err
	}
	if foundExitCode && exitCode == SnapshotCheckMissingExitCode {
		return SnapshotCheckMissing, nil
	}
	return SnapshotCheckFailed, nil
}

func SnapshotCheckJobExitCode(ctx context.Context, clientset kubernetes.Interface, job *batchv1.Job) (int32, bool, error) {
	pods, err := clientset.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", job.Name),
	})
	if err != nil {
		return 0, false, fmt.Errorf("listing pods for restic snapshot check job %s/%s: %w", job.Namespace, job.Name, err)
	}
	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != SnapshotCheckContainerName || status.State.Terminated == nil {
				continue
			}
			return status.State.Terminated.ExitCode, true, nil
		}
	}
	return 0, false, nil
}

func NewSnapshotCheckJob(opts SnapshotCheckJobOptions) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            opts.Name,
			Namespace:       opts.Namespace,
			Labels:          opts.Labels,
			OwnerReferences: opts.OwnerReferences,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			TTLSecondsAfterFinished: ptr.To[int32](300),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: opts.Labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:         SnapshotCheckContainerName,
						Image:        opts.Runtime.Image,
						Env:          opts.Runtime.Env,
						Command:      []string{"/bin/sh", "-c"},
						Resources:    opts.Runtime.Resources,
						VolumeMounts: opts.Runtime.VolumeMounts,
						Args:         []string{SnapshotCheckCommand(opts.Runtime.Command, opts.Tags)},
					}},
					Volumes: opts.Runtime.Volumes,
				},
			},
		},
	}
}

func SnapshotCheckCommand(resticCommand string, tags []string) string {
	return fmt.Sprintf(
		"set -eo pipefail; "+
			"SNAPSHOTS=$(%s snapshots --json --tag=%s | tr -d '[:space:]'); "+
			"test \"$SNAPSHOTS\" != \"[]\" || exit %d",
		resticCommand,
		strings.Join(tags, ","),
		SnapshotCheckMissingExitCode,
	)
}

func (r *JobRuntime) VolumesWith(volumes ...corev1.Volume) []corev1.Volume {
	result := append([]corev1.Volume{}, r.Volumes...)
	return append(result, volumes...)
}

func CacheVolumeAndMount() (*corev1.Volume, *corev1.VolumeMount, error) {
	if NoCache() {
		return nil, nil, nil
	}
	sizeLimit, err := resource.ParseQuantity(settings.ResticCacheSize.Get())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse setting %s=%q: %w",
			settings.ResticCacheSizeSettingName, settings.ResticCacheSize.Get(), err)
	}
	return &corev1.Volume{
			Name: CacheVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					SizeLimit: &sizeLimit,
				},
			},
		}, &corev1.VolumeMount{
			Name:      CacheVolume,
			MountPath: CacheDir,
		}, nil
}

func JobResources() (corev1.ResourceRequirements, error) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	if err := addQuantity(resources.Requests, corev1.ResourceEphemeralStorage, settings.ResticEphemeralRequest); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if err := addQuantity(resources.Limits, corev1.ResourceEphemeralStorage, settings.ResticEphemeralLimit); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	if len(resources.Requests) == 0 {
		resources.Requests = nil
	}
	if len(resources.Limits) == 0 {
		resources.Limits = nil
	}
	return resources, nil
}

func addQuantity(list corev1.ResourceList, name corev1.ResourceName, setting settings.Setting) error {
	value := setting.Get()
	if value == "" {
		return nil
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return fmt.Errorf("failed to parse setting %s=%q: %w", setting.Name, value, err)
	}
	list[name] = quantity
	return nil
}

func JobHasCapacity(ctx context.Context, clientset kubernetes.Interface) (bool, error) {
	maxConcurrent := settings.ResticMaxConcurrentJobs.GetInt()
	if maxConcurrent <= 0 {
		return true, nil
	}
	if clientset == nil {
		return false, fmt.Errorf("clientset is nil")
	}

	pods, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", LabelResticJob, LabelValueTrue),
	})
	if err != nil {
		return false, fmt.Errorf("listing restic pods for scheduling: %w", err)
	}

	activeTotal := 0
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		activeTotal++
	}
	if activeTotal >= maxConcurrent {
		return false, nil
	}
	return true, nil
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
