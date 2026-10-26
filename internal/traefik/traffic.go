package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var serviceGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}

func siteExcluded(annotations map[string]string) (bool, error) {
	value, present := annotations[SiteExcludedAnnotation]
	if present && value != "true" && value != "false" {
		return false, fmt.Errorf("%s must be true or false when present", SiteExcludedAnnotation)
	}
	return value == "true", nil
}

// ExcludedServiceName is used only by generated global Ingresses. No Service is
// created, and no existing live backend name is inferred or suffixed.
func ExcludedServiceName(parentUID string) string {
	hash := sha256.Sum256([]byte(parentUID))
	return "k8gb-ui-excluded-" + hex.EncodeToString(hash[:8])
}

func checkExcludedServiceAbsent(ctx context.Context, client dynamic.Interface, parent *unstructured.Unstructured) error {
	name := ExcludedServiceName(string(parent.GetUID()))
	_, err := client.Resource(serviceGVR).Namespace(parent.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check excluded Service absence: %w", err)
	}
	return mutation.Failure(409, "resource_collision", "synthetic exclusion Service already exists: "+parent.GetNamespace()+"/"+name)
}

func (p *provider) siteTrafficDependencies(ctx context.Context, parent *unstructured.Unstructured, expected target) ([]v1.ResourceReference, error) {
	name := childName(parent, false)
	ingress, err := p.client.Resource(ingressGVR).Namespace(parent.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, unsupportedSiteChildren()
		}
		return nil, err
	}
	gslb, err := p.client.Resource(gslbGVR).Namespace(parent.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, unsupportedSiteChildren()
		}
		return nil, err
	}
	return siteTrafficDependencies(parent, ingress, gslb, expected)
}

func unsupportedSiteChildren() error {
	return mutation.Failure(409, "unsupported_site_children", "site traffic requires a supported owned generated global Ingress/Gslb pair pinned by UID and resourceVersion")
}

func siteTrafficDependencies(parent, ingress, gslb *unstructured.Unstructured, expected target) ([]v1.ResourceReference, error) {
	if parent.GetUID() == "" || parent.GetResourceVersion() == "" || parent.GetDeletionTimestamp() != nil {
		return nil, unsupportedSiteChildren()
	}
	name := childName(parent, false)
	for _, u := range []*unstructured.Unstructured{ingress, gslb} {
		if u == nil || u.GetName() != name || u.GetUID() == "" || u.GetResourceVersion() == "" || u.GetDeletionTimestamp() != nil || !owned(u, parent) {
			return nil, unsupportedSiteChildren()
		}
		if _, err := siteExcluded(u.GetAnnotations()); err != nil {
			return nil, unsupportedSiteChildren()
		}
	}
	if ingress.GetAPIVersion() != "networking.k8s.io/v1" || ingress.GetKind() != "Ingress" || gslb.GetAPIVersion() != "k8gb.io/v1beta1" || gslb.GetKind() != "Gslb" {
		return nil, unsupportedSiteChildren()
	}
	ref, _, err := unstructured.NestedMap(gslb.Object, "spec", "resourceRef")
	if err != nil || text(ref, "apiVersion") != ingress.GetAPIVersion() || text(ref, "kind") != "Ingress" || text(ref, "name") != name || (text(ref, "namespace") != "" && text(ref, "namespace") != parent.GetNamespace()) {
		return nil, unsupportedSiteChildren()
	}
	rules, _, err := unstructured.NestedSlice(ingress.Object, "spec", "rules")
	if err != nil || len(rules) != 1 {
		return nil, unsupportedSiteChildren()
	}
	rule, ok := rules[0].(map[string]interface{})
	if !ok || !validHost(text(rule, "host")) {
		return nil, unsupportedSiteChildren()
	}
	paths, _, err := unstructured.NestedSlice(rule, "http", "paths")
	if err != nil || len(paths) != 1 {
		return nil, unsupportedSiteChildren()
	}
	path, ok := paths[0].(map[string]interface{})
	if !ok || text(path, "path") != "/" || text(path, "pathType") != "Prefix" {
		return nil, unsupportedSiteChildren()
	}
	backend, _, err := unstructured.NestedMap(path, "backend")
	if err != nil || len(backend) != 1 {
		return nil, unsupportedSiteChildren()
	}
	service := firstIngressService(ingress)
	port, found, err := unstructured.NestedInt64(service, "port", "number")
	if service == nil || text(service, "name") == "" || err != nil || !found || port < 1 || port > 65535 {
		return nil, unsupportedSiteChildren()
	}
	marked := parent.GetAnnotations()[SiteExcludedAnnotation] == "true"
	currentName := text(service, "name")
	ordinary := currentName == expected.service && port == expected.port
	excluded := marked && currentName == ExcludedServiceName(string(parent.GetUID()))
	if !ordinary && !excluded {
		return nil, mutation.Failure(409, "site_traffic_conflict", "generated global Ingress backend differs from the current desired route; legacy/manual exclusion or an unmanaged change must be resolved before changing site traffic")
	}
	return []v1.ResourceReference{reference(ingress), reference(gslb)}, nil
}

// SiteTrafficCandidate is the stricter catalog view for excludeSite/includeSite.
// Candidate remains the enable/update view. Pass the already-read namespace
// children; local siblings and independent objects cannot satisfy this check.
// Service absence is deliberately checked from live Kubernetes during Prepare.
func SiteTrafficCandidate(resource unstructured.Unstructured, children []unstructured.Unstructured, cfg Config) v1.Candidate {
	c := Candidate(resource, cfg)
	if !c.Eligible {
		return c
	}
	if !c.Enabled {
		c.Eligible = false
		c.Reason = "enable the IngressRoute before changing site traffic"
		return c
	}
	var ingress, gslb *unstructured.Unstructured
	for i := range children {
		u := &children[i]
		if u.GetNamespace() != resource.GetNamespace() || u.GetName() != childName(&resource, false) {
			continue
		}
		switch u.GetAPIVersion() + "/" + u.GetKind() {
		case "networking.k8s.io/v1/Ingress":
			ingress = u
		case "k8gb.io/v1beta1/Gslb":
			gslb = u
		}
	}
	expected, err := selectTarget(&resource, defaults(cfg))
	if err == nil {
		_, err = siteTrafficDependencies(&resource, ingress, gslb, expected)
	}
	if err != nil {
		c.Eligible, c.Reason = false, err.Error()
	}
	return c
}
