package mutation

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type nativeProvider struct {
	client      dynamic.Interface
	geoTags     []string
	siteTraffic bool
}

func NewNativeProvider(client dynamic.Interface, allowedGeoTags []string) Provider {
	return &nativeProvider{client: client, geoTags: append([]string(nil), allowedGeoTags...)}
}

func NewNativeProviderWithSiteTraffic(client dynamic.Interface, allowedGeoTags []string) Provider {
	return &nativeProvider{client: client, geoTags: append([]string(nil), allowedGeoTags...), siteTraffic: true}
}

func (provider *nativeProvider) Operations() []string {
	if provider.siteTraffic {
		return []string{"create", "update", "excludeSite", "includeSite"}
	}
	return []string{"create", "update"}
}

func (provider *nativeProvider) ConfigurationFingerprint() string {
	tags := append([]string(nil), provider.geoTags...)
	sort.Strings(tags)
	data, _ := json.Marshal(tags)
	if provider.siteTraffic {
		return string(data) + "/native-site-v1"
	}
	return string(data)
}

func (provider *nativeProvider) Prepare(ctx context.Context, req v1.MutationRequest) (Prepared, error) {
	var prepared Prepared
	if provider.siteTraffic && (req.Operation == "excludeSite" || req.Operation == "includeSite") {
		return provider.prepareNativeSite(ctx, req)
	}
	if req.Operation != "create" && req.Operation != "update" {
		return prepared, Unsupported("native", req.Operation)
	}
	gvr, err := resourceGVR(req.Resource)
	if err != nil {
		return prepared, err
	}
	if gvr.Resource != "gslbs" || req.Traefik != nil || req.Traffic != nil || req.Policy == nil || provider.client == nil {
		return prepared, Failure(422, "invalid_native_request", "native requires a Gslb resource and typed policy, without Traefik options")
	}
	if err := ValidatePolicy(*req.Policy, provider.geoTags); err != nil {
		return prepared, err
	}
	prepared.GVR = gvr
	prepared.Fields = []string{"/spec/strategy/type", "/spec/strategy/primaryGeoTag", "/spec/strategy/dnsTtlSeconds", "/spec/strategy/weight"}
	prepared.Warnings = []string{"Controller reconciliation and DNS propagation are asynchronous."}
	current, err := provider.client.Resource(gvr).Namespace(req.Resource.Namespace).Get(ctx, req.Resource.Name, metav1.GetOptions{})
	if req.Operation == "create" {
		if err == nil {
			return prepared, Failure(409, "resource_exists", "Gslb already exists")
		}
		if !apierrors.IsNotFound(err) {
			return prepared, err
		}
		if req.Resource.UID != "" || req.Resource.ResourceVersion != "" || req.Target == nil {
			return prepared, Failure(422, "invalid_target", "create requires a pinned native Ingress or HTTPRoute target and an unallocated Gslb identity")
		}
		target := *req.Target
		targetGVR, err := resourceGVR(target)
		if err != nil {
			return prepared, err
		}
		if target.Namespace != req.Resource.Namespace || target.UID == "" || target.ResourceVersion == "" || targetGVR.Resource != "ingresses" && targetGVR.Resource != "httproutes" {
			return prepared, Failure(422, "invalid_target", "target must be a same-namespace pinned native Ingress or HTTPRoute")
		}
		live, err := provider.client.Resource(targetGVR).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return prepared, err
		}
		if !matchesReference(live, target, true) || live.GetDeletionTimestamp() != nil {
			return prepared, Failure(409, "stale_dependency", "target UID or resourceVersion changed or target is deleting")
		}
		if GeneratedChild(live) {
			return prepared, Failure(409, "owned_resource", "externally owned routing resources cannot be used as native targets")
		}
		if err := provider.checkNativeTargetAvailable(ctx, live, nil); err != nil {
			return prepared, err
		}
		prepared.Dependencies = []v1.ResourceReference{objectReference(live)}
		prepared.Warnings = append(prepared.Warnings, "Routing target availability is rechecked on apply, but concurrent Gslb creation is not atomic.")
		prepared.Desired = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": req.Resource.APIVersion, "kind": "Gslb",
			"metadata": map[string]interface{}{"name": req.Resource.Name, "namespace": req.Resource.Namespace},
			"spec":     map[string]interface{}{"resourceRef": map[string]interface{}{"apiVersion": target.APIVersion, "kind": target.Kind, "name": target.Name}},
		}}
		prepared.Fields = append(prepared.Fields, "/spec/resourceRef", "/spec/serviceHealthPolicy")
	} else {
		if err != nil {
			return prepared, err
		}
		if req.Target != nil {
			return prepared, Failure(422, "invalid_target", "update cannot change a routing target")
		}
		if !matchesReference(current, req.Resource, true) || current.GetDeletionTimestamp() != nil {
			return prepared, Failure(409, "stale_resource", "Gslb UID or resourceVersion changed or resource is deleting")
		}
		if GeneratedChild(current) {
			return prepared, Failure(409, "owned_resource", "edit the owning controller resource rather than its Gslb")
		}
		prepared.Current, prepared.Desired = current.DeepCopy(), current.DeepCopy()
	}
	strategy, _, err := unstructured.NestedMap(prepared.Desired.Object, "spec", "strategy")
	if err != nil {
		return prepared, Failure(422, "invalid_policy", "existing strategy is not an object")
	}
	if strategy == nil {
		strategy = map[string]interface{}{}
	}
	strategy["type"] = req.Policy.Strategy
	if req.Policy.Strategy == "weightedRoundRobin" {
		strategy["type"] = "roundRobin"
	}
	delete(strategy, "primaryGeoTag")
	delete(strategy, "weight")
	if req.Policy.PrimaryGeoTag != "" {
		strategy["primaryGeoTag"] = req.Policy.PrimaryGeoTag
	}
	if req.Policy.DNSTTLSeconds != 0 {
		strategy["dnsTtlSeconds"] = req.Policy.DNSTTLSeconds
	}
	if req.Policy.Strategy == "weightedRoundRobin" {
		weights := map[string]interface{}{}
		for tag, weight := range req.Policy.Weights {
			weights[tag] = weight
		}
		strategy["weight"] = weights
	}
	if err := unstructured.SetNestedMap(prepared.Desired.Object, strategy, "spec", "strategy"); err != nil {
		return prepared, err
	}
	return prepared, nil
}

