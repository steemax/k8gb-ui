package mutation

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	v1 "k8gb-ui/api/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidatePolicy validates the public form policy, before provider conversion.
// GeoTags must be explicitly configured; no cluster or domain defaults are used.
func ValidatePolicy(policy v1.Policy, allowedGeoTags []string) error {
	invalid := func(message string) error { return Failure(422, "invalid_policy", message) }
	if len(allowedGeoTags) == 0 {
		return invalid("allowed GeoTags must be explicitly configured")
	}
	tags := map[string]bool{}
	for _, tag := range allowedGeoTags {
		if !boundedText(tag, 63) || strings.ContainsAny(tag, ",= ") {
			return invalid("configured GeoTag is invalid")
		}
		tags[tag] = true
	}
	switch policy.Strategy {
	case "roundRobin", "failover", "geoip", "weightedRoundRobin":
	default:
		return invalid("strategy must be roundRobin, failover, geoip or weightedRoundRobin")
	}
	if policy.DNSTTLSeconds < 0 || policy.DNSTTLSeconds > 2147483647 {
		return invalid("DNS TTL must be omitted or between 1 and 2147483647 seconds")
	}
	if policy.PrimaryGeoTag != "" && !tags[policy.PrimaryGeoTag] || policy.Strategy == "failover" && policy.PrimaryGeoTag == "" {
		return invalid("primary GeoTag must belong to the configured allowlist; failover requires it")
	}
	if policy.Strategy != "weightedRoundRobin" && len(policy.Weights) != 0 {
		return invalid("weights require weightedRoundRobin")
	}
	positive := false
	for tag, weight := range policy.Weights {
		if !tags[tag] || weight < 0 || weight > 2147483647 {
			return invalid("weights must be nonnegative integers for allowed GeoTags")
		}
		positive = positive || weight > 0
	}
	if policy.Strategy == "weightedRoundRobin" && !positive {
		return invalid("weightedRoundRobin requires at least one positive weight")
	}
	return nil
}

func boundedText(value string, limit int) bool {
	return len(value) <= limit && strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}

// Explicit mappings avoid pluralization, discovery fallback, or arbitrary GVRs.
func resourceGVR(ref v1.ResourceReference) (schema.GroupVersionResource, error) {
	var gvr schema.GroupVersionResource
	switch ref.APIVersion + "/" + ref.Kind {
	case "k8gb.io/v1beta1/Gslb":
		gvr = schema.GroupVersionResource{Group: "k8gb.io", Version: "v1beta1", Resource: "gslbs"}
	case "k8gb.absa.oss/v1beta1/Gslb":
		gvr = schema.GroupVersionResource{Group: "k8gb.absa.oss", Version: "v1beta1", Resource: "gslbs"}
	case "networking.k8s.io/v1/Ingress":
		gvr = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	case "gateway.networking.k8s.io/v1/HTTPRoute":
		gvr = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	case "traefik.io/v1alpha1/IngressRoute":
		gvr = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
	case "traefik.containo.us/v1alpha1/IngressRoute":
		gvr = schema.GroupVersionResource{Group: "traefik.containo.us", Version: "v1alpha1", Resource: "ingressroutes"}
	default:
		return gvr, Failure(422, "unsupported_resource", "unsupported exact API version and kind")
	}
	if len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 {
		return gvr, Failure(422, "invalid_resource", "a valid explicit namespace and resource name are required")
	}
	if len(ref.UID) > 128 || len(ref.ResourceVersion) > 128 {
		return gvr, Failure(422, "invalid_resource", "UID or resourceVersion is too long")
	}
	return gvr, nil
}

// Cluster-scoped objects are read-only dependencies, never mutation targets.
func dependencyGVR(ref v1.ResourceReference, namespace string) (schema.GroupVersionResource, error) {
	var gvr schema.GroupVersionResource
	switch ref.APIVersion + "/" + ref.Kind {
	case "traefik.io/v1alpha1/TLSOption", "traefik.containo.us/v1alpha1/TLSOption":
		if len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || len(ref.UID) > 128 || len(ref.ResourceVersion) > 128 {
			return gvr, Failure(422, "invalid_dependency", "invalid TLSOption dependency")
		}
		return schema.GroupVersionResource{Group: strings.TrimSuffix(ref.APIVersion, "/v1alpha1"), Version: "v1alpha1", Resource: "tlsoptions"}, nil
	case "apps/v1/Deployment":
		if len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || len(ref.UID) > 128 || len(ref.ResourceVersion) > 128 {
			return gvr, Failure(422, "invalid_dependency", "invalid controller Deployment dependency")
		}
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, nil
	case "v1/Service":
		if ref.Namespace != namespace || len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || len(ref.UID) > 128 || len(ref.ResourceVersion) > 128 {
			return gvr, Failure(422, "invalid_dependency", "invalid namespaced service dependency")
		}
		return schema.GroupVersionResource{Version: "v1", Resource: "services"}, nil
	case "admissionregistration.k8s.io/v1/MutatingWebhookConfiguration":
		gvr = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	case "v1/Namespace":
		if ref.Name != namespace {
			return gvr, Failure(422, "invalid_dependency", "namespace dependency must identify the target namespace")
		}
		gvr = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	default:
		if ref.Namespace != namespace {
			return gvr, Failure(422, "invalid_dependency", "namespaced dependency must be in the target namespace")
		}
		return resourceGVR(ref)
	}
	if ref.Namespace != "" || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || len(ref.UID) > 128 || len(ref.ResourceVersion) > 128 {
		return gvr, Failure(422, "invalid_dependency", "invalid cluster-scoped dependency identity")
	}
	return gvr, nil
}

func objectReference(object *unstructured.Unstructured) v1.ResourceReference {
	return v1.ResourceReference{APIVersion: object.GetAPIVersion(), Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion()}
}

func matchesReference(object *unstructured.Unstructured, ref v1.ResourceReference, pinned bool) bool {
	if object == nil {
		return false
	}
	got := objectReference(object)
	return got.APIVersion == ref.APIVersion && got.Kind == ref.Kind && got.Namespace == ref.Namespace && got.Name == ref.Name && (!pinned || ref.UID != "" && ref.ResourceVersion != "" && got.UID == ref.UID && got.ResourceVersion == ref.ResourceVersion)
}

func pointerParts(path string) ([]string, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("invalid JSON pointer")
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			if part[j] == '~' {
				if j+1 == len(part) || part[j+1] != '0' && part[j+1] != '1' {
					return nil, fmt.Errorf("invalid JSON pointer escape")
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		if parts[i] == "" {
			return nil, fmt.Errorf("empty JSON pointer segment")
		}
	}
	if len(parts) < 2 || parts[0] != "spec" && !(parts[0] == "metadata" && parts[1] == "annotations" && len(parts) >= 2) {
		return nil, fmt.Errorf("provider fields must be spec children or annotations")
	}
	return parts, nil
}

func jsonValue(object *unstructured.Unstructured, path string) (interface{}, bool) {
	if object == nil {
		return nil, false
	}
	parts, _ := pointerParts(path)
	value, found, _ := unstructured.NestedFieldCopy(object.Object, parts...)
	return value, found
}

func rawValue(value interface{}) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
