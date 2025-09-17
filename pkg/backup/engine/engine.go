package engine

import (
	"errors"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
)

var (
	ErrRetryLater = errors.New("retry later error")
)

type BackupEngine interface {
	Create(vmb *harvesterv1.VirtualMachineBackup, volIndex int, vscMap map[string]snapshotv1.VolumeSnapshotClass) error
	UpdateProgress(*harvesterv1.VolumeBackup) (int64, error)
	ForceDelete(vmb *harvesterv1.VirtualMachineBackup, volIndex int) error
}
