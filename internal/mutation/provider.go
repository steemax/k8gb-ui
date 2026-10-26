package mutation

import (
	"context"
	"fmt"

	v1 "k8gb-ui/api/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Prepared is reconstructed from live Kubernetes on both plan and apply. Fields
// are JSON pointers which the provider alone may change, not arbitrary patches.
type Prepared struct {
	GVR          schema.GroupVersionResource
	Current      *unstructured.Unstructured
	Desired      *unstructured.Unstructured
	Fields       []string
	Dependencies []v1.ResourceReference
	Warnings     []string
}

type Provider interface {
	Prepare(context.Context, v1.MutationRequest) (Prepared, error)
	Operations() []string
}

type Error struct {
	Status         int
	Code           string
	Message        string
	OutcomeUnknown bool
}

func (e *Error) Error() string { return e.Message }
func Failure(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
}
func (e *Error) Public() v1.MutationError {
	return v1.MutationError{Code: e.Code, Error: e.Message, OutcomeUnknown: e.OutcomeUnknown}
}
func Unsupported(provider, operation string) error {
	return Failure(422, "unsupported_operation", fmt.Sprintf("%s does not support %s", provider, operation))
}
