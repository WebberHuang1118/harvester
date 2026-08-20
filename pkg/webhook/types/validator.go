package types

import (
	"fmt"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"

	"github.com/harvester/harvester/pkg/util"
	werror "github.com/harvester/harvester/pkg/webhook/error"
)

// RelatedResource identifies a resource referenced by the object being admitted.
type RelatedResource struct {
	// Verb defaults to get when omitted.
	Verb        string
	GVR         schema.GroupVersionResource
	Namespace   string
	Name        string
	Description string
	Field       string
}

// ResourceAccessResolver is implemented by validators that authorize access to
// resources referenced by an admitted object. The adapter calls the resolver for
// CREATE and UPDATE operations before invoking the resource-specific validator.
type ResourceAccessResolver interface {
	ResolveAccessChecks(
		request *Request,
		operation admissionv1.Operation,
		oldObj runtime.Object,
		newObj runtime.Object,
	) ([]RelatedResource, error)
}

// Validator is a Mutator that doesn't modify received API objects.
type Validator interface {
	// Create checks if a CREATE operation is allowed. If no error is returned, the operation is allowed.
	Create(request *Request, newObj runtime.Object) error

	// Update checks if a UPDATE operation is allowed. If no error is returned, the operation is allowed.
	Update(request *Request, oldObj runtime.Object, newObj runtime.Object) error

	// Delete checks if a DELETE operation is allowed. If no error is returned, the operation is allowed.
	Delete(request *Request, oldObj runtime.Object) error

	// Connect checks if a CONNECT operation is allowed. If no error is returned, the operation is allowed.
	Connect(request *Request, newObj runtime.Object) error

	Resource() Resource
}

// ValidatorAdapter adapts a Validator to an Admitter.
type ValidatorAdapter struct {
	validator Validator
	sar       authorizationv1client.SubjectAccessReviewInterface
}

func NewValidatorAdapter(validator Validator, sar authorizationv1client.SubjectAccessReviewInterface) Mutator {
	return &ValidatorAdapter{
		validator: validator,
		sar:       sar,
	}
}

func (c *ValidatorAdapter) Create(request *Request, newObj runtime.Object) (PatchOps, error) {
	if err := c.checkResourceAccess(request, admissionv1.Create, nil, newObj); err != nil {
		return nil, err
	}
	return nil, c.validator.Create(request, newObj)
}

func (c *ValidatorAdapter) Update(request *Request, oldObj runtime.Object, newObj runtime.Object) (PatchOps, error) {
	if err := c.checkResourceAccess(request, admissionv1.Update, oldObj, newObj); err != nil {
		return nil, err
	}
	return nil, c.validator.Update(request, oldObj, newObj)
}

func (c *ValidatorAdapter) checkResourceAccess(
	request *Request,
	operation admissionv1.Operation,
	oldObj runtime.Object,
	newObj runtime.Object,
) error {
	resolver, ok := c.validator.(ResourceAccessResolver)
	if !ok {
		return nil
	}

	resources, err := resolver.ResolveAccessChecks(request, operation, oldObj, newObj)
	if err != nil {
		return err
	}
	if len(resources) > 0 && c.sar == nil {
		return werror.NewInternalError("subject access review client is not configured")
	}
	for _, resource := range resources {
		if err := c.authorizeResource(request, resource); err != nil {
			return err
		}
	}

	return nil
}

func (c *ValidatorAdapter) authorizeResource(request *Request, resource RelatedResource) error {
	verb := resource.Verb
	if verb == "" {
		verb = util.VerbGet
	}

	allowed, err := util.CheckObjectAccess(request.Context, util.ResourceAccessCheck{
		SAR:       c.sar,
		Username:  request.UserInfo.Username,
		Groups:    request.UserInfo.Groups,
		Verb:      verb,
		GVR:       resource.GVR,
		Namespace: resource.Namespace,
		Name:      resource.Name,
	})
	if err != nil {
		return werror.NewInternalError(fmt.Sprintf(
			"failed to check access to %s %s/%s: %v",
			resource.Description, resource.Namespace, resource.Name, err,
		))
	}
	if allowed {
		return nil
	}

	return werror.NewInvalidError(fmt.Sprintf(
		"user %q is not allowed to access %s %s/%s",
		request.UserInfo.Username, resource.Description, resource.Namespace, resource.Name,
	), resource.Field)
}

func (c *ValidatorAdapter) Delete(request *Request, oldObj runtime.Object) (PatchOps, error) {
	return nil, c.validator.Delete(request, oldObj)
}

func (c *ValidatorAdapter) Connect(request *Request, newObj runtime.Object) (PatchOps, error) {
	return nil, c.validator.Connect(request, newObj)
}

func (c *ValidatorAdapter) Resource() Resource {
	return c.validator.Resource()
}

// DefaultValidator allows every supported operation.
type DefaultValidator struct {
}

func (v *DefaultValidator) Create(_ *Request, _ runtime.Object) error {
	return nil
}

func (v *DefaultValidator) Update(_ *Request, _ runtime.Object, _ runtime.Object) error {
	return nil
}

func (v *DefaultValidator) Delete(_ *Request, _ runtime.Object) error {
	return nil
}

func (v *DefaultValidator) Connect(_ *Request, _ runtime.Object) error {
	return nil
}
