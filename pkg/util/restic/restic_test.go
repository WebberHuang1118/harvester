package restic

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/harvester/harvester/pkg/settings"
)

func resetResticSettings(t *testing.T) {
	t.Helper()
	require.NoError(t, settings.ResticCacheSize.Set(settings.ResticCacheSize.GetDefault()))
	require.NoError(t, settings.ResticNoCache.Set(settings.ResticNoCache.GetDefault()))
	require.NoError(t, settings.ResticMaxConcurrentJobs.Set(settings.ResticMaxConcurrentJobs.GetDefault()))
	require.NoError(t, settings.ResticEphemeralRequest.Set(settings.ResticEphemeralRequest.GetDefault()))
	require.NoError(t, settings.ResticEphemeralLimit.Set(settings.ResticEphemeralLimit.GetDefault()))
	t.Cleanup(func() {
		_ = settings.ResticCacheSize.Set(settings.ResticCacheSize.GetDefault())
		_ = settings.ResticNoCache.Set(settings.ResticNoCache.GetDefault())
		_ = settings.ResticMaxConcurrentJobs.Set(settings.ResticMaxConcurrentJobs.GetDefault())
		_ = settings.ResticEphemeralRequest.Set(settings.ResticEphemeralRequest.GetDefault())
		_ = settings.ResticEphemeralLimit.Set(settings.ResticEphemeralLimit.GetDefault())
	})
}

func TestCacheVolumeAndMount(t *testing.T) {
	resetResticSettings(t)
	require.NoError(t, settings.ResticCacheSize.Set("2Gi"))

	volume, mount, err := CacheVolumeAndMount()
	require.NoError(t, err)
	require.NotNil(t, volume)
	require.NotNil(t, mount)
	require.Equal(t, CacheVolume, volume.Name)
	require.NotNil(t, volume.EmptyDir)
	require.Equal(t, resource.MustParse("2Gi"), *volume.EmptyDir.SizeLimit)
	require.Equal(t, CacheVolume, mount.Name)
	require.Equal(t, CacheDir, mount.MountPath)
}

func TestCacheVolumeAndMountDisabledByNoCache(t *testing.T) {
	resetResticSettings(t)
	require.NoError(t, settings.ResticNoCache.Set("true"))

	volume, mount, err := CacheVolumeAndMount()
	require.NoError(t, err)
	require.Nil(t, volume)
	require.Nil(t, mount)
	require.Equal(t, "restic --no-cache", Command())
}

func TestJobHasCapacityReturnsFalseWhenGlobalLimitIsReached(t *testing.T) {
	resetResticSettings(t)
	require.NoError(t, settings.ResticMaxConcurrentJobs.Set("3"))
	clientset := fake.NewSimpleClientset(
		resticPod("pod1", "node1", corev1.PodRunning),
		resticPod("pod2", "node1", corev1.PodRunning),
		resticPod("pod3", "node2", corev1.PodRunning),
	)

	hasCapacity, err := JobHasCapacity(context.Background(), clientset)
	require.NoError(t, err)
	require.False(t, hasCapacity)
}

func TestJobHasCapacityReturnsTrueBelowGlobalLimit(t *testing.T) {
	resetResticSettings(t)
	require.NoError(t, settings.ResticMaxConcurrentJobs.Set("3"))
	clientset := fake.NewSimpleClientset(
		resticPod("pod1", "node1", corev1.PodRunning),
		resticPod("pod2", "node1", corev1.PodSucceeded),
	)

	hasCapacity, err := JobHasCapacity(context.Background(), clientset)
	require.NoError(t, err)
	require.True(t, hasCapacity)
}

func resticPod(name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				LabelResticJob: LabelValueTrue,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
		},
		Status: corev1.PodStatus{
			Phase: phase,
		},
	}
}
