// Package agent exposes a namespace-scoped, read-only view of native k8gb data.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/dnszones"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

var (
	canonicalGSLB  = schema.GroupVersionResource{Group: "k8gb.io", Version: "v1beta1", Resource: "gslbs"}
	legacyGSLB     = schema.GroupVersionResource{Group: "k8gb.absa.oss", Version: "v1beta1", Resource: "gslbs"}
	namespaces     = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	ingresses      = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	httpRoutes     = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	dnsEndpoints   = schema.GroupVersionResource{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}
	endpointSlices = schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}
	services       = schema.GroupVersionResource{Version: "v1", Resource: "services"}
)

var ErrNamespaceDenied = errors.New("namespace is not allowed")
var ErrInvalidNamespace = errors.New("one valid namespace is required")
var ErrPrimaryAPIUnavailable = errors.New("neither canonical nor legacy Gslb API is available")

// APIUnavailableError distinguishes missing Gslb APIs from a genuinely empty
// namespace. Callers can use errors.As or errors.Is(ErrPrimaryAPIUnavailable).
type APIUnavailableError struct {
	Canonical error
	Legacy    error
}

func (e *APIUnavailableError) Error() string { return ErrPrimaryAPIUnavailable.Error() }
func (e *APIUnavailableError) Unwrap() []error {
	return []error{ErrPrimaryAPIUnavailable, e.Canonical, e.Legacy}
}

type Options struct {
	DNSZones          *dnszones.Reader
	ClusterID         string
	AllowedNamespaces []string
	SnapshotTimeout   time.Duration
	RequestTimeout    time.Duration
	PageSize          int64
	MaxPages          int
}

type Collector struct {
	client  dynamic.Interface
	opts    Options
	allowed map[string]bool
}

func NewCollector(client dynamic.Interface, opts Options) (*Collector, error) {
	if client == nil || strings.TrimSpace(opts.ClusterID) == "" {
		return nil, errors.New("dynamic client and cluster ID are required")
	}
	if opts.SnapshotTimeout == 0 {
		opts.SnapshotTimeout = 30 * time.Second
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 5 * time.Second
	}
	if opts.PageSize == 0 {
		opts.PageSize = 500
	}
	if opts.MaxPages == 0 {
		opts.MaxPages = 100
	}
	if opts.SnapshotTimeout <= 0 || opts.SnapshotTimeout > 5*time.Minute || opts.RequestTimeout <= 0 || opts.RequestTimeout > opts.SnapshotTimeout || opts.PageSize < 1 || opts.PageSize > 1000 || opts.MaxPages < 1 || opts.MaxPages > 1000 {
		return nil, errors.New("invalid collection timeout or pagination bounds")
	}
	c := &Collector{client: client, opts: opts, allowed: map[string]bool{}}
	for _, ns := range opts.AllowedNamespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return nil, fmt.Errorf("invalid allowed namespace %q", ns)
		}
		c.allowed[ns] = true
	}
	return c, nil
}

func (c *Collector) validateNamespace(ns string) error {
	if len(validation.IsDNS1123Label(ns)) != 0 {
		return ErrInvalidNamespace
	}
	if len(c.allowed) > 0 && !c.allowed[ns] {
		return ErrNamespaceDenied
	}
	return nil
}

func (c *Collector) Namespaces(ctx context.Context) (v1.NamespaceList, error) {
	result := v1.NamespaceList{APIVersion: v1.Version, ClusterID: c.opts.ClusterID, Namespaces: []string{}}
	if len(c.allowed) > 0 {
		for ns := range c.allowed {
			result.Namespaces = append(result.Namespaces, ns)
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, c.opts.SnapshotTimeout)
		defer cancel()
		items, err := c.list(ctx, namespaces, "")
		if err != nil {
			return result, err
		}
		for _, item := range items {
			result.Namespaces = append(result.Namespaces, item.GetName())
		}
	}
	sort.Strings(result.Namespaces)
	return result, nil
}

// Only Namespace itself is cluster-scoped. All other reads require a validated
// namespace, even when the service account has cluster-wide RBAC.
func (c *Collector) list(ctx context.Context, gvr schema.GroupVersionResource, ns string) ([]unstructured.Unstructured, error) {
	var resource dynamic.ResourceInterface
	if gvr == namespaces {
		resource = c.client.Resource(gvr)
	} else {
		if err := c.validateNamespace(ns); err != nil {
			return nil, err
		}
		resource = c.client.Resource(gvr).Namespace(ns)
	}
	items := []unstructured.Unstructured{}
	seen := map[string]bool{}
	options := metav1.ListOptions{Limit: c.opts.PageSize}
	seconds := int64((c.opts.RequestTimeout + time.Second - 1) / time.Second)
	options.TimeoutSeconds = &seconds
	for page := 0; page < c.opts.MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		requestCtx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
		result, err := resource.List(requestCtx, options)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", gvr.String(), err)
		}
		if int64(len(result.Items)) > c.opts.PageSize {
			return nil, fmt.Errorf("list %s exceeded page size", gvr.String())
		}
		for _, item := range result.Items {
			if gvr != namespaces && item.GetNamespace() != ns {
				return nil, fmt.Errorf("list %s returned an out-of-scope object", gvr.String())
			}
			items = append(items, item)
		}
		if result.GetContinue() == "" {
			return items, nil
		}
		if seen[result.GetContinue()] {
			return nil, fmt.Errorf("list %s repeated a continuation token", gvr.String())
		}
		seen[result.GetContinue()] = true
		options.Continue = result.GetContinue()
	}
	return nil, fmt.Errorf("list %s exceeded %d pages", gvr.String(), c.opts.MaxPages)
}

