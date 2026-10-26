package mutation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kjson "k8s.io/apimachinery/pkg/util/json"
)

const (
	NativeSiteStateAnnotation = "k8gb-ui.io/native-site-state"
	NativeSiteLocalWarning    = "Excluding this site changes the EXISTING Ingress: LOCAL Ingress routing will also stop working for ALL its hosts and HTTP paths until includeSite restores it."
	nativeSiteSnapshotLimit   = 48 * 1024
	nativeSiteServicePrefix   = "k8gb-ui-native-excluded-"
)

type nativeSiteSnapshot struct {
	Version    int             `json:"version"`
	GslbUID    string          `json:"gslbUID"`
	IngressUID string          `json:"ingressUID"`
	Rules      json.RawMessage `json:"rules"`
	Excluded   *string         `json:"excluded"`
}

// NativeSiteTrafficState is derived from the live Ingress, never Gslb health.
type NativeSiteTrafficState struct {
	Excluded         bool
	ServiceName      string
	OriginalServices []string
}

func NativeSiteServiceName(gslbUID string) string {
	hash := sha256.Sum256([]byte(gslbUID))
	return nativeSiteServicePrefix + hex.EncodeToString(hash[:10])
}

func nativeSiteConflict(message string) error { return Failure(409, "native_site_conflict", message) }

