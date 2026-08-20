package types

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/wrangler/v3/pkg/webhook"
	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeRelatedResourceValidator struct {
	DefaultValidator
	resources    []RelatedResource
	resolveErr   error
	operation    admissionv1.Operation
	oldObj       runtime.Object
	newObj       runtime.Object
	createCalled bool
	updateCalled bool
}

func (v *fakeRelatedResourceValidator) Resource() Resource {
	return Resource{}
}

func (v *fakeRelatedResourceValidator) ResolveAccessChecks(
	_ *Request,
	operation admissionv1.Operation,
	oldObj runtime.Object,
	newObj runtime.Object,
) ([]RelatedResource, error) {
	v.operation = operation
	v.oldObj = oldObj
	v.newObj = newObj
	return v.resources, v.resolveErr
}

func (v *fakeRelatedResourceValidator) Create(_ *Request, _ runtime.Object) error {
	v.createCalled = true
	return nil
}

func (v *fakeRelatedResourceValidator) Update(_ *Request, _, _ runtime.Object) error {
	v.updateCalled = true
	return nil
}

type fakeSubjectAccessReviews struct {
	allowed bool
	err     error
	review  *authorizationv1.SubjectAccessReview
}

func (f *fakeSubjectAccessReviews) Create(
	_ context.Context,
	review *authorizationv1.SubjectAccessReview,
	_ metav1.CreateOptions,
) (*authorizationv1.SubjectAccessReview, error) {
	f.review = review
	if f.err != nil {
		return nil, f.err
	}

	result := review.DeepCopy()
	result.Status.Allowed = f.allowed
	return result, nil
}

func TestValidatorAdapterChecksRelatedResourceAccess(t *testing.T) {
	resource := RelatedResource{
		GVR: schema.GroupVersionResource{
			Group:    "harvesterhci.io",
			Version:  "v1beta1",
			Resource: "virtualmachineimages",
		},
		Namespace:   "images",
		Name:        "base-image",
		Description: "image",
		Field:       "spec.storageClassName",
	}
	request := NewRequest(&webhook.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UserInfo: authenticationv1.UserInfo{
				Username: "alice",
				Groups:   []string{"developers", "image-readers"},
			},
		},
		Context: context.Background(),
	}, nil)
	object := &corev1.ConfigMap{}

	t.Run("create is delegated after access is allowed", func(t *testing.T) {
		validator := &fakeRelatedResourceValidator{resources: []RelatedResource{resource}}
		sar := &fakeSubjectAccessReviews{allowed: true}
		adapter := NewValidatorAdapter(validator, sar)

		_, err := adapter.Create(request, object)

		assert.NoError(t, err)
		assert.True(t, validator.createCalled)
		assert.Equal(t, admissionv1.Create, validator.operation)
		assert.Nil(t, validator.oldObj)
		assert.Same(t, object, validator.newObj)
		if assert.NotNil(t, sar.review) {
			assert.Equal(t, request.UserInfo.Username, sar.review.Spec.User)
			assert.Equal(t, request.UserInfo.Groups, sar.review.Spec.Groups)
			assert.Equal(t, &authorizationv1.ResourceAttributes{
				Namespace: resource.Namespace,
				Verb:      "get",
				Group:     resource.GVR.Group,
				Version:   resource.GVR.Version,
				Resource:  resource.GVR.Resource,
				Name:      resource.Name,
			}, sar.review.Spec.ResourceAttributes)
		}
	})

	t.Run("denied access prevents validator execution", func(t *testing.T) {
		validator := &fakeRelatedResourceValidator{resources: []RelatedResource{resource}}
		adapter := NewValidatorAdapter(validator, &fakeSubjectAccessReviews{})

		_, err := adapter.Create(request, object)

		assert.EqualError(t, err, `user "alice" is not allowed to access image images/base-image`)
		assert.False(t, validator.createCalled)
		status := err.(interface{ AsResult() *metav1.Status }).AsResult()
		assert.Equal(t, int32(422), status.Code)
		assert.Equal(t, resource.Field, status.Details.Causes[0].Field)
	})

	t.Run("SAR error prevents validator execution", func(t *testing.T) {
		validator := &fakeRelatedResourceValidator{resources: []RelatedResource{resource}}
		adapter := NewValidatorAdapter(validator, &fakeSubjectAccessReviews{err: errors.New("api unavailable")})

		_, err := adapter.Create(request, object)

		assert.EqualError(t, err,
			"failed to check access to image images/base-image: failed to check access: api unavailable")
		assert.False(t, validator.createCalled)
	})

	t.Run("update passes both objects to resolver", func(t *testing.T) {
		validator := &fakeRelatedResourceValidator{}
		adapter := NewValidatorAdapter(validator, &fakeSubjectAccessReviews{allowed: true})
		oldObj := &corev1.ConfigMap{}

		_, err := adapter.Update(request, oldObj, object)

		assert.NoError(t, err)
		assert.True(t, validator.updateCalled)
		assert.Equal(t, admissionv1.Update, validator.operation)
		assert.Same(t, oldObj, validator.oldObj)
		assert.Same(t, object, validator.newObj)
	})

	t.Run("resolver error prevents validator execution", func(t *testing.T) {
		resolveErr := errors.New("failed to resolve reference")
		validator := &fakeRelatedResourceValidator{resolveErr: resolveErr}
		adapter := NewValidatorAdapter(validator, &fakeSubjectAccessReviews{allowed: true})

		_, err := adapter.Create(request, object)

		assert.ErrorIs(t, err, resolveErr)
		assert.False(t, validator.createCalled)
	})

	t.Run("missing SAR client fails closed", func(t *testing.T) {
		validator := &fakeRelatedResourceValidator{resources: []RelatedResource{resource}}
		adapter := NewValidatorAdapter(validator, nil)

		_, err := adapter.Create(request, object)

		assert.EqualError(t, err, "subject access review client is not configured")
		assert.False(t, validator.createCalled)
	})
}
