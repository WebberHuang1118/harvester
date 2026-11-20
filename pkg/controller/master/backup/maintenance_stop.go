package backup

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/harvester/harvester/pkg/util"
	kopiautil "github.com/harvester/harvester/pkg/util/kopia"
)

// stopMaintenance waits for active consumers as well as removing schedules.
// Existing Pods have already captured the old shared credentials.
func (h *TargetHandler) stopMaintenance() error {
	if err := h.deleteKopiaMaintenance(); err != nil {
		return err
	}
	for _, schedule := range kopiaMaintenanceSchedules {
		_, err := h.clientset.BatchV1().CronJobs(util.LonghornSystemNamespaceName).Get(h.ctx, schedule.name, metav1.GetOptions{})
		if err == nil {
			return fmt.Errorf("waiting for Kopia maintenance schedule removal")
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
	}
	waiting, err := h.stopMaintenanceJobs()
	if err != nil {
		return err
	}
	pods, err := h.clientset.CoreV1().Pods(util.LonghornSystemNamespaceName).List(h.ctx, metav1.ListOptions{
		LabelSelector: kopiautil.LabelKopiaMaintenance + "=" + kopiautil.LabelValueTrue,
	})
	if err != nil {
		return err
	}
	if waiting || len(pods.Items) != 0 {
		return fmt.Errorf("waiting for Kopia maintenance to stop")
	}
	return nil
}

func (h *TargetHandler) stopMaintenanceJobs() (bool, error) {
	jobs := h.clientset.BatchV1().Jobs(util.LonghornSystemNamespaceName)
	list, err := jobs.List(h.ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	waiting := false
	for _, job := range list.Items {
		owner := metav1.GetControllerOf(&job)
		if owner == nil || owner.Kind != "CronJob" || !maintenanceName(owner.Name) {
			continue
		}
		if err := jobs.Delete(h.ctx, job.Name, metav1.DeleteOptions{
			PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
			Preconditions:     &metav1.Preconditions{UID: &job.UID},
		}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		waiting = true
	}
	return waiting, nil
}

func maintenanceName(name string) bool {
	return name == kopiautil.QuickMaintenanceCronJobName || name == kopiautil.FullMaintenanceCronJobName
}
