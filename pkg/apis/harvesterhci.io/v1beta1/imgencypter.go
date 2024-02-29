package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +genclient
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +kubebuilder:printcolumn:name="STAGE",type="integer",JSONPath=`.status.stage`

type ImgEncrypter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ImgEncrypterSpec   `json:"spec"`
	Status ImgEncrypterStatus `json:"status,omitempty"`
}

type ImgEncrypterSpec struct {
	// +kubebuilder:validation:Required
	DisplayName string `json:"displayName"`

	// +kubebuilder:validation:Required
	SrcImgNamespace string `json:"srcImgNamespace"`

	// +kubebuilder:validation:Required
	SrcImgName string `json:"srcImgName"`
}

type ImgEncrypterStatus struct {
	// +optional
	Stage int64 `json:"stage,omitempty"`

	// +optional
	Message string `json:"message"`
}