type collection struct {
	items     []unstructured.Unstructured
	available bool
}

func (c *Collector) Snapshot(ctx context.Context, ns string) (v1.Snapshot, error) {
	result := v1.Snapshot{
		APIVersion: v1.Version, ClusterID: c.opts.ClusterID, Namespace: ns,
		CollectedAt: time.Now().UTC(), Capabilities: v1.Capabilities{ReadOnly: true, Providers: []string{"native"}},
		Objects: []v1.Object{}, Warnings: []string{},
	}
	if err := c.validateNamespace(ns); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.opts.SnapshotTimeout)
	defer cancel()
	canonical, err := c.list(ctx, canonicalGSLB, ns)
	if err != nil && !apierrors.IsNotFound(err) {
		return result, err
	}
	// Canonical is authoritative: never fall back on Forbidden, timeouts or 5xx.
	// On success, also read legacy to retain independently managed, unmigrated
	// objects. On canonical NotFound alone, legacy is the fallback API.
	legacy, legacyErr := c.list(ctx, legacyGSLB, ns)
	if legacyErr != nil {
		if apierrors.IsNotFound(err) && apierrors.IsNotFound(legacyErr) {
			return result, &APIUnavailableError{Canonical: err, Legacy: legacyErr}
		}
		if err != nil && !apierrors.IsNotFound(legacyErr) {
			return result, legacyErr
		}
		if !apierrors.IsNotFound(legacyErr) {
			result.Warnings = append(result.Warnings, legacyErr.Error())
		}
	}
	// Only primary inventory reads determine completeness. Optional resource
	// failures below must not make a complete Gslb inventory appear partial.
	result.ObjectsComplete = legacyErr == nil || apierrors.IsNotFound(legacyErr)
	all := append([]unstructured.Unstructured{}, canonical...)
	for _, old := range legacy {
		if !migratedDuplicate(old, canonical) {
			all = append(all, old)
		}
	}
	if len(all) == 0 {
		return result, nil
	}

	data := map[schema.GroupVersionResource]collection{}
	data[canonicalGSLB] = collection{items: canonical, available: err == nil || apierrors.IsNotFound(err)}
	data[legacyGSLB] = collection{items: legacy, available: legacyErr == nil || apierrors.IsNotFound(legacyErr)}
	optional := []schema.GroupVersionResource{dnsEndpoints, ingresses, endpointSlices, services}
	for _, gslb := range all {
		if str(gslb.Object, "spec", "resourceRef", "kind") == "HTTPRoute" {
			optional = append(optional, httpRoutes)
			break
		}
	}
	for _, gvr := range optional {
		items, readErr := c.list(ctx, gvr, ns)
		if readErr != nil {
			result.Warnings = append(result.Warnings, readErr.Error())
		}
		data[gvr] = collection{items: items, available: readErr == nil}
	}
	if ctx.Err() != nil {
		result.Warnings = append(result.Warnings, "collection deadline reached; optional data may be incomplete")
	}
	for _, gslb := range all {
		result.Objects = append(result.Objects, buildObjects(gslb, data)...)
	}
	sort.Slice(result.Objects, func(i, j int) bool {
		a, b := result.Objects[i], result.Objects[j]
		return a.Resource.APIVersion+"/"+a.Resource.Name+"/"+a.Hostname < b.Resource.APIVersion+"/"+b.Resource.Name+"/"+b.Hostname
	})
	return result, nil
}

func migratedDuplicate(old unstructured.Unstructured, canonical []unstructured.Unstructured) bool {
	for _, current := range canonical {
		if old.GetNamespace() != current.GetNamespace() {
			continue
		}
		if old.GetName() == current.GetName() && old.GetLabels()["k8gb.io/migrated-to-k8gb-io"] == "true" {
			return true
		}
		// A real UID ownership link is evidence; equal names or equal hostnames
		// across groups (or references to the same Ingress) are not.
		if owns(current, old) || owns(old, current) {
			return true
		}
	}
	return false
}

func owns(owner, child unstructured.Unstructured) bool {
	for _, ref := range child.GetOwnerReferences() {
		if owner.GetUID() != "" && ref.UID == owner.GetUID() && ref.APIVersion == owner.GetAPIVersion() && ref.Kind == owner.GetKind() && ref.Name == owner.GetName() {
			return true
		}
	}
	return false
}
