package mutation

import (
	"context"
	"slices"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/dnszones"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (m *Manager) creationZone(ctx context.Context, req v1.MutationRequest, prepared *Prepared) error {
	creating := req.Provider == "native" && req.Operation == "create" || req.Provider == "traefik" && req.Operation == "enable"
	if !creating {
		if req.DNSZone != "" {
			return Failure(422, "invalid_dns_zone", "dnsZone is only accepted on creation")
		}
		return nil
	}
	if m.opts.DNSZones == nil {
		return nil
	} // Embedded providers may omit discovery; the agent CLI always configures it.
	zones, ref, err := m.opts.DNSZones.Read(ctx)
	if err != nil {
		return Failure(503, "dns_zones_unavailable", zones.Error)
	}
	if !slices.Contains(zones.Zones, req.DNSZone) {
		return Failure(422, "invalid_dns_zone", "select a DNS zone served by this cluster's k8gb controller")
	}
	hosts := []string{}
	if req.Provider == "traefik" {
		if req.Traefik == nil || req.Traefik.Local {
			return Failure(422, "invalid_options", "creation requires explicit Traefik options without local children")
		}
		hosts = append(hosts, req.Traefik.Hostname)
	} else {
		if req.Target == nil {
			return Failure(422, "invalid_target", "native creation needs a pinned routing target")
		}
		gvr, err := resourceGVR(*req.Target)
		if err != nil {
			return err
		}
		target, err := m.client.Resource(gvr).Namespace(req.Target.Namespace).Get(ctx, req.Target.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if !matchesReference(target, *req.Target, true) {
			return Failure(409, "stale_dependency", "native routing target changed")
		}
		if target.GetKind() == "Ingress" {
			rules, _, _ := unstructured.NestedSlice(target.Object, "spec", "rules")
			for _, raw := range rules {
				rule, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				host, _, _ := unstructured.NestedString(rule, "host")
				hosts = append(hosts, host)
			}
		} else {
			hosts, _, _ = unstructured.NestedStringSlice(target.Object, "spec", "hostnames")
		}
	}
	if len(hosts) == 0 {
		return Failure(422, "invalid_dns_zone", "routing target declares no hostname")
	}
	for _, host := range hosts {
		if !dnszones.Contains(req.DNSZone, host) {
			return Failure(422, "invalid_dns_zone", "every routing hostname must be below the selected DNS zone; existing native Ingress/HTTPRoute hostnames are not rewritten")
		}
	}
	prepared.Dependencies = append(prepared.Dependencies, ref)
	return nil
}