func validateNativeSiteBinding(gslb, ingress *unstructured.Unstructured) error {
	if gslb == nil || ingress == nil || ingress.GetAPIVersion() != "networking.k8s.io/v1" || ingress.GetKind() != "Ingress" || gslb.GetKind() != "Gslb" || (gslb.GetAPIVersion() != "k8gb.io/v1beta1" && gslb.GetAPIVersion() != "k8gb.absa.oss/v1beta1") {
		return Failure(422, "unsupported_native_site", "native site traffic supports only standalone Gslb resourceRef by name to a networking.k8s.io/v1 Ingress; HTTPRoute and inline ingress are unsupported")
	}
	if GeneratedChild(gslb) || GeneratedChild(ingress) {
		return Failure(409, "owned_resource", "native site traffic cannot change webhook-generated or externally owned resources")
	}
	if gslb.GetUID() == "" || ingress.GetUID() == "" || gslb.GetNamespace() != ingress.GetNamespace() || gslb.GetDeletionTimestamp() != nil || ingress.GetDeletionTimestamp() != nil {
		return nativeSiteConflict("native site binding has missing identities, a different namespace or a deleting resource")
	}
	ref, found, err := unstructured.NestedMap(gslb.Object, "spec", "resourceRef")
	if err != nil || !found || len(ref) == 0 {
		return Failure(422, "unsupported_native_site", "native site traffic requires an explicit named Ingress resourceRef, not inline ingress")
	}
	for _, key := range []string{"matchLabels", "matchExpressions"} {
		if _, exists := ref[key]; exists {
			return Failure(422, "unsupported_native_site", "native site traffic does not support selector resourceRefs")
		}
	}
	version, _, _ := unstructured.NestedString(ref, "apiVersion")
	kind, _, _ := unstructured.NestedString(ref, "kind")
	name, _, _ := unstructured.NestedString(ref, "name")
	ns, _, _ := unstructured.NestedString(ref, "namespace")
	uid, _, _ := unstructured.NestedString(ref, "uid")
	if version != ingress.GetAPIVersion() || kind != "Ingress" || name != ingress.GetName() || ns != "" && ns != ingress.GetNamespace() || uid != "" && uid != string(ingress.GetUID()) {
		return nativeSiteConflict("Gslb resourceRef does not identify this exact live Ingress")
	}
	// The canonical CRD defaults resourceRef objects to ingress: {}.
	// Only an actual inline routing configuration conflicts with resourceRef.
	if inline, _, err := unstructured.NestedMap(gslb.Object, "spec", "ingress"); err != nil || len(inline) > 0 {
		return Failure(422, "unsupported_native_site", "mixed inline ingress and resourceRef is unsupported")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(ingress.Object, "spec", "defaultBackend"); found {
		return Failure(422, "unsupported_native_site", "Ingress defaultBackend is unsupported; only explicit HTTP Service paths can be excluded")
	}
	return nil
}

// Rebuild from the original rules each time, changing only Service names.
func nativeSiteRules(original []interface{}, service string) ([]interface{}, []string, error) {
	invalid := func() ([]interface{}, []string, error) {
		return nil, nil, Failure(422, "unsupported_native_site", "every Ingress rule must have a hostname and HTTP paths with named Service backends and valid ports; resource backends are unsupported")
	}
	if len(original) == 0 {
		return invalid()
	}
	rules := runtime.DeepCopyJSONValue(original).([]interface{})
	names := map[string]bool{}
	for _, raw := range rules {
		rule, ok := raw.(map[string]interface{})
		if !ok {
			return invalid()
		}
		host, _, _ := unstructured.NestedString(rule, "host")
		paths, found, err := unstructured.NestedSlice(rule, "http", "paths")
		if host == "" || err != nil || !found || len(paths) == 0 {
			return invalid()
		}
		for _, rawPath := range paths {
			path, ok := rawPath.(map[string]interface{})
			if !ok {
				return invalid()
			}
			backend, _, err := unstructured.NestedMap(path, "backend")
			if err != nil || backend == nil {
				return invalid()
			}
			if _, exists := backend["resource"]; exists {
				return invalid()
			}
			name, _, _ := unstructured.NestedString(backend, "service", "name")
			port, found, err := unstructured.NestedMap(backend, "service", "port")
			if name == "" || strings.HasPrefix(name, nativeSiteServicePrefix) || !found || err != nil {
				return invalid()
			}
			portName, hasName, nameErr := unstructured.NestedString(port, "name")
			portNumber, hasNumber, numberErr := unstructured.NestedInt64(port, "number")
			if nameErr != nil || numberErr != nil || hasName == hasNumber || hasName && portName == "" || hasNumber && (portNumber < 1 || portNumber > 65535) {
				return invalid()
			}
			names[name] = true
			if err := unstructured.SetNestedField(path, service, "backend", "service", "name"); err != nil {
				return nil, nil, err
			}
		}
		if err := unstructured.SetNestedSlice(rule, paths, "http", "paths"); err != nil {
			return nil, nil, err
		}
	}
	services := make([]string, 0, len(names))
	for name := range names {
		services = append(services, name)
	}
	sort.Strings(services)
	if len(services) > 64 {
		return nil, nil, Failure(422, "unsupported_native_site", "too many distinct Service backends to pin safely")
	}
	return rules, services, nil
}

func readNativeSiteSnapshot(ingress *unstructured.Unstructured) (*nativeSiteSnapshot, error) {
	raw, found := ingress.GetAnnotations()[NativeSiteStateAnnotation]
	if !found {
		return nil, nil
	}
	if len(raw) == 0 || len(raw) > nativeSiteSnapshotLimit {
		return nil, nativeSiteConflict("invalid or oversized native site snapshot")
	}
	state := &nativeSiteSnapshot{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(state) != nil || decoder.Decode(new(interface{})) != io.EOF || state.Version != 1 || state.GslbUID == "" || state.IngressUID != string(ingress.GetUID()) || len(state.Rules) == 0 || state.Excluded != nil && len(*state.Excluded) > 4096 {
		return nil, nativeSiteConflict("malformed, unsupported or mismatched native site snapshot")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields["excluded"] == nil {
		return nil, nativeSiteConflict("native snapshot must explicitly record whether the original exclusion marker was absent")
	}
	return state, nil
}

func InspectNativeSiteTraffic(gslb, ingress *unstructured.Unstructured) (NativeSiteTrafficState, error) {
	var result NativeSiteTrafficState
	if err := validateNativeSiteBinding(gslb, ingress); err != nil {
		return result, err
	}
	result.ServiceName = NativeSiteServiceName(string(gslb.GetUID()))
	state, err := readNativeSiteSnapshot(ingress)
	if err != nil {
		return result, err
	}
	live, found, err := unstructured.NestedSlice(ingress.Object, "spec", "rules")
	if err != nil || !found {
		return result, nativeSiteConflict("Ingress rules are missing or invalid")
	}
	var original []interface{}
	if state != nil {
		if state.GslbUID != string(gslb.GetUID()) || kjson.Unmarshal(state.Rules, &original) != nil {
			return result, nativeSiteConflict("native snapshot is bound to another Gslb UID or has invalid rules")
		}
	} else {
		original = live
	}
	applied, names, err := nativeSiteRules(original, result.ServiceName)
	if err != nil {
		if state != nil {
			return result, nativeSiteConflict("native snapshot has unsupported original rules")
		}
		return result, err
	}
	result.OriginalServices = names
	if state != nil {
		if ingress.GetAnnotations()[SiteExcludedAnnotation] != "true" || !reflect.DeepEqual(live, applied) {
			return result, nativeSiteConflict("native exclusion rules or marker changed outside this operation; refusing to restore")
		}
		result.Excluded = true
	}
	return result, nil
}

func (provider *nativeProvider) prepareNativeSite(ctx context.Context, req v1.MutationRequest) (Prepared, error) {
	var prepared Prepared
	if req.Provider != "native" || req.Policy != nil || req.Traefik != nil || req.Traffic != nil || req.Target == nil || provider.client == nil || req.Resource.APIVersion != "networking.k8s.io/v1" || req.Resource.Kind != "Ingress" || req.Resource.UID == "" || req.Resource.ResourceVersion == "" {
		return prepared, Failure(422, "invalid_native_site_request", "native site operations require only a fresh pinned Ingress resource and a same-namespace standalone Gslb target with UID; no policy, Traefik or Traffic payload")
	}
	gvr, err := resourceGVR(req.Resource)
	if err != nil {
		return prepared, err
	}
	targetGVR, err := resourceGVR(*req.Target)
	if err != nil {
		return prepared, err
	}
	if targetGVR.Resource != "gslbs" || req.Target.Namespace != req.Resource.Namespace || req.Target.UID == "" || req.Target.ConfigurationDigest != "" {
		return prepared, Failure(422, "invalid_native_site_request", "target must identify a same-namespace standalone Gslb with an observed UID")
	}
	ingress, err := provider.client.Resource(gvr).Namespace(req.Resource.Namespace).Get(ctx, req.Resource.Name, metav1.GetOptions{})
	if err != nil {
		return prepared, err
	}
	if !matchesReference(ingress, req.Resource, true) || ingress.GetDeletionTimestamp() != nil {
		return prepared, Failure(409, "stale_resource", "Ingress UID/resourceVersion changed or it is deleting")
	}
	gslb, err := provider.client.Resource(targetGVR).Namespace(req.Target.Namespace).Get(ctx, req.Target.Name, metav1.GetOptions{})
	if err != nil {
		return prepared, err
	}
	if !matchesReference(gslb, *req.Target, false) || string(gslb.GetUID()) != req.Target.UID || gslb.GetResourceVersion() == "" {
		return prepared, Failure(409, "stale_dependency", "Gslb UID changed or live identity is invalid")
	}
	state, err := InspectNativeSiteTraffic(gslb, ingress)
	if err != nil {
		return prepared, err
	}
	if err := provider.checkNativeTargetAvailable(ctx, ingress, gslb); err != nil {
		return prepared, err
	}
	serviceGVR := schema.GroupVersionResource{Version: "v1", Resource: "services"}
	_, err = provider.client.Resource(serviceGVR).Namespace(ingress.GetNamespace()).Get(ctx, state.ServiceName, metav1.GetOptions{})
	if err == nil {
		return prepared, nativeSiteConflict("synthetic exclusion Service exists; refusing to hide or modify a real Service")
	}
	if !apierrors.IsNotFound(err) {
		return prepared, err
	}
	prepared = Prepared{GVR: gvr, Current: ingress.DeepCopy(), Desired: ingress.DeepCopy(), Dependencies: []v1.ResourceReference{objectReference(gslb)}, Fields: []string{"/spec/rules", annotationPath(NativeSiteStateAnnotation), annotationPath(SiteExcludedAnnotation)}, Warnings: []string{NativeSiteLocalWarning, "No Service or EndpointSlice is changed. Gslb policy and IP overrides are preserved. DNS withdrawal still depends on controller reconciliation and DNS cache expiry; this write does not confirm convergence.", "Gslb reference exclusivity and synthetic Service absence are rechecked on apply, but cross-resource changes are not atomic."}}
	a := prepared.Desired.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if req.Operation == "includeSite" {
		if !state.Excluded {
			return prepared, Failure(409, "unmanaged_exclusion", "no valid native snapshot exists; refusing to change an unmanaged or broken backend")
		}
		snapshot, err := readNativeSiteSnapshot(ingress)
		if err != nil {
			return prepared, err
		}
		var rules []interface{}
		if err := kjson.Unmarshal(snapshot.Rules, &rules); err != nil {
			return prepared, err
		}
		if err := unstructured.SetNestedSlice(prepared.Desired.Object, rules, "spec", "rules"); err != nil {
			return prepared, err
		}
		restoreAnnotation(a, SiteExcludedAnnotation, snapshot.Excluded)
		delete(a, NativeSiteStateAnnotation)
	} else if !state.Excluded {
		// Pin real Service identities on first exclude; never snapshot an already
		// missing backend and subsequently call that unmanaged fault intentional.
		for _, name := range state.OriginalServices {
			service, err := provider.client.Resource(serviceGVR).Namespace(ingress.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return prepared, err
			}
			if service.GetUID() == "" || service.GetResourceVersion() == "" || service.GetDeletionTimestamp() != nil {
				return prepared, nativeSiteConflict("original Service is unpinned or deleting")
			}
			prepared.Dependencies = append(prepared.Dependencies, objectReference(service))
		}
		rules, _, _ := unstructured.NestedSlice(ingress.Object, "spec", "rules")
		rawRules, err := json.Marshal(rules)
		if err != nil {
			return prepared, err
		}
		snapshot := nativeSiteSnapshot{Version: 1, GslbUID: string(gslb.GetUID()), IngressUID: string(ingress.GetUID()), Rules: rawRules, Excluded: annotationValue(a, SiteExcludedAnnotation)}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return prepared, err
		}
		if len(raw) > nativeSiteSnapshotLimit || snapshot.Excluded != nil && len(*snapshot.Excluded) > 4096 {
			return prepared, Failure(422, "snapshot_too_large", "original Ingress rules/marker exceed the native snapshot limit")
		}
		applied, _, err := nativeSiteRules(rules, state.ServiceName)
		if err != nil {
			return prepared, err
		}
		if err := unstructured.SetNestedSlice(prepared.Desired.Object, applied, "spec", "rules"); err != nil {
			return prepared, err
		}
		a[NativeSiteStateAnnotation], a[SiteExcludedAnnotation] = string(raw), "true"
	}
	prepared.Desired.SetAnnotations(a)
	// Keep the aggregate Kubernetes annotation limit below 256 KiB as well.
	size := 0
	for key, value := range a {
		size += len(key) + len(value)
	}
	if size > 256*1024 {
		return prepared, Failure(422, "snapshot_too_large", "native snapshot would exceed the Kubernetes annotation limit")
	}
	return prepared, nil
}
