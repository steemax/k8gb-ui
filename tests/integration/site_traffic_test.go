//go:build integration

package integration_test

import (
	"reflect"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestSiteTrafficRealTLS(t *testing.T) {
	f := newFixture(t)
	cfg := f.installWebhook(t, requireWebhookImage(t))
	reconcile := traefik.Reconciler{Client: f.client, Config: cfg}
	parent, err := f.api(routes).Create(f.ctx, f.route("site-traffic", map[string]string{"gslb": "true", "gslb_hostname": "traffic.apps.example.test", "gslb_local": "true"}), metav1.CreateOptions{})
	check(t, err)
	check(t, reconcile.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
	child := func(gvr schema.GroupVersionResource, local bool) *unstructured.Unstructured {
		t.Helper()
		for _, u := range f.children(t, gvr, string(parent.GetUID())) {
			if strings.HasPrefix(u.GetName(), "local-gslb-") == local {
				return u.DeepCopy()
			}
		}
		t.Fatal("expected generated child missing")
		return nil
	}
	global, local := child(ingresses, false), child(ingresses, true)
	gslb := child(gslbs, false)
	m := f.manager(t, []string{f.ns}, cfg, nil)
	r := v1.MutationRequest{Provider: "traefik", Operation: "excludeSite", Resource: ref(parent), Actor: "local-admin", Reason: "Maintenance acceptance"}
	plan, err := m.Plan(f.ctx, r)
	check(t, err)
	f.unchanged(t, ingresses, global)
	f.unchanged(t, gslbs, gslb)
	_, err = m.Apply(f.ctx, plan)
	check(t, err)
	f.unchanged(t, ingresses, global)
	committed := f.get(t, routes, parent.GetName())
	if !reflect.DeepEqual(parent.Object["spec"], committed.Object["spec"]) {
		t.Fatal("site exclusion changed live parent routing")
	}
	check(t, reconcile.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
	excluded := child(ingresses, false)
	if ingressBackend(t, excluded, "") != traefik.ExcludedServiceName(string(parent.GetUID())) || excluded.GetAnnotations()[mutation.SiteExcludedAnnotation] != "true" {
		t.Fatalf("exclusion not reconciled: %v", excluded.Object)
	}
	f.unchanged(t, ingresses, local)
	gslb = child(gslbs, false)
	override := v1.MutationRequest{Provider: "traffic", Operation: "overrideIPs", Resource: ref(gslb), Traffic: &v1.TrafficOptions{IPs: []string{"192.0.2.99"}}, Actor: r.Actor, Reason: "Override during maintenance"}
	applied(t, f, m, override)
	// Application routing may change while excluded; include must use its latest backend.
	committed = f.get(t, routes, parent.GetName())
	routesSpec, _, _ := unstructured.NestedSlice(committed.Object, "spec", "routes")
	for _, item := range routesSpec {
		route := item.(map[string]interface{})
		services, _, _ := unstructured.NestedSlice(route, "services")
		for _, item := range services {
			item.(map[string]interface{})["name"] = "latest-backend"
		}
		_ = unstructured.SetNestedSlice(route, services, "services")
	}
	check(t, unstructured.SetNestedSlice(committed.Object, routesSpec, "spec", "routes"))
	committed, err = f.api(routes).Update(f.ctx, committed, metav1.UpdateOptions{})
	check(t, err)
	r.Operation, r.Resource, r.Policy = "update", ref(committed), &v1.Policy{Strategy: "failover", PrimaryGeoTag: "west", DNSTTLSeconds: 45}
	applied(t, f, m, r)
	check(t, reconcile.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
	if ingressBackend(t, child(ingresses, false), "") != traefik.ExcludedServiceName(string(parent.GetUID())) {
		t.Fatal("settings update cleared exclusion")
	}
	gslb = child(gslbs, false)
	if gslb.GetAnnotations()[mutation.ExposedIPsAnnotation] != "192.0.2.99" || !mutation.InspectTraffic(gslb.GetAnnotations()).OverrideManaged {
		t.Fatal("reconcile lost managed IP override")
	}
	local = child(ingresses, true)
	r.Operation, r.Resource, r.Policy = "includeSite", ref(f.get(t, routes, parent.GetName())), nil
	applied(t, f, m, r)
	check(t, reconcile.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
	global = child(ingresses, false)
	if ingressBackend(t, global, "") != "latest-backend" || global.GetAnnotations()[mutation.SiteExcludedAnnotation] != "" {
		t.Fatal("include did not restore latest desired route")
	}
	f.unchanged(t, ingresses, local)
	gslb = child(gslbs, false)
	if gslb.GetAnnotations()[mutation.SiteExcludedAnnotation] != "" || gslb.GetAnnotations()[mutation.ExposedIPsAnnotation] != "192.0.2.99" {
		t.Fatal("include changed IP override or retained exclusion marker")
	}
	override.Operation, override.Resource, override.Traffic = "resetIPs", ref(gslb), nil
	applied(t, f, m, override)
	if _, ok := child(gslbs, false).GetAnnotations()[mutation.ExposedIPsAnnotation]; ok {
		t.Fatal("reset did not restore absent original IP annotation")
	}
	// A future Service matching the reserved exclusion name cannot be silently used.
	serviceGVR := schema.GroupVersionResource{Version: "v1", Resource: "services"}
	svc := object("v1", "Service", f.ns, traefik.ExcludedServiceName(string(parent.GetUID())), map[string]interface{}{"ports": []interface{}{map[string]interface{}{"port": int64(80)}}})
	_, err = f.api(serviceGVR).Create(f.ctx, svc, metav1.CreateOptions{})
	check(t, err)
	r.Operation, r.Resource = "excludeSite", ref(f.get(t, routes, parent.GetName()))
	_, err = m.Plan(f.ctx, r)
	expectFailure(t, err, 409, "resource_collision")
}
