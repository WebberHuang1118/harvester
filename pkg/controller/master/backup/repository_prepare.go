package backup

import (
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/settings"
	"github.com/harvester/harvester/pkg/util"
	backuputil "github.com/harvester/harvester/pkg/util/backup"
)

func (h *TargetHandler) prepareTarget(
	setting *harvesterv1.Setting, target *settings.BackupTarget,
) (*harvesterv1.Setting, error) {
	if err := backuputil.CheckPasswordInput(target.RepoPassword); err != nil {
		return setting, err
	}
	secret, data, err := h.prepareS3Secret(target)
	if err != nil {
		return setting, err
	}
	if err := backuputil.CheckPasswordChange(data, backuputil.RepositoryID(target), target.RepoPassword); err != nil {
		return setting, err
	}
	data[backuputil.ResticPasswordKey] = []byte(target.RepoPassword)
	data[backuputil.KopiaPasswordKey] = []byte(target.RepoPassword)
	setting, err = h.beginPreparation(setting)
	if err != nil {
		return setting, err
	}
	if err := h.prepare(h.ctx, target, data, true); err != nil {
		return setting, err
	}
	setting, err = h.currentSetting(setting)
	if err != nil {
		return setting, err
	}
	if err := h.saveS3Secret(secret, data, target); err != nil {
		return setting, err
	}
	saved, err := h.getBackupSecret()
	if err != nil {
		return setting, err
	}
	if saved == nil || !reflect.DeepEqual(saved.Data, data) {
		return setting, fmt.Errorf("backup target credentials changed during preparation")
	}
	return h.saveReceipt(setting, target, saved)
}

// checkSavedRepository handles startup and interrupted publication after masking.
// It can open saved repositories, but never initializes missing remote contents.
func (h *TargetHandler) checkSavedRepository(
	setting *harvesterv1.Setting, target *settings.BackupTarget,
) (*harvesterv1.Setting, error) {
	secret, err := h.checkSavedSecret(target)
	if err != nil {
		return setting, err
	}
	if backuputil.CheckReady(setting, target, secret) == nil {
		return setting, nil
	}
	setting, err = h.beginPreparation(setting)
	if err != nil {
		return setting, err
	}
	if err := h.prepare(h.ctx, target, secret.Data, false); err != nil {
		return setting, err
	}
	current, err := h.getBackupSecret()
	if err != nil {
		return setting, err
	}
	if current == nil || current.UID != secret.UID || current.ResourceVersion != secret.ResourceVersion {
		return setting, fmt.Errorf("backup target credentials changed during validation")
	}
	setting, err = h.currentSetting(setting)
	if err != nil {
		return setting, err
	}
	if err := h.updateLonghornTarget(target); err != nil {
		return setting, err
	}
	if err := h.setLonghornSecret(util.BackupTargetSecretName); err != nil {
		return setting, err
	}
	return h.saveReceipt(setting, target, secret)
}

func (h *TargetHandler) beginPreparation(setting *harvesterv1.Setting) (*harvesterv1.Setting, error) {
	current, err := h.currentSetting(setting)
	if err != nil {
		return setting, err
	}
	updated := current.DeepCopy()
	delete(updated.Annotations, backuputil.ReadyAnnotation)
	if !harvesterv1.SettingConfigured.IsFalse(updated) {
		harvesterv1.SettingConfigured.False(updated)
		harvesterv1.SettingConfigured.Message(updated, "Preparing backup repositories")
	}
	if !reflect.DeepEqual(current, updated) {
		current, err = h.settings.Update(updated)
		if err != nil {
			return setting, err
		}
	}
	return current, h.stopMaintenance()
}

// currentSetting rejects results from a superseded submission. Metadata-only
// writes can be retained; every subsequent update still uses API conflict checks.
func (h *TargetHandler) currentSetting(setting *harvesterv1.Setting) (*harvesterv1.Setting, error) {
	current, err := h.settings.Get(setting.Name, metav1.GetOptions{})
	if err != nil {
		return setting, err
	}
	if current.DeletionTimestamp != nil || current.UID != setting.UID || current.Value != setting.Value {
		return setting, apierrors.NewConflict(schema.GroupResource{
			Group: harvesterv1.SchemeGroupVersion.Group, Resource: "settings",
		}, setting.Name, fmt.Errorf("backup target changed during preparation"))
	}
	return current, nil
}

func (h *TargetHandler) saveReceipt(
	setting *harvesterv1.Setting, target *settings.BackupTarget, secret *corev1.Secret,
) (*harvesterv1.Setting, error) {
	current, err := h.currentSetting(setting)
	if err != nil {
		return setting, err
	}
	record, err := backuputil.ReadyRecord(target, secret)
	if err != nil {
		return setting, err
	}
	updated := current.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[backuputil.ReadyAnnotation] = record
	// Publish the receipt with the masked value. Configured remains false until
	// Longhorn synchronization and the remaining target updates have succeeded.
	if !backuputil.Masked(target) {
		return h.stripCredentials(updated, target)
	}
	saved, err := h.settings.Update(updated)
	if err != nil {
		return setting, err
	}
	return saved, nil
}

func (h *TargetHandler) clearReceipt(setting *harvesterv1.Setting) (*harvesterv1.Setting, error) {
	if setting.Annotations[backuputil.ReadyAnnotation] == "" {
		return setting, nil
	}
	updated := setting.DeepCopy()
	delete(updated.Annotations, backuputil.ReadyAnnotation)
	saved, err := h.settings.Update(updated)
	if err != nil {
		return setting, err
	}
	return saved, nil
}
