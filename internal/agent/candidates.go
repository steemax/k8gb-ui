package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (c *Collector) Catalog(ctx context.Context, namespace string, cfg *traefik.Config, caps v1.Capabilities) (v1.Catalog, error) {
	result := v1.Catalog{APIVersion: v1.Version, ClusterID: c.opts.ClusterID, Namespace: namespace, Capabilities: caps, Candidates: []v1.Candidate{}, Warnings: []string{}}
	if err := c.validateNamespace(namespace); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if c.opts.DNSZones != nil {
		zones, _, err := c.opts.DNSZones.Read(ctx)
		result.DNSZones = &zones
		if err != nil {
			result.Warnings = append(result.Warnings, "DNS zones unavailable: "+err.Error())
		}
	}
	if cfg != nil {
		result.TraefikDomains = traefik.Domains(*cfg)
		security := traefik.SecurityCatalog(ctx, c.client, namespace, *cfg)
		result.TraefikSecurity = &security
	}
	var generated []unstructured.Unstructured
	var gslbItems []unstructured.Unstructured
	inventoryComplete, inventoryAvailable := true, false
	for _, gvr := range []schema.GroupVersionResource{canonicalGSLB, legacyGSLB} {
		items, err := c.list(ctx, gvr, namespace)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				inventoryComplete = false
				result.Warnings = append(result.Warnings, "Gslb inventory unavailable: "+gvr.Group)
			}
			continue
		}
		inventoryAvailable = true
		gslbItems = append(gslbItems, items...)
	}
	if !inventoryAvailable {
		result.Warnings = append(result.Warnings, "No readable Gslb inventory; native target availability cannot be confirmed")
	}
	generated = append(generated, gslbItems...)
	siteEnabled := false
	for _, operation := range caps.Operations["native"] {
		if operation == "excludeSite" {
			siteEnabled = true
		}
	}
	var siteServices []unstructured.Unstructured
	siteServicesRead, siteServicesAvailable := false, false
	for _, gvr := range []schema.GroupVersionResource{ingresses, httpRoutes} {
		items, err := c.list(ctx, gvr, namespace)
		if err != nil {
			if gvr != httpRoutes || !apierrors.IsNotFound(err) {
				result.Warnings = append(result.Warnings, "Routing API unavailable: "+gvr.Resource)
			}
			continue
		}
		for _, item := range items {
			if gvr == ingresses {
				generated = append(generated, item)
			}
			if mutation.GeneratedChild(&item) {
				continue
			}
			hosts := []string{}
			if gvr == ingresses {
				for _, rule := range maps(item.Object, "spec", "rules") {
					if host := str(rule, "host"); host != "" {
						hosts = append(hosts, host)
					}
				}
			} else {
				raw, _, _ := unstructured.NestedFieldNoCopy(item.Object, "spec", "hostnames")
				hosts = stringValues(raw)
			}
			ref := reference(item)
			candidate := v1.Candidate{Resource: ref, Provider: "native", Hostnames: unique(hosts), Eligible: len(hosts) > 0}
			if !candidate.Eligible {
				candidate.Reason = "No hostname is declared"
			}
			if !inventoryComplete || !inventoryAvailable {
				candidate.Eligible = false
				candidate.Reason = "Gslb inventory unavailable; native target availability cannot be confirmed"
			}
			var bound []*unstructured.Unstructured
			for i := range gslbItems {
				if mutation.ReferencesNativeTarget(&gslbItems[i], &item) {
					bound = append(bound, &gslbItems[i])
					candidate.Enabled, candidate.Eligible = true, false
					candidate.Reason = fmt.Sprintf("Already referenced by Gslb %s/%s (%s)", gslbItems[i].GetNamespace(), gslbItems[i].GetName(), gslbItems[i].GetAPIVersion())
				}
			}
			_, hasSnapshot := item.GetAnnotations()[mutation.NativeSiteStateAnnotation]
			if !siteServicesRead && len(bound) == 1 && (siteEnabled || hasSnapshot) {
				siteServicesRead = true
				var readErr error
				siteServices, readErr = c.list(ctx, services, namespace)
				siteServicesAvailable = readErr == nil
				if readErr != nil {
					result.Warnings = append(result.Warnings, "Service inventory unavailable; native site traffic cannot be confirmed")
				}
			}
			setNativeSiteCandidate(&candidate, &item, bound, inventoryComplete && inventoryAvailable, siteEnabled && !caps.ReadOnly, collection{items: siteServices, available: siteServicesAvailable})
			result.Candidates = append(result.Candidates, candidate)
		}
	}
	if cfg != nil {
		served := false
		for _, group := range []string{"traefik.io", "traefik.containo.us"} {
			gvr := schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "ingressroutes"}
			items, err := c.list(ctx, gvr, namespace)
			if err != nil {
				if !apierrors.IsNotFound(err) {
					result.Warnings = append(result.Warnings, "Traefik API unavailable: "+group)
				}
				continue
			}
			served = true
			for _, item := range items {
				candidate := traefik.Candidates(item, *cfg)
				traffic := traefik.SiteTrafficCandidate(item, generated, *cfg)
				candidate.SiteTrafficEligible, candidate.SiteTrafficReason = traffic.Eligible, traffic.Reason
				result.Candidates = append(result.Candidates, candidate)
			}
		}
		if !served {
			result.Warnings = append(result.Warnings, "No readable Traefik IngressRoute API; webhook management is unavailable")
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		a, b := result.Candidates[i], result.Candidates[j]
		return a.Provider+"/"+a.Resource.Name+"/"+a.Resource.APIVersion < b.Provider+"/"+b.Resource.Name+"/"+b.Resource.APIVersion
	})
	return result, nil
}

