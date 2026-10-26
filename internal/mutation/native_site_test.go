package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func nativeSiteFixtures(t *testing.T) (*unstructured.Unstructured, *unstructured.Unstructured, []*unstructured.Unstructured) {
	t.Helper()
	ingress := nativeTestTarget("Ingress")
	var spec map[string]interface{}
	if err := kjson.Unmarshal([]byte(`{"rules":[{"host":"web.example.test","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}},{"path":"/api","pathType":"Exact","backend":{"service":{"name":"api","port":{"name":"http"}}}}]}},{"host":"other.example.test","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":8080}}}}]}}],"tls":[{"hosts":["web.example.test"],"secretName":"existing-cert"}],"ingressClassName":"nginx"}`), &spec); err != nil {
		t.Fatal(err)
	}
	ingress.Object["spec"] = spec
	ingress.SetAnnotations(map[string]string{"keep": "yes", ExposedIPsAnnotation: "192.0.2.4"})
	gslb := nativeTestGslb(ingress, "k8gb.io/v1beta1")
	unstructured.RemoveNestedField(gslb.Object, "spec", "ingress")
	gslb.SetAnnotations(map[string]string{ExposedIPsAnnotation: "192.0.2.8", IPOverrideStateAnnotation: `{"version":1,"ips":null,"hostnames":null,"applied":"192.0.2.8"}`, SiteExcludedAnnotation: "old-gslb-marker"})
	_ = unstructured.SetNestedField(gslb.Object, true, "spec", "sourceIngressTLS")
	var services []*unstructured.Unstructured
	for _, name := range []string{"web", "api"} {
		services = append(services, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": name, "namespace": "app", "uid": name + "-uid", "resourceVersion": "1"}, "spec": map[string]interface{}{"selector": map[string]interface{}{"app": name}}}})
	}
	return ingress, gslb, services
}

func nativeSiteRequest(operation string, ingress, gslb *unstructured.Unstructured) v1.MutationRequest {
	target := objectReference(gslb)
	return v1.MutationRequest{Provider: "native", Operation: operation, Resource: objectReference(ingress), Target: &target, Actor: "admin", Reason: "site maintenance"}
}

func nativeSiteFake(ingress, gslb *unstructured.Unstructured, services []*unstructured.Unstructured) *fake.FakeDynamicClient {
	objects := []runtime.Object{ingress, gslb}
	for _, service := range services {
		objects = append(objects, service)
	}
	return nativeTestClient(objects...)
}

func TestNativeSiteRoundTripAndFirstSnapshot(t *testing.T) {
	for _, marker := range []*string{nil, stringPtr(""), stringPtr("false"), stringPtr("true"), stringPtr("external-value")} {
		name := "absent"
		if marker != nil {
			name = "value=" + *marker
		}
		t.Run(name, func(t *testing.T) {
			ingress, gslb, services := nativeSiteFixtures(t)
			a := ingress.GetAnnotations()
			restoreAnnotation(a, SiteExcludedAnnotation, marker)
			ingress.SetAnnotations(a)
			original := ingress.DeepCopy()
			originalGslb := gslb.DeepCopy()
			client := nativeSiteFake(ingress, gslb, services)
			provider := NewNativeProviderWithSiteTraffic(client, nil)
			prepared, err := provider.Prepare(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(prepared.Warnings, " "), "LOCAL Ingress routing will also stop working") {
				t.Fatal("missing required local routing warning")
			}
			if !reflect.DeepEqual(withoutFields(prepared.Current, prepared.Fields), withoutFields(prepared.Desired, prepared.Fields)) {
				t.Fatal("changed fields outside allowlist")
			}
			state, err := InspectNativeSiteTraffic(gslb, prepared.Desired)
			if err != nil || !state.Excluded {
				t.Fatalf("invalid applied snapshot: %+v %v", state, err)
			}
			rules, _, _ := unstructured.NestedSlice(original.Object, "spec", "rules")
			want, _, _ := nativeSiteRules(rules, state.ServiceName)
			got, _, _ := unstructured.NestedSlice(prepared.Desired.Object, "spec", "rules")
			if !reflect.DeepEqual(want, got) || len(prepared.Dependencies) != 3 {
				t.Fatalf("rules or dependencies: %+v", prepared)
			}
			if err := client.Tracker().Update(prepared.GVR, prepared.Desired, "app"); err != nil {
				t.Fatal(err)
			}
			repeat, err := provider.Prepare(context.Background(), nativeSiteRequest("excludeSite", prepared.Desired, gslb))
			if err != nil || !reflect.DeepEqual(repeat.Current.Object, repeat.Desired.Object) {
				t.Fatalf("repeat exclude replaced first snapshot: %v", err)
			}
			included, err := provider.Prepare(context.Background(), nativeSiteRequest("includeSite", prepared.Desired, gslb))
			if err != nil || !reflect.DeepEqual(included.Desired.Object, original.Object) {
				t.Fatalf("restore was not exact: %v\ngot=%+v\nwant=%+v", err, included.Desired, original)
			}
			liveGslb, _ := client.Resource(schema.GroupVersionResource{Group: "k8gb.io", Version: "v1beta1", Resource: "gslbs"}).Namespace("app").Get(context.Background(), gslb.GetName(), metav1.GetOptions{})
			if !reflect.DeepEqual(liveGslb.Object, originalGslb.Object) {
				t.Fatal("changed Gslb policy, sourceIngressTLS or IP overrides")
			}
			assertNativePrepareReadOnly(t, client)
		})
	}
}

func stringPtr(value string) *string { return &value }

func TestNativeSiteOptInAndCapabilities(t *testing.T) {
	ingress, gslb, services := nativeSiteFixtures(t)
	client := nativeSiteFake(ingress, gslb, services)
	for _, enabled := range []bool{false, true} {
		provider := NewNativeProvider(client, []string{"east"})
		if enabled {
			provider = NewNativeProviderWithSiteTraffic(client, []string{"east"})
		}
		manager, err := NewManager(client, Options{ClusterID: "test", Enabled: true, SigningKey: []byte(strings.Repeat("k", 32)), Providers: map[string]Provider{"native": provider}})
		if err != nil {
			t.Fatal(err)
		}
		ops := manager.Capabilities().Operations["native"]
		if enabled && len(ops) != 4 || !enabled && len(ops) != 2 {
			t.Fatalf("unexpected default capabilities: %+v", ops)
		}
		_, err = provider.Prepare(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
		if enabled && err != nil {
			t.Fatal(err)
		}
		if !enabled {
			requireFailure(t, err, "unsupported_operation")
		}
		manager.opts.Enabled = false
		if !manager.Capabilities().ReadOnly || len(manager.Capabilities().Operations) != 0 {
			t.Fatal("write-disabled capabilities advertised native site traffic")
		}
		_, err = manager.Plan(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
		requireFailure(t, err, "mutations_disabled")
	}
}

func TestNativeSiteRejectsUnsafeBindingsAndPayload(t *testing.T) {
	for _, test := range []struct {
		name, code string
		edit       func(*unstructured.Unstructured, *unstructured.Unstructured, *v1.MutationRequest)
	}{
		{"policy", "invalid_native_site_request", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			r.Policy = &v1.Policy{Strategy: "roundRobin"}
		}},
		{"traefik", "invalid_native_site_request", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Traefik = &v1.TraefikOptions{} }},
		{"traffic", "invalid_native_site_request", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Traffic = &v1.TrafficOptions{} }},
		{"target-missing", "invalid_native_site_request", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Target = nil }},
		{"target-uid", "stale_dependency", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Target.UID = "old" }},
		{"target-namespace", "invalid_native_site_request", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Target.Namespace = "other" }},
		{"ingress-rv", "stale_resource", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Resource.ResourceVersion = "old" }},
		{"ingress-unpinned", "invalid_native_site_request", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) { r.Resource.UID = "" }},
		{"other-ref", "native_site_conflict", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			_ = unstructured.SetNestedField(g.Object, "other", "spec", "resourceRef", "name")
		}},
		{"inline", "unsupported_native_site", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			unstructured.RemoveNestedField(g.Object, "spec", "resourceRef")
		}},
		{"mixed-inline", "unsupported_native_site", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			_ = unstructured.SetNestedMap(g.Object, map[string]interface{}{"rules": []interface{}{}}, "spec", "ingress")
		}},
		{"selector", "unsupported_native_site", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			_ = unstructured.SetNestedStringMap(g.Object, map[string]string{"app": "web"}, "spec", "resourceRef", "matchLabels")
		}},
		{"default-backend", "unsupported_native_site", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			_ = unstructured.SetNestedMap(i.Object, map[string]interface{}{}, "spec", "defaultBackend")
		}},
		{"gslb-owner", "owned_resource", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			g.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Other", Name: "owner"}})
		}},
		{"ingress-owner", "owned_resource", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			i.SetOwnerReferences([]metav1.OwnerReference{{Kind: "IngressRoute", Name: "owner"}})
		}},
		{"gslb-owner-label", "owned_resource", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			g.SetLabels(map[string]string{"routeOwnerRef": "parent"})
		}},
		{"ingress-owner-label", "owned_resource", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			i.SetAnnotations(map[string]string{"k8gb-ui.io/managed-by": "traefik-adapter"})
		}},
		{"deleting", "stale_resource", func(i, g *unstructured.Unstructured, r *v1.MutationRequest) {
			now := metav1.Now()
			i.SetDeletionTimestamp(&now)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ingress, gslb, services := nativeSiteFixtures(t)
			req := nativeSiteRequest("excludeSite", ingress, gslb)
			test.edit(ingress, gslb, &req)
			client := nativeSiteFake(ingress, gslb, services)
			_, err := NewNativeProviderWithSiteTraffic(client, nil).Prepare(context.Background(), req)
			requireFailure(t, err, test.code)
			assertNativePrepareReadOnly(t, client)
		})
	}
}

