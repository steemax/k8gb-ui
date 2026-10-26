//go:build integration

package integration_test

import (
	"reflect"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/traefik"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestTraefikRealTLSAdmission(t *testing.T) {
	image := requireWebhookImage(t)
	f := newFixture(t)
	cfg := f.installWebhook(t, image)
	reconciler := traefik.Reconciler{Client: f.client, Config: cfg}

	t.Run("admission_is_pure_and_dry_run_does_not_commit", func(t *testing.T) {
		u := f.route("pure-admission", map[string]string{"gslb": "true", "gslb_mtls": "true", "gslb_hostname": "pure.apps.example.test"})
		admitted, err := f.api(routes).Create(f.ctx, u, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		check(t, err)
		nested(t, admitted, []interface{}{"mtls"}, "spec", "entryPoints")
		if admitted.GetAnnotations()[traefik.OriginalEntrypoints] != `["websecure","grpc"]` {
			t.Fatalf("missing original entrypoints snapshot: %v", admitted.GetAnnotations())
		}
		f.absent(t, routes, u.GetName())
		f.noChildren(t, "")
		parent, err := f.api(routes).Create(f.ctx, u, metav1.CreateOptions{})
		check(t, err)
		nested(t, parent, []interface{}{"mtls"}, "spec", "entryPoints")
		f.noChildren(t, "")
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		f.ownedChild(t, ingresses, parent)
		f.ownedChild(t, gslbs, parent)
		parent = f.get(t, routes, parent.GetName())
		a := parent.GetAnnotations()
		a["gslb"], a["gslb_mtls"] = "false", "false"
		parent.SetAnnotations(a)
		parent, err = f.api(routes).Update(f.ctx, parent, metav1.UpdateOptions{})
		check(t, err)
		nested(t, parent, []interface{}{"websecure", "grpc"}, "spec", "entryPoints")
		if parent.GetAnnotations()[traefik.OriginalEntrypoints] != "" {
			t.Fatal("snapshot was not removed when restoring entrypoints")
		}
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		f.noChildren(t, string(parent.GetUID()))
	})

	t.Run("api_server_rejects_invalid_admission", func(t *testing.T) {
		for _, dryRun := range []bool{true, false} {
			u := f.route("invalid-admission", map[string]string{"gslb": "true", "gslb_hostname": "invalid.apps.example.test", "gslb_mtls": "true", "gslb_mtls_cn_check": "client,client"})
			opts := metav1.CreateOptions{}
			if dryRun {
				opts.DryRun = []string{metav1.DryRunAll}
			}
			_, err := f.api(routes).Create(f.ctx, u, opts)
			if err == nil || !apierrors.IsInvalid(err) {
				t.Fatalf("expected webhook 422 for duplicate CN (dryRun=%t), got %v", dryRun, err)
			}
			f.absent(t, routes, u.GetName())
		}
	})

	t.Run("provider_plan_apply_and_reconcile_preserve_manual_controls", func(t *testing.T) {
		parent, err := f.api(routes).Create(f.ctx, f.route("provider-route", map[string]string{"integration.example.test/keep": "parent"}), metav1.CreateOptions{})
		check(t, err)
		m := f.manager(t, []string{f.ns}, cfg, nil)
		request := v1.MutationRequest{Provider: "traefik", Operation: "enable", Resource: ref(parent), Actor: "integration-user", Reason: "enable via real API admission", Policy: &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 60}, Traefik: &v1.TraefikOptions{Hostname: "provider.apps.example.test", CommonNames: []string{}}}
		plan, err := m.Plan(f.ctx, request)
		check(t, err)
		if len(plan.Dependencies) < 2 {
			t.Fatalf("webhook registration and namespace must be pinned: %+v", plan.Dependencies)
		}
		f.unchanged(t, routes, parent)
		f.noChildren(t, string(parent.GetUID()))
		result, err := m.Apply(f.ctx, plan)
		check(t, err)
		if result.Status != "applied" {
			t.Fatalf("unexpected apply: %+v", result)
		}
		f.noChildren(t, string(parent.GetUID()))
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		ingress := f.ownedChild(t, ingresses, parent)
		gslb := f.ownedChild(t, gslbs, parent)
		ingressHost(t, ingress, "provider.apps.example.test")
		if ingressBackend(t, ingress, "backendqaz") != "backend" {
			t.Fatal("incorrect initial backend")
		}
		a := ingress.GetAnnotations()
		a["k8gb.io/exposed-ip-addresses"] = "192.0.2.10"
		a["integration.example.test/keep"] = "ingress"
		ingress.SetAnnotations(a)
		ingress, err = f.api(ingresses).Update(f.ctx, ingress, metav1.UpdateOptions{})
		check(t, err)
		gslb.SetAnnotations(map[string]string{"k8gb.io/exposed-ip-addresses": "192.0.2.11", "k8gb.io/exposed-hostnames": "override.example.test", "integration.example.test/keep": "gslb"})
		check(t, unstructured.SetNestedField(gslb.Object, int64(120), "spec", "strategy", "splitBrainThresholdSeconds"))
		check(t, unstructured.SetNestedField(gslb.Object, "All", "spec", "serviceHealthPolicy"))
		gslb, err = f.api(gslbs).Update(f.ctx, gslb, metav1.UpdateOptions{})
		check(t, err)
		independent := []struct {
			gvr    schema.GroupVersionResource
			object *unstructured.Unstructured
		}{}
		for _, group := range []string{"k8gb.io", "k8gb.absa.oss"} {
			r := f.createRequest(t, group, "independent-"+suffix(t))
			applied(t, f, m, r)
			gvr := gslbs
			gvr.Group = group
			independent = append(independent, struct {
				gvr    schema.GroupVersionResource
				object *unstructured.Unstructured
			}{gvr, f.get(t, gvr, r.Resource.Name)})
		}
		endpoint := f.createDNSEndpointIfSupported(t)
		parent = f.get(t, routes, parent.GetName())
		request.Operation, request.Resource = "update", ref(parent)
		request.Policy = &v1.Policy{Strategy: "failover", PrimaryGeoTag: "east", DNSTTLSeconds: 90}
		request.Traefik.Hostname = "updated.apps.example.test"
		plan, err = m.Plan(f.ctx, request)
		check(t, err)
		f.unchanged(t, routes, parent)
		f.unchanged(t, ingresses, ingress)
		f.unchanged(t, gslbs, gslb)
		result, err = m.Apply(f.ctx, plan)
		check(t, err)
		if result.Status != "applied" {
			t.Fatalf("update status: %+v", result)
		}
		f.unchanged(t, ingresses, ingress)
		f.unchanged(t, gslbs, gslb)
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		updatedIngress := f.get(t, ingresses, ingress.GetName())
		ingressHost(t, updatedIngress, "updated.apps.example.test")
		if ingressBackend(t, updatedIngress, "") != "backendqaz" {
			t.Fatal("reconciliation undid manual qaz Disable")
		}
		for _, key := range []string{"k8gb.io/exposed-ip-addresses", "integration.example.test/keep"} {
			if updatedIngress.GetAnnotations()[key] != ingress.GetAnnotations()[key] {
				t.Fatalf("Ingress lost override annotation %s", key)
			}
		}
		updatedGslb := f.get(t, gslbs, gslb.GetName())
		nested(t, updatedGslb, "failover", "spec", "strategy", "type")
		nested(t, updatedGslb, "east", "spec", "strategy", "primaryGeoTag")
		nested(t, updatedGslb, int64(90), "spec", "strategy", "dnsTtlSeconds")
		nested(t, updatedGslb, int64(120), "spec", "strategy", "splitBrainThresholdSeconds")
		nested(t, updatedGslb, "All", "spec", "serviceHealthPolicy")
		if !reflect.DeepEqual(updatedGslb.GetAnnotations(), gslb.GetAnnotations()) {
			t.Fatal("Gslb lost IP/hostname overrides")
		}
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		f.unchanged(t, ingresses, updatedIngress)
		f.unchanged(t, gslbs, updatedGslb)
		_, err = m.Plan(f.ctx, v1.MutationRequest{Provider: "native", Operation: "update", Resource: ref(updatedGslb), Actor: request.Actor, Reason: "reject editing generated Gslb", Policy: request.Policy})
		expectFailure(t, err, 409, "owned_resource")
		parent = f.get(t, routes, parent.GetName())
		request.Operation, request.Resource, request.Policy, request.Traefik = "deactivate", ref(parent), nil, nil
		applied(t, f, m, request)
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		f.noChildren(t, string(parent.GetUID()))
		for _, native := range independent {
			f.unchanged(t, native.gvr, native.object)
		}
		t.Run("dns_endpoint_preservation", func(t *testing.T) {
			if endpoint == nil {
				t.Skip("optional externaldns.k8s.io DNSEndpoint CRD is not installed")
			}
			f.unchanged(t, dnsEndpoints, endpoint)
		})
		parent = f.get(t, routes, parent.GetName())
		if parent.GetAnnotations()["integration.example.test/keep"] != "parent" {
			t.Fatal("parent lost unrelated annotation")
		}
		nested(t, parent, []interface{}{"websecure", "grpc"}, "spec", "entryPoints")
	})

	t.Run("mtls_cn_middleware_is_owned_and_removed", func(t *testing.T) {
		parent, err := f.api(routes).Create(f.ctx, f.route("cn-route", nil), metav1.CreateOptions{})
		check(t, err)
		m := f.manager(t, []string{f.ns}, cfg, nil)
		r := v1.MutationRequest{Provider: "traefik", Operation: "enable", Resource: ref(parent), Actor: "integration-user", Reason: "CN middleware acceptance", Policy: &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 60}, Traefik: &v1.TraefikOptions{Hostname: "cn.apps.example.test", MTLS: true, CommonNames: []string{"client.example.test"}}}
		plan, err := m.Plan(f.ctx, r)
		check(t, err)
		f.unchanged(t, routes, parent)
		f.noChildren(t, string(parent.GetUID()))
		result, err := m.Apply(f.ctx, plan)
		check(t, err)
		if result.Status != "applied" {
			t.Fatalf("CN apply: %+v", result)
		}
		f.noChildren(t, string(parent.GetUID()))
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		mw := f.ownedChild(t, middlewares, parent)
		nested(t, mw, []interface{}{"client.example.test"}, "spec", "plugin", "cncheck", "domains")
		f.ownedChild(t, ingresses, parent)
		f.ownedChild(t, gslbs, parent)
		parent = f.get(t, routes, parent.GetName())
		nested(t, parent, []interface{}{"mtls"}, "spec", "entryPoints")
		r.Operation, r.Resource, r.Policy, r.Traefik = "deactivate", ref(parent), nil, nil
		applied(t, f, m, r)
		check(t, reconciler.Reconcile(f.ctx, f.ns, parent.GetName(), parent.GetAPIVersion()))
		f.noChildren(t, string(parent.GetUID()))
		nested(t, f.get(t, routes, parent.GetName()), []interface{}{"websecure", "grpc"}, "spec", "entryPoints")
	})
}

