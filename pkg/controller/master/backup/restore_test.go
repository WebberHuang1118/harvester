package backup

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	restorecommon "github.com/harvester/harvester/pkg/restore/common"
	"github.com/harvester/harvester/pkg/restore/engine"
)

func TestReconcileVolumeRestores(t *testing.T) {
	errBoot := errors.New("boot restore failed")
	errData := errors.New("data restore failed")

	tests := []struct {
		name       string
		errors     []error
		wantReady  bool
		wantRetry  bool
		wantErrors []error
		wantCalls  int
	}{
		{
			name:      "all volumes ready",
			errors:    []error{nil, nil},
			wantReady: true,
			wantCalls: 2,
		},
		{
			name:      "pending volume",
			errors:    []error{engine.ErrRetryLater, nil},
			wantRetry: true,
			wantCalls: 2,
		},
		{
			name:      "wrapped pending volume",
			errors:    []error{fmt.Errorf("snapshot check: %w", engine.ErrRetryLater), nil},
			wantRetry: true,
			wantCalls: 2,
		},
		{
			name:       "hard error stops reconciliation",
			errors:     []error{errBoot, errData},
			wantErrors: []error{errBoot},
			wantCalls:  1,
		},
		{
			name:       "hard error takes precedence over pending",
			errors:     []error{engine.ErrRetryLater, errData},
			wantErrors: []error{errData},
			wantCalls:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re := &fakeRestoreEngine{errors: tt.errors}
			h := &RestoreHandler{
				vmro: restorecommon.NewVMRestoreOperatorBuilder().Build(),
				engines: map[harvesterv1.BackupType]engine.RestoreEngine{
					harvesterv1.Restic: re,
				},
			}
			vmr := &harvesterv1.VirtualMachineRestore{
				Status: harvesterv1.VirtualMachineRestoreStatus{
					VolumeRestores: []harvesterv1.VolumeRestore{
						{VolumeName: "boot-disk"},
						{VolumeName: "data-disk"},
					},
				},
			}
			vmb := &harvesterv1.VirtualMachineBackup{
				Spec: harvesterv1.VirtualMachineBackupSpec{Type: harvesterv1.Restic},
			}

			ready, err := h.reconcileVolumeRestores(vmr, vmb)

			require.Equal(t, tt.wantReady, ready)
			require.Len(t, re.calls, tt.wantCalls)
			require.Equal(t, tt.wantRetry, errors.Is(err, engine.ErrRetryLater))
			for _, wantErr := range tt.wantErrors {
				require.ErrorIs(t, err, wantErr)
			}
		})
	}
}

type fakeRestoreEngine struct {
	errors []error
	calls  []int
}

func (f *fakeRestoreEngine) Reconcile(
	_ *harvesterv1.VirtualMachineRestore,
	_ *harvesterv1.VirtualMachineBackup,
	index int,
) error {
	f.calls = append(f.calls, index)
	return f.errors[index]
}

func (f *fakeRestoreEngine) UpdateProgress(*harvesterv1.VolumeRestore) (int64, error) {
	return 0, nil
}

func (f *fakeRestoreEngine) Delete(*harvesterv1.VirtualMachineRestore, int) error {
	return nil
}

func (f *fakeRestoreEngine) RegisterWatchers(
	context.Context,
	func(namespace, name string),
) {
}