func setNativeSiteCandidate(candidate *v1.Candidate, ingress *unstructured.Unstructured, bound []*unstructured.Unstructured, complete, enabled bool, serviceData collection) {
	candidate.SiteTrafficReason = "Native site traffic requires exactly one standalone Gslb with a named Ingress resourceRef"
	if !complete {
		candidate.SiteTrafficReason = "Gslb inventory unavailable; native site traffic exclusivity cannot be confirmed"
		return
	}
	if len(bound) != 1 {
		if len(bound) > 1 {
			candidate.SiteTrafficReason = "Ingress is referenced by multiple Gslbs; native site traffic is unsafe"
		}
		return
	}
	state, err := mutation.InspectNativeSiteTraffic(bound[0], ingress)
	if err != nil {
		candidate.SiteTrafficReason = err.Error()
		return
	}
	traffic := mutation.InspectTraffic(bound[0].GetAnnotations())
	traffic.Excluded = false
	candidate.Traffic = &traffic
	if !serviceData.available {
		candidate.SiteTrafficReason = "Service inventory unavailable; native site traffic cannot be confirmed"
		if !enabled {
			candidate.SiteTrafficReason = "Native site traffic is disabled; opt in with --enable-native-site-traffic and --enable-writes"
		}
		return
	}
	present := map[string]bool{}
	for _, service := range serviceData.items {
		present[service.GetName()] = true
	}
	if present[state.ServiceName] {
		candidate.SiteTrafficReason = "Synthetic exclusion Service exists; refusing native site traffic"
		return
	}
	traffic.Excluded = state.Excluded
	if !enabled {
		candidate.SiteTrafficReason = "Native site traffic is disabled; opt in with --enable-native-site-traffic and --enable-writes"
		return
	}
	if !state.Excluded {
		for _, name := range state.OriginalServices {
			if !present[name] {
				candidate.SiteTrafficReason = "Original Service backend is missing; refusing native exclusion"
				return
			}
		}
	}
	candidate.SiteTrafficEligible = true
	candidate.SiteTrafficReason = mutation.NativeSiteLocalWarning
}
