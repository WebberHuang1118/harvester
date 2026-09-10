package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	harvesterv1 "github.com/harvester/harvester/pkg/apis/harvesterhci.io/v1beta1"
	"github.com/harvester/harvester/pkg/generated/clientset/versioned/fake"
	"github.com/harvester/harvester/pkg/util"
	"github.com/harvester/harvester/pkg/util/fakeclients"
)

func TestCheckSCNameOverride(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		existingSC  string
		errContains string
	}{
		{
			name: "annotation absent",
		},
		{
			name: "valid unused name",
			annotations: map[string]string{
				util.AnnotationVMImageSCNameOverride: "my-custom-storage-class",
			},
		},
		{
			name: "empty name",
			annotations: map[string]string{
				util.AnnotationVMImageSCNameOverride: "",
			},
			errContains: "is not a valid Longhorn name",
		},
		{
			name: "name requiring auto-correction",
			annotations: map[string]string{
				util.AnnotationVMImageSCNameOverride: strings.Repeat("a", 41),
			},
			errContains: "would be auto-corrected",
		},
		{
			name: "existing storage class",
			annotations: map[string]string{
				util.AnnotationVMImageSCNameOverride: "existing-storage-class",
			},
			existingSC:  "existing-storage-class",
			errContains: "is already in use",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []runtime.Object
			if tt.existingSC != "" {
				objects = append(objects, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: tt.existingSC}})
			}
			clientSet := fake.NewSimpleClientset(objects...)
			validator := &vmiValidator{
				scCache: fakeclients.StorageClassCache(clientSet.StorageV1().StorageClasses),
			}

			err := validator.CheckSCNameOverride(&harvesterv1.VirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations},
			})
			if tt.errContains == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.errContains)
			}
		})
	}
}

func TestCheckDisplayName(t *testing.T) {
	maxLenDisplayName := strings.Repeat("a", 63)
	tooLongDisplayName := strings.Repeat("a", 64)

	testCases := []struct {
		name           string
		displayName    string
		existingImages []*harvesterv1.VirtualMachineImage
		expectErr      bool
		errContains    string
	}{
		{
			name:        "rejects empty displayName",
			displayName: "",
			expectErr:   true,
			errContains: "displayName is required",
		},
		{
			name:        "accepts displayName with 63 chars",
			displayName: maxLenDisplayName,
			expectErr:   false,
		},
		{
			name:        "rejects displayName with more than 63 chars",
			displayName: tooLongDisplayName,
			expectErr:   true,
			errContains: "must be no more than 63 characters",
		},
		{
			name:        "rejects displayName with invalid Kubernetes label value",
			displayName: "Invalid/Name",
			expectErr:   true,
			errContains: "displayName is not a valid Kubernetes label value",
		},
		{
			name:        "rejects duplicate displayName",
			displayName: "duplicate-name",
			existingImages: []*harvesterv1.VirtualMachineImage{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "existing-image",
						Namespace: "default",
						UID:       "existing-uid",
						Labels: map[string]string{
							util.LabelImageDisplayName: "duplicate-name",
						},
					},
				},
			},
			expectErr:   true,
			errContains: "A resource with the same name exists",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clientSet := fake.NewSimpleClientset()
			for _, image := range tc.existingImages {
				err := clientSet.Tracker().Add(image)
				assert.NoError(t, err)
			}

			validator := &vmiValidator{
				vmiCache: fakeclients.VirtualMachineImageCache(clientSet.HarvesterhciV1beta1().VirtualMachineImages),
			}

			vmi := &harvesterv1.VirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "new-image",
					Namespace: "default",
				},
				Spec: harvesterv1.VirtualMachineImageSpec{
					DisplayName: tc.displayName,
				},
			}

			err := validator.CheckDisplayName(vmi)
			if tc.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCheckUpdateDisplayName(t *testing.T) {
	testCases := []struct {
		name           string
		oldDisplayName string
		newDisplayName string
		expectErr      bool
		errContains    string
	}{
		{
			name:           "accepts unchanged valid displayName",
			oldDisplayName: "valid-name",
			newDisplayName: "valid-name",
			expectErr:      false,
		},
		{
			name:           "rejects changed displayName",
			oldDisplayName: "old-name",
			newDisplayName: "new-name",
			expectErr:      true,
			errContains:    "displayName cannot be modified",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clientSet := fake.NewSimpleClientset()
			validator := &vmiValidator{
				vmiCache: fakeclients.VirtualMachineImageCache(clientSet.HarvesterhciV1beta1().VirtualMachineImages),
			}

			oldVMI := &harvesterv1.VirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "update-image",
					Namespace: "default",
				},
				Spec: harvesterv1.VirtualMachineImageSpec{
					DisplayName: tc.oldDisplayName,
				},
			}

			newVMI := &harvesterv1.VirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "update-image",
					Namespace: "default",
				},
				Spec: harvesterv1.VirtualMachineImageSpec{
					DisplayName: tc.newDisplayName,
				},
			}

			err := validator.CheckUpdateDisplayName(oldVMI, newVMI)
			if tc.expectErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