func TestNativeSiteAllowsEmptyCRDDefaultIngress(t *testing.T) {
	ingress, gslb, services := nativeSiteFixtures(t)
	if err := unstructured.SetNestedMap(gslb.Object, map[string]interface{}{}, "spec", "ingress"); err != nil {
		t.Fatal(err)
	}
	client := nativeSiteFake(ingress, gslb, services)
	prepared, err := NewNativeProviderWithSiteTraffic(client, nil).Prepare(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
	if err != nil {
		t.Fatal(err)
	}
	state, err := InspectNativeSiteTraffic(gslb, prepared.Desired)
	if err != nil || !state.Excluded {
		t.Fatalf("CRD-defaulted reference must allow exclusion: state=%+v err=%v", state, err)
	}
	assertNativePrepareReadOnly(t, client)
}

func TestNativeSiteSnapshotDriftAndMalformed(t *testing.T) {
	for _, mode := range []string{"rules", "marker", "empty", "json", "version", "unknown", "trailing", "gslb-uid", "ingress-uid", "missing-marker-snapshot", "oversized", "invalid-original"} {
		t.Run(mode, func(t *testing.T) {
			ingress, gslb, services := nativeSiteFixtures(t)
			client := nativeSiteFake(ingress, gslb, services)
			provider := NewNativeProviderWithSiteTraffic(client, nil)
			p, err := provider.Prepare(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
			if err != nil {
				t.Fatal(err)
			}
			u := p.Desired
			a := u.GetAnnotations()
			var snapshot map[string]json.RawMessage
			_ = json.Unmarshal([]byte(a[NativeSiteStateAnnotation]), &snapshot)
			switch mode {
			case "rules":
				rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
				rules[0].(map[string]interface{})["host"] = "changed.example.test"
				_ = unstructured.SetNestedSlice(u.Object, rules, "spec", "rules")
			case "marker":
				a[SiteExcludedAnnotation] = "false"
			case "empty":
				a[NativeSiteStateAnnotation] = ""
			case "json":
				a[NativeSiteStateAnnotation] = "{"
			case "version":
				snapshot["version"] = json.RawMessage(`2`)
			case "unknown":
				snapshot["unknown"] = json.RawMessage(`true`)
			case "trailing":
				a[NativeSiteStateAnnotation] += " {}"
			case "gslb-uid":
				snapshot["gslbUID"] = json.RawMessage(`"other"`)
			case "ingress-uid":
				snapshot["ingressUID"] = json.RawMessage(`"other"`)
			case "missing-marker-snapshot":
				delete(snapshot, "excluded")
			case "oversized":
				a[NativeSiteStateAnnotation] = strings.Repeat("x", nativeSiteSnapshotLimit+1)
			case "invalid-original":
				snapshot["rules"] = json.RawMessage(`[null]`)
			}
			if mode == "version" || mode == "unknown" || mode == "gslb-uid" || mode == "ingress-uid" || mode == "missing-marker-snapshot" || mode == "invalid-original" {
				raw, _ := json.Marshal(snapshot)
				a[NativeSiteStateAnnotation] = string(raw)
			}
			u.SetAnnotations(a)
			if err := client.Tracker().Update(p.GVR, u, "app"); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"excludeSite", "includeSite"} {
				_, err := provider.Prepare(context.Background(), nativeSiteRequest(op, u, gslb))
				if requireFailure(t, err, "native_site_conflict").Status != 409 {
					t.Fatal("snapshot conflict must be 409")
				}
			}
			state, err := InspectNativeSiteTraffic(gslb, u)
			if err == nil || state.Excluded {
				t.Fatalf("malformed state trusted: %+v %v", state, err)
			}
			assertNativePrepareReadOnly(t, client)
		})
	}
}

func TestNativeSiteInventoryAndServiceGuards(t *testing.T) {
	for _, mode := range []string{"canonical-extra", "legacy-extra", "selector-extra", "synthetic-collision", "services-forbidden", "legacy-forbidden", "original-missing", "optional-legacy-missing"} {
		t.Run(mode, func(t *testing.T) {
			ingress, gslb, services := nativeSiteFixtures(t)
			client := nativeSiteFake(ingress, gslb, services)
			if strings.Contains(mode, "extra") {
				extra := gslb.DeepCopy()
				extra.SetName("another-gslb")
				extra.SetUID("another-uid")
				if mode == "legacy-extra" {
					extra.SetAPIVersion("k8gb.absa.oss/v1beta1")
				}
				if mode == "selector-extra" {
					unstructured.RemoveNestedField(extra.Object, "spec", "resourceRef", "name")
					_ = unstructured.SetNestedStringMap(extra.Object, map[string]string{"app": "web"}, "spec", "resourceRef", "matchLabels")
				}
				gvr, _ := resourceGVR(objectReference(extra))
				if err := client.Tracker().Create(gvr, extra, "app"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "synthetic-collision" {
				service := services[0].DeepCopy()
				service.SetName(NativeSiteServiceName(string(gslb.GetUID())))
				_ = client.Tracker().Create(schema.GroupVersionResource{Version: "v1", Resource: "services"}, service, "app")
			}
			if mode == "original-missing" {
				_ = client.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "services"}, "app", "web")
			}
			if mode == "services-forbidden" {
				client.PrependReactor("get", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				})
			}
			if mode == "legacy-forbidden" || mode == "optional-legacy-missing" {
				client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.GetResource().Group != "k8gb.absa.oss" {
						return false, nil, nil
					}
					if mode == "optional-legacy-missing" {
						return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
					}
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				})
			}
			_, err := NewNativeProviderWithSiteTraffic(client, nil).Prepare(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
			if mode == "optional-legacy-missing" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe inventory accepted")
			}
			assertNativePrepareReadOnly(t, client)
		})
	}
}

