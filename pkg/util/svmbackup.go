package util

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	ctlharvesterv1 "github.com/harvester/harvester/pkg/generated/controllers/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/ref"
)

func ResolveSVMBackupRef(svmbackupCache ctlharvesterv1.ScheduleVMBackupCache, obj metav1.Object) *harvesterv1.ScheduleVMBackup {
	var annotations = obj.GetAnnotations()
	if annotations == nil || annotations[AnnotationSVMBackupID] == "" {
		return nil
	}

	namespace, name := ref.Parse(annotations[AnnotationSVMBackupID])
	svmbackup, err := svmbackupCache.Get(namespace, name)
	if err != nil {
		return nil
	}

	return svmbackup
}