func (provider *nativeProvider) ValidateAdmission(prepared Prepared, admitted *unstructured.Unstructured) error {
	if prepared.GVR.Resource == "ingresses" {
		for _, path := range prepared.Fields {
			wanted, wantedFound := jsonValue(prepared.Desired, path)
			got, gotFound := jsonValue(admitted, path)
			if wantedFound != gotFound || !reflect.DeepEqual(wanted, got) {
				return Failure(409, "unexpected_mutation", "admission changed the reviewed native site traffic operation")
			}
		}
		return nil
	}
	if prepared.Current == nil {
		wanted, _, _ := unstructured.NestedMap(prepared.Desired.Object, "spec", "resourceRef")
		got, _, _ := unstructured.NestedMap(admitted.Object, "spec", "resourceRef")
		if !reflect.DeepEqual(wanted, got) {
			return Failure(409, "unexpected_mutation", "admission changed the pinned native routing reference")
		}
		// The upstream CRD defaults this on create; it is not a writable policy.
		healthPolicy, found, err := unstructured.NestedString(admitted.Object, "spec", "serviceHealthPolicy")
		if err != nil || found && healthPolicy != "Any" {
			return Failure(409, "unexpected_mutation", "only the upstream serviceHealthPolicy Any create default is allowed")
		}
	}
	policy := v1.Policy{}
	var err error
	policy.Strategy, _, err = unstructured.NestedString(admitted.Object, "spec", "strategy", "type")
	if err != nil {
		return Failure(409, "unexpected_mutation", "admitted strategy type is invalid")
	}
	policy.PrimaryGeoTag, _, err = unstructured.NestedString(admitted.Object, "spec", "strategy", "primaryGeoTag")
	if err != nil {
		return Failure(409, "unexpected_mutation", "admitted primary GeoTag is invalid")
	}
	policy.DNSTTLSeconds, _, err = unstructured.NestedInt64(admitted.Object, "spec", "strategy", "dnsTtlSeconds")
	if err != nil {
		return Failure(409, "unexpected_mutation", "admitted DNS TTL is invalid")
	}
	weights, _, err := unstructured.NestedMap(admitted.Object, "spec", "strategy", "weight")
	if err != nil {
		return Failure(409, "unexpected_mutation", "admitted weights are invalid")
	}
	if len(weights) > 0 {
		policy.Weights = map[string]int64{}
		for tag, raw := range weights {
			weight, ok := raw.(int64)
			if !ok {
				return Failure(409, "unexpected_mutation", "admitted weight is not an integer")
			}
			policy.Weights[tag] = weight
		}
		if policy.Strategy == "roundRobin" {
			policy.Strategy = "weightedRoundRobin"
		}
	}
	return ValidatePolicy(policy, provider.geoTags)
}