func TestNativeSiteUnmanagedIncludeAndSnapshotLimit(t *testing.T) {
	ingress, gslb, services := nativeSiteFixtures(t)
	client := nativeSiteFake(ingress, gslb, services)
	provider := NewNativeProviderWithSiteTraffic(client, nil)
	_, err := provider.Prepare(context.Background(), nativeSiteRequest("includeSite", ingress, gslb))
	requireFailure(t, err, "unmanaged_exclusion")
	rules, _, _ := unstructured.NestedSlice(ingress.Object, "spec", "rules")
	paths, _, _ := unstructured.NestedSlice(rules[0].(map[string]interface{}), "http", "paths")
	paths[0].(map[string]interface{})["path"] = strings.Repeat("x", nativeSiteSnapshotLimit)
	_ = unstructured.SetNestedSlice(rules[0].(map[string]interface{}), paths, "http", "paths")
	_ = unstructured.SetNestedSlice(ingress.Object, rules, "spec", "rules")
	_ = client.Tracker().Update(schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}, ingress, "app")
	_, err = provider.Prepare(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
	requireFailure(t, err, "snapshot_too_large")
	assertNativePrepareReadOnly(t, client)
}

func TestNativeSiteUnmanagedBrokenInclude(t *testing.T) {
	ingress, gslb, services := nativeSiteFixtures(t)
	rules, _, _ := unstructured.NestedSlice(ingress.Object, "spec", "rules")
	paths, _, _ := unstructured.NestedSlice(rules[0].(map[string]interface{}), "http", "paths")
	_ = unstructured.SetNestedField(paths[0].(map[string]interface{}), "unmanaged-missing", "backend", "service", "name")
	_ = unstructured.SetNestedSlice(rules[0].(map[string]interface{}), paths, "http", "paths")
	_ = unstructured.SetNestedSlice(ingress.Object, rules, "spec", "rules")
	original := ingress.DeepCopy()
	client := nativeSiteFake(ingress, gslb, services)
	_, err := NewNativeProviderWithSiteTraffic(client, nil).Prepare(context.Background(), nativeSiteRequest("includeSite", ingress, gslb))
	requireFailure(t, err, "unmanaged_exclusion")
	if !reflect.DeepEqual(ingress.Object, original.Object) {
		t.Fatal("unmanaged broken backend was changed")
	}
	assertNativePrepareReadOnly(t, client)
}

