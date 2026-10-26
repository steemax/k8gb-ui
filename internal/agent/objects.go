package agent

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func str(object map[string]interface{}, fields ...string) string {
	s, _, _ := unstructured.NestedString(object, fields...)
	return s
}

func maps(object map[string]interface{}, fields ...string) []map[string]interface{} {
	raw, _, _ := unstructured.NestedSlice(object, fields...)
	result := make([]map[string]interface{}, 0, len(raw))
	for _, value := range raw {
		if m, ok := value.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result
}

func reference(item unstructured.Unstructured) v1.ResourceReference {
	return v1.ResourceReference{APIVersion: item.GetAPIVersion(), Kind: item.GetKind(), Namespace: item.GetNamespace(), Name: item.GetName(), UID: string(item.GetUID()), ResourceVersion: item.GetResourceVersion()}
}

func hostname(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func stringValues(raw interface{}) []string {
	result := []string{}
	switch value := raw.(type) {
	case string:
		result = append(result, strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })...)
	case []interface{}:
		for _, v := range value {
			if s, ok := v.(string); ok {
				result = append(result, s)
			}
		}
	case []string:
		result = append(result, value...)
	}
	return result
}

func unique(values []string) []string {
	set := map[string]bool{}
	for _, v := range values {
		if v != "" {
			set[v] = true
		}
	}
	result := make([]string, 0, len(set))
	for v := range set {
		result = append(result, v)
	}
	sort.Strings(result)
	return result
}