// GeneratedChild identifies resources that are not native edit targets. Even a
// non-controller owner is treated conservatively, as in the management view.
func GeneratedChild(object *unstructured.Unstructured) bool {
	if object == nil {
		return false
	}
	if len(object.GetOwnerReferences()) > 0 {
		return true
	}
	for _, key := range []string{"routeOwnerRef", "route-owner-ref", "gslb.io/route-owner-ref", "k8gb.io/route-owner-ref", "k8gb-ui.io/route-uid", "k8gb-ui.io/managed-by", "app.k8gb.io/managed-by"} {
		if object.GetAnnotations()[key] != "" || object.GetLabels()[key] != "" {
			return true
		}
	}
	return object.GetLabels()["created-by"] == "gslb-controller" || object.GetAnnotations()["created-by"] == "gslb-controller"
}

// ReferencesNativeTarget shares conservative occupancy checks with the catalog.
// API versions within a group address the same resource. Optional UID/version
// fields do not free a named target for a second controller.
func ReferencesNativeTarget(gslb, target *unstructured.Unstructured) bool {
	if gslb == nil || target == nil || gslb.GetNamespace() != target.GetNamespace() {
		return false
	}
	ref, found, err := unstructured.NestedMap(gslb.Object, "spec", "resourceRef")
	if err != nil || !found || len(ref) == 0 {
		return false
	}
	kind, _, _ := unstructured.NestedString(ref, "kind")
	apiVersion, _, _ := unstructured.NestedString(ref, "apiVersion")
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil || gv.Version == "" || kind != target.GetKind() || gv.Group != target.GroupVersionKind().Group {
		return false
	}
	if kind != "Ingress" && kind != "HTTPRoute" {
		return false
	}
	if ns, _, _ := unstructured.NestedString(ref, "namespace"); ns != "" && ns != target.GetNamespace() {
		return false
	}
	name, _, _ := unstructured.NestedString(ref, "name")
	if name != "" {
		return name == target.GetName()
	}
	selector := &metav1.LabelSelector{}
	selectorFields := map[string]interface{}{}
	for _, key := range []string{"matchLabels", "matchExpressions"} {
		if value, ok := ref[key]; ok {
			selectorFields[key] = value
		}
	}
	if name == "" && len(selectorFields) == 0 {
		return false
	}
	data, err := json.Marshal(selectorFields)
	if err != nil || json.Unmarshal(data, selector) != nil {
		return true // An unreadable selector must not advertise a free target.
	}
	matcher, err := metav1.LabelSelectorAsSelector(selector)
	return err != nil || matcher.Matches(labels.Set(target.GetLabels()))
}

func (provider *nativeProvider) checkNativeTargetAvailable(ctx context.Context, target, allowed *unstructured.Unstructured) error {
	for _, group := range []string{"k8gb.io", "k8gb.absa.oss"} {
		gvr := schema.GroupVersionResource{Group: group, Version: "v1beta1", Resource: "gslbs"}
		options := metav1.ListOptions{Limit: 500}
		seen := map[string]bool{}
		for page := 0; ; page++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if page >= 100 {
				return Failure(503, "inventory_unavailable", "Gslb inventory exceeded the page limit; native target availability is unknown")
			}
			items, err := provider.client.Resource(gvr).Namespace(target.GetNamespace()).List(ctx, options)
			if apierrors.IsNotFound(err) && page == 0 {
				break // Either canonical or legacy Gslb API may be absent.
			}
			if err != nil {
				return err
			}
			for i := range items.Items {
				item := &items.Items[i]
				if item.GetNamespace() != target.GetNamespace() {
					return Failure(503, "inventory_unavailable", "Gslb inventory returned an out-of-scope object")
				}
				if allowed != nil && matchesReference(item, objectReference(allowed), false) && item.GetUID() == allowed.GetUID() {
					continue
				}
				if ReferencesNativeTarget(item, target) {
					return Failure(409, "target_in_use", fmt.Sprintf("routing target is already referenced by Gslb %s/%s (%s)", item.GetNamespace(), item.GetName(), group))
				}
			}
			if items.GetContinue() == "" {
				break
			}
			if seen[items.GetContinue()] {
				return Failure(503, "inventory_unavailable", "Gslb inventory repeated a continuation token")
			}
			seen[items.GetContinue()] = true
			options.Continue = items.GetContinue()
		}
	}
	return nil
}