func nativeSiteEngine(t *testing.T) (*Manager, *mutationAPI, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	manager, api := nativePlanningHarness(t)
	ingress, gslb, services := nativeSiteFixtures(t)
	api.objects[resourcePath(objectReference(ingress))] = ingress
	api.objects[resourcePath(objectReference(gslb))] = gslb
	for _, service := range services {
		api.objects[resourcePath(objectReference(service))] = service
	}
	manager.opts.Providers["native"] = NewNativeProviderWithSiteTraffic(manager.client, []string{"east", "west"})
	return manager, api, ingress, gslb
}

func TestNativeSiteSignedEngineStatusChurnAndRoundTrip(t *testing.T) {
	manager, api, ingress, gslb := nativeSiteEngine(t)
	originalIngress, originalGslb := ingress.DeepCopy(), gslb.DeepCopy()
	req := nativeSiteRequest("excludeSite", ingress, gslb)
	req.Target.ResourceVersion = "old-observed-status-rv"
	plan, err := manager.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Dependencies) != 4 {
		t.Fatalf("missing Gslb/config, Service or root CAS pins: %+v", plan.Dependencies)
	}
	for _, dep := range plan.Dependencies {
		if dep.Kind == "Gslb" && len(dep.ConfigurationDigest) != 64 {
			t.Fatal("Gslb configuration not pinned")
		}
	}
	api.mu.Lock()
	gslb.SetResourceVersion("status-2")
	_ = unstructured.SetNestedField(gslb.Object, "new status", "status", "health")
	api.mu.Unlock()
	result, err := manager.Apply(context.Background(), plan)
	if err != nil || result.Status != "applied" {
		t.Fatalf("status-only churn blocked apply: %+v %v", result, err)
	}
	saved := api.objects[resourcePath(req.Resource)]
	repeat, err := manager.Plan(context.Background(), nativeSiteRequest("excludeSite", saved, gslb))
	if err != nil || !repeat.Noop {
		t.Fatalf("repeat exclude must be a noop: %+v %v", repeat, err)
	}
	noop, err := manager.Apply(context.Background(), repeat)
	if err != nil || noop.Status != "noop" || api.writes != 1 {
		t.Fatalf("repeat exclude wrote again: %+v %v", noop, err)
	}
	if !reflect.DeepEqual(normalize(gslb), normalize(originalGslb)) {
		t.Fatal("Gslb configuration changed")
	}
	for _, patch := range api.patches {
		if patch[0].Path != "/metadata/uid" || patch[1].Path != "/metadata/resourceVersion" {
			t.Fatal("missing root CAS")
		}
		for _, op := range patch[2:] {
			if op.Path != "/spec/rules" && !strings.HasPrefix(op.Path, "/metadata/annotations/") {
				t.Fatalf("unexpected write path: %s", op.Path)
			}
		}
	}
	include, err := manager.Plan(context.Background(), nativeSiteRequest("includeSite", saved, gslb))
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Apply(context.Background(), include)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalize(api.objects[resourcePath(req.Resource)]), normalize(originalIngress)) || api.writes != 2 {
		t.Fatal("include did not restore exactly")
	}
	for _, key := range []string{ExposedIPsAnnotation, IPOverrideStateAnnotation, SiteExcludedAnnotation} {
		if gslb.GetAnnotations()[key] != originalGslb.GetAnnotations()[key] {
			t.Fatal("Gslb IP override or marker changed")
		}
	}
}