func (f *fixture) children(t *testing.T, gvr schema.GroupVersionResource, uid string) []unstructured.Unstructured {
	t.Helper()
	selector := traefik.ManagedLabel + "=traefik-adapter"
	if uid != "" {
		selector += "," + traefik.OwnerLabel + "=" + uid
	}
	list, err := f.api(gvr).List(f.ctx, metav1.ListOptions{LabelSelector: selector})
	check(t, err)
	return list.Items
}

func (f *fixture) noChildren(t *testing.T, uid string) {
	t.Helper()
	for _, gvr := range []schema.GroupVersionResource{ingresses, gslbs, middlewares} {
		if children := f.children(t, gvr, uid); len(children) != 0 {
			t.Fatalf("unexpected %d generated %s before explicit reconciliation/after deactivation", len(children), gvr.Resource)
		}
	}
}

func (f *fixture) ownedChild(t *testing.T, gvr schema.GroupVersionResource, parent *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	children := f.children(t, gvr, string(parent.GetUID()))
	if len(children) != 1 {
		t.Fatalf("expected one owned %s, got %d", gvr.Resource, len(children))
	}
	u := children[0].DeepCopy()
	owners := u.GetOwnerReferences()
	if len(owners) != 1 || owners[0].UID != parent.GetUID() || owners[0].Name != parent.GetName() || owners[0].APIVersion != parent.GetAPIVersion() || owners[0].Controller == nil || !*owners[0].Controller {
		t.Fatalf("invalid child ownership: %+v", owners)
	}
	return u
}

