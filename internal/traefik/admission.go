package traefik

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

const MaxAdmissionBytes = 1 << 20

// The wire types deliberately avoid a typed Traefik or Kubernetes API client.
// Unknown AdmissionRequest fields (including dryRun) cannot affect the pure result.
type admissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *admissionRequest  `json:"request,omitempty"`
	Response   *admissionResponse `json:"response,omitempty"`
}
type admissionRequest struct {
	UID         types.UID                   `json:"uid"`
	Kind        schema.GroupVersionKind     `json:"kind"`
	Resource    schema.GroupVersionResource `json:"resource"`
	SubResource string                      `json:"subResource"`
	Namespace   string                      `json:"namespace"`
	Name        string                      `json:"name"`
	Operation   string                      `json:"operation"`
	Object      json.RawMessage             `json:"object"`
	OldObject   json.RawMessage             `json:"oldObject"`
	DryRun      *bool                       `json:"dryRun,omitempty"`
}
type admissionResponse struct {
	UID       types.UID      `json:"uid"`
	Allowed   bool           `json:"allowed"`
	Result    *metav1.Status `json:"status,omitempty"`
	Patch     []byte         `json:"patch,omitempty"`
	PatchType *string        `json:"patchType,omitempty"`
}
type patch struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

// NewAdmissionHandler is pure: it holds no Kubernetes client or mutable state.
func NewAdmissionHandler(cfg Config) http.Handler {
	return newAdmissionHandler(nil, cfg, false)
}

// Read-only Kubernetes validation is safe for dry-run; no child writes occur here.
func NewValidatedAdmissionHandler(client dynamic.Interface, cfg Config) http.Handler {
	return newAdmissionHandler(client, cfg, true)
}

func newAdmissionHandler(client dynamic.Interface, cfg Config, validate bool) http.Handler {
	c := defaults(cfg)
	configErr := ValidateConfig(c)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxAdmissionBytes))
		if err != nil {
			http.Error(w, "invalid or oversized admission body", http.StatusRequestEntityTooLarge)
			return
		}
		var review admissionReview
		if json.Unmarshal(body, &review) != nil || review.APIVersion != "admission.k8s.io/v1" || review.Kind != "AdmissionReview" || review.Request == nil || review.Request.UID == "" || review.Response != nil {
			http.Error(w, "invalid admission.k8s.io/v1 AdmissionReview or missing UID", http.StatusBadRequest)
			return
		}
		response := &admissionResponse{UID: review.Request.UID, Allowed: true}
		var patches []patch
		if configErr != nil {
			err = configErr
		} else {
			patches, err = prepareAdmission(review.Request, c)
			if err == nil && validate && review.Request.Operation != "DELETE" {
				var route unstructured.Unstructured
				if err = json.Unmarshal(review.Request.Object, &route.Object); err == nil {
					ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
					_, err = validateMTLS(ctx, client, &route, c)
					cancel()
				}
			}
		}
		if err != nil {
			response.Allowed = false
			response.Result = &metav1.Status{Status: "Failure", Reason: metav1.StatusReasonInvalid, Code: 422, Message: err.Error()}
		} else if len(patches) > 0 {
			response.Patch, _ = json.Marshal(patches)
			kind := "JSONPatch"
			response.PatchType = &kind
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Response: response})
	})
}

func prepareAdmission(req *admissionRequest, c Config) ([]patch, error) {
	gvr, err := routeGVR(req.Kind.GroupVersion().String())
	if err != nil || req.Kind.Kind != "IngressRoute" || req.Resource != gvr || req.SubResource != "" {
		return nil, fmt.Errorf("only pinned v1alpha1 IngressRoute create/update requests are supported")
	}
	if !namespaceAllowed(req.Namespace, c) {
		return nil, fmt.Errorf("namespace is not allowed")
	}
	if req.Operation != "CREATE" && req.Operation != "UPDATE" && req.Operation != "DELETE" {
		return nil, fmt.Errorf("unsupported admission operation")
	}
	decode := func(raw json.RawMessage) (*unstructured.Unstructured, error) {
		u := &unstructured.Unstructured{}
		if len(raw) == 0 || json.Unmarshal(raw, &u.Object) != nil || u.Object == nil || u.GetName() == "" || u.GetName() != req.Name || u.GetNamespace() != req.Namespace || u.GetKind() != req.Kind.Kind || u.GetAPIVersion() != req.Kind.GroupVersion().String() {
			return nil, fmt.Errorf("admission object identity does not match the request")
		}
		return u, nil
	}
	if req.Operation == "DELETE" {
		_, err := decode(req.OldObject)
		return nil, err
	}
	route, err := decode(req.Object)
	if err != nil {
		return nil, err
	}
	var old *unstructured.Unstructured
	if req.Operation == "UPDATE" {
		old, err = decode(req.OldObject)
		if err != nil {
			return nil, err
		}
		if old.GetUID() == "" || route.GetUID() != old.GetUID() {
			return nil, fmt.Errorf("update must preserve parent UID")
		}
	}
	if _, err := siteExcluded(route.GetAnnotations()); err != nil {
		return nil, err
	}
	if route.GetDeletionTimestamp() != nil {
		return nil, nil
	}
	if _, err := settings(route, c); err != nil {
		return nil, err
	}
	desired := route.DeepCopy()
	if err := normalizeParent(desired, old, c); err != nil {
		return nil, err
	}
	var result []patch
	if !reflect.DeepEqual(route.GetAnnotations(), desired.GetAnnotations()) {
		if desired.GetAnnotations() == nil {
			result = append(result, patch{Op: "remove", Path: "/metadata/annotations"})
		} else {
			result = append(result, patch{Op: "add", Path: "/metadata/annotations", Value: desired.GetAnnotations()})
		}
	}
	for _, field := range []string{"entryPoints", "tls"} {
		b, bf, _ := unstructured.NestedFieldNoCopy(route.Object, "spec", field)
		a, af, _ := unstructured.NestedFieldNoCopy(desired.Object, "spec", field)
		if bf != af || !reflect.DeepEqual(b, a) {
			if af {
				result = append(result, patch{Op: "add", Path: "/spec/" + field, Value: a})
			} else {
				result = append(result, patch{Op: "remove", Path: "/spec/" + field})
			}
		}
	}
	return result, nil
}