func TestNativeSiteSignedEngineDriftConflictAndAdmission(t *testing.T) {
	for _, mode := range []string{"ingress-rv", "gslb-config", "gslb-owner", "extra-ref", "synthetic-service", "signature", "write-conflict", "admission"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, ingress, gslb := nativeSiteEngine(t)
			plan, err := manager.Plan(context.Background(), nativeSiteRequest("excludeSite", ingress, gslb))
			if err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			switch mode {
			case "ingress-rv":
				ingress.SetResourceVersion("new")
			case "gslb-config":
				_ = unstructured.SetNestedField(gslb.Object, int64(3600), "spec", "strategy", "dnsTtlSeconds")
			case "gslb-owner":
				gslb.SetLabels(map[string]string{"routeOwnerRef": "owner"})
			case "extra-ref":
				extra := gslb.DeepCopy()
				extra.SetName("extra")
				extra.SetUID("extra-uid")
				extra.SetAPIVersion("k8gb.absa.oss/v1beta1")
				api.objects[resourcePath(objectReference(extra))] = extra
			case "synthetic-service":
				service := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": NativeSiteServiceName(string(gslb.GetUID())), "namespace": "app"}}}
				api.objects[resourcePath(objectReference(service))] = service
			case "signature":
				plan.Request.Target.UID = "forged"
			case "write-conflict":
				api.writeStatus = 409
			case "admission":
				api.admit = func(u *unstructured.Unstructured, dry bool) {
					_ = unstructured.SetNestedSlice(u.Object, []interface{}{}, "spec", "rules")
				}
			}
			api.mu.Unlock()
			_, err = manager.Apply(context.Background(), plan)
			if err == nil {
				t.Fatal("unsafe apply succeeded")
			}
			if mode == "write-conflict" {
				if api.writes != 1 {
					t.Fatal("conflicting write retried")
				}
			} else if api.writes != 0 {
				t.Fatal("drift reached writes")
			}
			if _, found := ingress.GetAnnotations()[NativeSiteStateAnnotation]; found {
				t.Fatal("failed apply mutated ingress")
			}
		})
	}
}