func ingressBackend(t *testing.T, u *unstructured.Unstructured, replacement string) string {
	t.Helper()
	rules, found, err := unstructured.NestedSlice(u.Object, "spec", "rules")
	check(t, err)
	if !found || len(rules) == 0 {
		t.Fatal("missing Ingress rules")
	}
	rule := rules[0].(map[string]interface{})
	paths := rule["http"].(map[string]interface{})["paths"].([]interface{})
	service := paths[0].(map[string]interface{})["backend"].(map[string]interface{})["service"].(map[string]interface{})
	name, _ := service["name"].(string)
	if replacement != "" {
		service["name"] = replacement
		check(t, unstructured.SetNestedSlice(u.Object, rules, "spec", "rules"))
	}
	return name
}

func ingressHost(t *testing.T, u *unstructured.Unstructured, want string) {
	t.Helper()
	rules, _, err := unstructured.NestedSlice(u.Object, "spec", "rules")
	check(t, err)
	if len(rules) != 1 || rules[0].(map[string]interface{})["host"] != want {
		t.Fatalf("unexpected ingress host: %#v, want %s", rules, want)
	}
}

func (f *fixture) createDNSEndpointIfSupported(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	u := object("externaldns.k8s.io/v1alpha1", "DNSEndpoint", f.ns, "independent-dns", map[string]interface{}{
		"endpoints": []interface{}{map[string]interface{}{"dnsName": "independent.apps.example.test", "recordType": "A", "recordTTL": int64(60), "targets": []interface{}{"192.0.2.53"}}},
	})
	u, err := f.api(dnsEndpoints).Create(f.ctx, u, metav1.CreateOptions{})
	if apierrors.IsNotFound(err) {
		t.Log("DNSEndpoint preservation not exercised: optional externaldns.k8s.io CRD is not installed")
		return nil
	}
	check(t, err)
	return u
}
