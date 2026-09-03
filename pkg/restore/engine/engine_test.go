package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/harvester/harvester/pkg/generated/clientset/versioned/fake"
	"github.com/harvester/harvester/pkg/util/fakeclients"
)

func TestReleaseRestoreJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "restore",
			Namespace:  "default",
			Finalizers: []string{"example.io/other", RestoreJobCompletionFinalizer},
		},
	}
	clientset := fake.NewSimpleClientset(job)
	jobClient := fakeclients.JobClient(clientset.BatchV1().Jobs)

	require.NoError(t, ReleaseRestoreJob(jobClient, job.Namespace, job.Name))

	updated, err := clientset.BatchV1().Jobs(job.Namespace).Get(
		context.Background(),
		job.Name,
		metav1.GetOptions{},
	)
	require.NoError(t, err)
	require.Equal(t, []string{"example.io/other"}, updated.Finalizers)
}

func TestDeleteRestoreJob(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "restore",
			Namespace:  "default",
			Finalizers: []string{RestoreJobCompletionFinalizer},
		},
	}
	clientset := fake.NewSimpleClientset(job)
	jobClient := fakeclients.JobClient(clientset.BatchV1().Jobs)

	require.NoError(t, DeleteRestoreJob(jobClient, job.Namespace, job.Name))
	_, err := clientset.BatchV1().Jobs(job.Namespace).Get(
		context.Background(),
		job.Name,
		metav1.GetOptions{},
	)
	require.Error(t, err)
}