// resourceRef uses a namespace-bound unique name/label match, as documented at
// https://www.k8gb.io/master/resource_ref/. No arbitrary GVRs are constructed
// from untrusted objects. Unsupported providers remain visible through status.
func resolveTarget(gslb unstructured.Unstructured, data map[schema.GroupVersionResource]collection) (*unstructured.Unstructured, []string) {
	ref, found, _ := unstructured.NestedMap(gslb.Object, "spec", "resourceRef")
	if !found || len(ref) == 0 {
		return nil, nil
	}
	if ns := str(ref, "namespace"); ns != "" && ns != gslb.GetNamespace() {
		return nil, []string{"cross-namespace resourceRef is not read"}
	}
	var gvr schema.GroupVersionResource
	switch str(ref, "apiVersion") + "/" + str(ref, "kind") {
	case "networking.k8s.io/v1/Ingress":
		gvr = ingresses
	case "gateway.networking.k8s.io/v1/HTTPRoute":
		gvr = httpRoutes
	case "v1/Service":
		gvr = services
	default:
		return nil, []string{"unsupported resourceRef API version/kind; using observed status only"}
	}
	if !data[gvr].available {
		return nil, []string{"resourceRef API unavailable; target resolution is unknown"}
	}
	matchLabels, _, _ := unstructured.NestedStringMap(ref, "matchLabels")
	if expressions, _, _ := unstructured.NestedSlice(ref, "matchExpressions"); len(expressions) > 0 {
		return nil, []string{"resourceRef matchExpressions is unsupported in stage0"}
	}
	name := str(ref, "name")
	if name == "" && len(matchLabels) == 0 {
		return nil, []string{"resourceRef has no name or matchLabels"}
	}
	matches := []unstructured.Unstructured{}
	for _, item := range data[gvr].items {
		if item.GetNamespace() != gslb.GetNamespace() {
			continue
		}
		if name != "" && item.GetName() != name {
			continue
		}
		if !labels.SelectorFromSet(matchLabels).Matches(labels.Set(item.GetLabels())) {
			continue
		}
		if uid := str(ref, "uid"); uid != "" && string(item.GetUID()) != uid {
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) != 1 {
		return nil, []string{fmt.Sprintf("resourceRef resolved to %d objects; exactly one is required", len(matches))}
	}
	return &matches[0], nil
}

func buildObjects(gslb unstructured.Unstructured, data map[schema.GroupVersionResource]collection) []v1.Object {
	target, warnings := resolveTarget(gslb, data)
	hosts := []string{}
	raw, _, _ := unstructured.NestedFieldNoCopy(gslb.Object, "status", "hosts")
	hosts = append(hosts, stringValues(raw)...)
	health, _, _ := unstructured.NestedStringMap(gslb.Object, "status", "serviceHealth")
	for host := range health {
		hosts = append(hosts, host)
	}
	for _, server := range maps(gslb.Object, "status", "servers") {
		hosts = append(hosts, str(server, "host"))
	}
	native, nativeWarnings := nativeBackends(gslb, target)
	warnings = append(warnings, nativeWarnings...)
	nativeSite := mutation.NativeSiteTrafficState{}
	if target != nil {
		if _, hasSnapshot := target.GetAnnotations()[mutation.NativeSiteStateAnnotation]; hasSnapshot {
			var err error
			nativeSite, err = mutation.InspectNativeSiteTraffic(&gslb, target)
			if err == nil && nativeSite.Excluded {
				if !data[canonicalGSLB].available || !data[legacyGSLB].available || !data[services].available {
					err = fmt.Errorf("native exclusion cannot be confirmed without complete Gslb and Service inventories")
				}
				count := 0
				for _, gvr := range []schema.GroupVersionResource{canonicalGSLB, legacyGSLB} {
					for i := range data[gvr].items {
						if mutation.ReferencesNativeTarget(&data[gvr].items[i], target) {
							count++
						}
					}
				}
				if count != 1 {
					err = fmt.Errorf("native exclusion cannot be confirmed: Ingress is not exclusively referenced by one Gslb")
				}
				for _, service := range data[services].items {
					if service.GetNamespace() == target.GetNamespace() && service.GetName() == nativeSite.ServiceName {
						err = fmt.Errorf("native exclusion Service name collides with a real Service")
					}
				}
			}
			if err != nil {
				nativeSite.Excluded = false
				warnings = append(warnings, err.Error())
			}
		}
	}
	for host := range native {
		hosts = append(hosts, host)
	}
	if target != nil && target.GetKind() == "HTTPRoute" {
		raw, _, _ := unstructured.NestedFieldNoCopy(target.Object, "spec", "hostnames")
		hosts = append(hosts, stringValues(raw)...)
	}
	if annotated := gslb.GetAnnotations()["k8gb.io/hostname"]; annotated != "" {
		hosts = append(hosts, stringValues(annotated)...)
	}
	for i, host := range hosts {
		hosts[i] = hostname(host)
	}
	hosts = unique(hosts)
	if len(hosts) == 0 {
		hosts = []string{""}
		warnings = append(warnings, "hostname is not available in status or native routing data")
	}
	policy := v1.Policy{Strategy: str(gslb.Object, "spec", "strategy", "type"), PrimaryGeoTag: str(gslb.Object, "spec", "strategy", "primaryGeoTag")}
	policy.DNSTTLSeconds, _, _ = unstructured.NestedInt64(gslb.Object, "spec", "strategy", "dnsTtlSeconds")
	weights, _, _ := unstructured.NestedMap(gslb.Object, "spec", "strategy", "weight")
	if len(weights) > 0 {
		policy.Weights = map[string]int64{}
		for region, value := range weights {
			if n, ok := value.(int64); ok {
				policy.Weights[region] = n
			}
		}
		if len(policy.Weights) > 0 && policy.Strategy == "roundRobin" {
			policy.Strategy = "weightedRoundRobin"
		}
	}
	result := []v1.Object{}
	for _, host := range hosts {
		obj := v1.Object{Resource: reference(gslb), Hostname: host, GeoTag: str(gslb.Object, "status", "geoTag"), Policy: policy, Health: "Unknown", Backends: []v1.Backend{}, DNSTargets: []string{}, Warnings: append([]string{}, warnings...), Traffic: mutation.InspectTraffic(gslb.GetAnnotations())}
		for _, owner := range gslb.GetOwnerReferences() {
			if owner.Kind == "IngressRoute" && (owner.APIVersion == "traefik.io/v1alpha1" || owner.APIVersion == "traefik.containo.us/v1alpha1") {
				obj.Source = &v1.ResourceReference{APIVersion: owner.APIVersion, Kind: owner.Kind, Namespace: gslb.GetNamespace(), Name: owner.Name, UID: string(owner.UID)}
				break
			}
		}
		obj.Management = "native"
		if mutation.GeneratedChild(&gslb) || len(gslb.GetOwnerReferences()) > 0 {
			obj.Management = "external"
		}
		if obj.Source != nil && obj.Source.UID != "" && gslb.GetLabels()[traefik.ManagedLabel] == "traefik-adapter" && gslb.GetLabels()[traefik.OwnerLabel] == obj.Source.UID {
			obj.Management = "traefik"
		}
		if target != nil {
			ref := reference(*target)
			obj.Target = &ref
		}
		// Only our owned generated route is intentionally excluded. A free-form
		// annotation on a native live Ingress must never hide real backend faults.
		obj.Traffic.Excluded = obj.Traffic.Excluded && obj.Source != nil && target != nil && target.GetAnnotations()[mutation.SiteExcludedAnnotation] == "true" &&
			obj.Source.UID != "" && gslb.GetLabels()[traefik.ManagedLabel] == "traefik-adapter" && target.GetLabels()[traefik.ManagedLabel] == "traefik-adapter" &&
			gslb.GetLabels()[traefik.OwnerLabel] == obj.Source.UID && target.GetLabels()[traefik.OwnerLabel] == obj.Source.UID
		if obj.Traffic.Excluded {
			matchedBackend := false
			for _, backend := range native[host] {
				matchedBackend = matchedBackend || backend.name == traefik.ExcludedServiceName(obj.Source.UID)
			}
			if !matchedBackend {
				obj.Traffic.Excluded = false
				obj.Warnings = append(obj.Warnings, "Exclusion marker and generated routing backend disagree; inspect reconciliation")
			}
			for _, service := range data[services].items {
				if service.GetNamespace() == gslb.GetNamespace() && service.GetName() == traefik.ExcludedServiceName(obj.Source.UID) {
					obj.Traffic.Excluded = false
					obj.Warnings = append(obj.Warnings, "Exclusion Service name collides with an existing Service; inspect the live route")
				}
			}
		}
		intentionalService := ""
		if obj.Traffic.Excluded {
			intentionalService = traefik.ExcludedServiceName(obj.Source.UID)
		}
		if nativeSite.Excluded && len(native[host]) > 0 {
			obj.Traffic.Excluded = true
			intentionalService = nativeSite.ServiceName
			obj.Warnings = append(obj.Warnings, mutation.NativeSiteLocalWarning)
		}
		for h, value := range health {
			if hostname(h) == host && value != "" {
				obj.Health = value
			}
		}
		if obj.Traffic.Excluded {
			obj.Health = "Excluded"
		}
		obj.DNSTargets, obj.DNSObserved = resolveDNS(gslb, target, host, data[dnsEndpoints])
		if !data[dnsEndpoints].available {
			obj.Warnings = append(obj.Warnings, "DNSEndpoint data unavailable; DNS publication is unknown")
		}
		refs := append([]backendRef{}, native[host]...)
		for _, server := range maps(gslb.Object, "status", "servers") {
			if hostname(str(server, "host")) != host {
				continue
			}
			for _, service := range maps(server, "services") {
				ns := str(service, "namespace")
				if ns == "" {
					ns = gslb.GetNamespace()
				}
				name := str(service, "name")
				if name == "" {
					continue
				}
				matched := false
				for _, ref := range refs {
					if ref.namespace == ns && ref.name == name {
						matched = true
					}
				}
				if !matched {
					refs = append(refs, backendRef{namespace: ns, name: name})
				}
			}
		}
		if target != nil && target.GetKind() == "Service" {
			refs = append(refs, backendRef{namespace: target.GetNamespace(), name: target.GetName()})
		}
		seen := map[backendRef]bool{}
		for _, ref := range refs {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			backend := v1.Backend{Namespace: ref.namespace, Service: ref.name, Port: ref.port, Health: "Unknown"}
			if ref.namespace != gslb.GetNamespace() {
				obj.Warnings = append(obj.Warnings, "cross-namespace backend is not read: "+ref.namespace+"/"+ref.name)
			} else {
				backend = backendStatus(backend, data[endpointSlices], data[services])
			}
			if obj.Traffic.Excluded && backend.Service == intentionalService && backend.Namespace == gslb.GetNamespace() {
				backend.Health, backend.EndpointDataAvailable = "Excluded", false
				backend.ReadyEndpoints, backend.TotalEndpoints = 0, 0
			}
			if nativeSite.Excluded && backend.Service != intentionalService && (backend.Health == "NotFound" || backend.Health == "Unhealthy") {
				obj.Health = backend.Health
				obj.Warnings = append(obj.Warnings, "Real backend fault remains while native site is excluded: "+backend.Namespace+"/"+backend.Service)
			}
			obj.Backends = append(obj.Backends, backend)
		}
		sort.Slice(obj.Backends, func(i, j int) bool {
			a, b := obj.Backends[i], obj.Backends[j]
			return a.Namespace+"/"+a.Service+"/"+a.Port < b.Namespace+"/"+b.Service+"/"+b.Port
		})
		obj.Warnings = unique(obj.Warnings)
		result = append(result, obj)
	}
	return result
}

func resolveDNS(gslb unstructured.Unstructured, target *unstructured.Unstructured, host string, data collection) ([]string, bool) {
	targets, observed := []string{}, false
	if !data.available || host == "" {
		return targets, false
	}
	for _, item := range data.items {
		if item.GetNamespace() != gslb.GetNamespace() {
			continue
		}
		linked := owns(gslb, item)
		if target != nil {
			linked = linked || owns(*target, item)
		}
		// Same-name is the older controller's convention. Never let it override
		// an explicit owner pointing at a different Gslb/route.
		if len(item.GetOwnerReferences()) == 0 {
			linked = linked || item.GetName() == gslb.GetName() || (target != nil && item.GetName() == target.GetName())
		}
		if !linked {
			continue
		}
		for _, endpoint := range maps(item.Object, "spec", "endpoints") {
			dnsName := hostname(str(endpoint, "dnsName"))
			if dnsName != host || dnsName == "localtargets" || strings.HasPrefix(dnsName, "localtargets.") {
				continue
			}
			switch str(endpoint, "recordType") {
			case "A", "AAAA", "CNAME":
			default:
				continue
			}
			observed = true
			raw, _, _ := unstructured.NestedFieldNoCopy(endpoint, "targets")
			for _, value := range stringValues(raw) {
				if value = strings.TrimSpace(value); value != "" {
					targets = append(targets, value)
				}
			}
		}
	}
	return unique(targets), observed
}

type backendRef struct{ namespace, name, port string }

func nativeBackends(gslb unstructured.Unstructured, target *unstructured.Unstructured) (map[string][]backendRef, []string) {
	result := map[string][]backendRef{}
	if target != nil && target.GetKind() == "HTTPRoute" {
		raw, _, _ := unstructured.NestedFieldNoCopy(target.Object, "spec", "hostnames")
		hosts := stringValues(raw)
		warnings := []string{"HTTPRoute backends are declared references only; parent attachment, filters, weights and ReferenceGrant acceptance are not evaluated in stage0"}
		for _, rule := range maps(target.Object, "spec", "rules") {
			for _, ref := range maps(rule, "backendRefs") {
				kind, group := str(ref, "kind"), str(ref, "group")
				if (kind != "" && kind != "Service") || group != "" {
					warnings = append(warnings, "non-core-Service HTTPRoute backend is unsupported")
					continue
				}
				name := str(ref, "name")
				if name == "" {
					continue
				}
				ns := str(ref, "namespace")
				if ns == "" {
					ns = target.GetNamespace()
				}
				port, _, _ := unstructured.NestedInt64(ref, "port")
				portText := ""
				if port > 0 {
					portText = strconv.FormatInt(port, 10)
				}
				for _, host := range hosts {
					result[hostname(host)] = append(result[hostname(host)], backendRef{namespace: ns, name: name, port: portText})
				}
			}
		}
		return result, warnings
	}
	var spec map[string]interface{}
	if target != nil && target.GetKind() == "Ingress" {
		spec, _, _ = unstructured.NestedMap(target.Object, "spec")
	} else if ref, _, _ := unstructured.NestedMap(gslb.Object, "spec", "resourceRef"); len(ref) == 0 {
		spec, _, _ = unstructured.NestedMap(gslb.Object, "spec", "ingress")
	}
	for _, rule := range maps(spec, "rules") {
		host := hostname(str(rule, "host"))
		if host == "" {
			continue
		}
		if _, ok := result[host]; !ok {
			result[host] = []backendRef{}
		}
		for _, path := range maps(rule, "http", "paths") {
			name := str(path, "backend", "service", "name")
			if name == "" {
				continue
			}
			port := str(path, "backend", "service", "port", "name")
			if n, ok, _ := unstructured.NestedInt64(path, "backend", "service", "port", "number"); ok {
				port = strconv.FormatInt(n, 10)
			}
			result[host] = append(result[host], backendRef{namespace: gslb.GetNamespace(), name: name, port: port})
		}
	}
	return result, nil
}

func backendStatus(backend v1.Backend, slices, serviceData collection) v1.Backend {
	if !slices.available {
		return backend
	}
	backend.EndpointDataAvailable = true
	for _, slice := range slices.items {
		if slice.GetNamespace() != backend.Namespace || slice.GetLabels()["kubernetes.io/service-name"] != backend.Service {
			continue
		}
		for _, endpoint := range maps(slice.Object, "endpoints") {
			backend.TotalEndpoints++
			ready, known, _ := unstructured.NestedBool(endpoint, "conditions", "ready")
			terminating, _, _ := unstructured.NestedBool(endpoint, "conditions", "terminating")
			if (!known || ready) && !terminating {
				backend.ReadyEndpoints++
			}
		}
	}
	if backend.ReadyEndpoints > 0 {
		backend.Health = "Healthy"
		return backend
	}
	if backend.TotalEndpoints > 0 {
		backend.Health = "Unhealthy"
		return backend
	}
	// Empty/missing EndpointSlices do not establish whether a Service exists.
	if serviceData.available {
		backend.Health = "NotFound"
		for _, service := range serviceData.items {
			if service.GetNamespace() == backend.Namespace && service.GetName() == backend.Service {
				backend.Health = "Unhealthy"
				break
			}
		}
	}
	return backend
}
