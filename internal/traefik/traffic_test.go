package traefik

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func trafficFixture(t *testing.T, apiVersion string, mtls bool) (*unstructured.Unstructured, *fake.FakeDynamicClient, Config) {
	t.Helper()
	cfg := config()
	cfg.LocalDomain = "local.test"
	u := route(t, map[string]string{"gslb": "true", "local": "true"})
	u.SetAPIVersion(apiVersion)
	if mtls {
		a := u.GetAnnotations()
		a["mtls"] = "true"
		a["mtls_cn_check"] = "client.example.test"
		u.SetAnnotations(a)
		if err := normalizeParent(u, nil, cfg); err != nil {
			t.Fatal(err)
		}
	}
	c := client(u)
	if err := (Reconciler{Client: c, Config: cfg}).Reconcile(context.Background(), u.GetNamespace(), u.GetName(), u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	c.ClearActions()
	return u, c, cfg
}

func trafficGet(t *testing.T, c dynamic.Interface, gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := c.Resource(gvr).Namespace("app").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func trafficPut(t *testing.T, c dynamic.Interface, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	t.Helper()
	if _, err := c.Resource(gvr).Namespace(u.GetNamespace()).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func requireTrafficFailure(t *testing.T, err error, code string) {
	t.Helper()
	var failure *mutation.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func TestSiteTrafficLifecycle(t *testing.T) {
	for _, api := range []string{"traefik.io/v1alpha1", "traefik.containo.us/v1alpha1"} {
		for _, mtls := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mtls=%v", api, mtls), func(t *testing.T) {
				u, c, cfg := trafficFixture(t, api, mtls)
				ctx := context.Background()
				gvr, _ := routeGVR(api)
				name := childName(u, false)
				p := NewProvider(c, cfg)
				r := Reconciler{Client: c, Config: cfg}
				global := trafficGet(t, c, ingressGVR, name)
				a := global.GetAnnotations()
				a["custom.example/keep"] = "keep"
				global.SetAnnotations(a)
				trafficPut(t, c, ingressGVR, global)
				gslb := trafficGet(t, c, gslbGVR, name)
				gslb.SetAnnotations(map[string]string{mutation.IPOverrideStateAnnotation: "keep", "k8gb.io/override-ip": "192.0.2.1", "custom.example/keep": "yes"})
				mustSet(t, gslb, "keep", "spec", "extension")
				mustSet(t, gslb, int64(120), "spec", "strategy", "splitBrainThresholdSeconds")
				mustSet(t, gslb, "keep", "status", "extension")
				trafficPut(t, c, gslbGVR, gslb)
				var localIngress, localGslb *unstructured.Unstructured
				if !mtls {
					localIngress = trafficGet(t, c, ingressGVR, childName(u, true))
					localGslb = trafficGet(t, c, gslbGVR, childName(u, true))
				}
				liveIngress := global.DeepCopy()
				liveIngress.SetName("native-ingress")
				liveIngress.SetLabels(nil)
				liveIngress.SetOwnerReferences(nil)
				liveService := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": "backend", "namespace": "app", "uid": "live-service", "resourceVersion": "1"}, "spec": map[string]interface{}{"ports": []interface{}{map[string]interface{}{"port": int64(8080)}}}}}
				if err := c.Tracker().Add(liveIngress); err != nil {
					t.Fatal(err)
				}
				if err := c.Tracker().Add(liveService); err != nil {
					t.Fatal(err)
				}
				c.ClearActions()
				for _, operation := range []string{"excludeSite", "includeSite"} {
					prepared, err := p.Prepare(ctx, v1.MutationRequest{Provider: "traefik", Operation: operation, Resource: reference(u)})
					if err != nil {
						t.Fatal(err)
					}
					if len(writes(c)) != 0 || len(prepared.Dependencies) != 2 || !reflect.DeepEqual(prepared.Fields, []string{"/metadata/annotations/k8gb-ui.io~1site-excluded"}) {
						t.Fatal("site preparation wrote, omitted children pins, or allowed extra parent fields")
					}
					if !reflect.DeepEqual(prepared.Current.Object["spec"], prepared.Desired.Object["spec"]) {
						t.Fatal("site operation changed live parent routing")
					}
					if len(prepared.Warnings) == 0 || !strings.Contains(prepared.Warnings[0], "asynchronously") || !strings.Contains(prepared.Warnings[0], "not confirmed") {
						t.Fatal("missing asynchronous/non-DNS-confirmation warning")
					}
					before := trafficGet(t, c, ingressGVR, name)
					u = prepared.Desired.DeepCopy()
					u.SetResourceVersion(u.GetResourceVersion() + "1")
					trafficPut(t, c, gvr, u)
					if !reflect.DeepEqual(before.Object, trafficGet(t, c, ingressGVR, name).Object) {
						t.Fatal("parent commit synchronously mutated children")
					}
					c.ClearActions()
					if err := r.Reconcile(ctx, "app", "api", api); err != nil {
						t.Fatal(err)
					}
					global = trafficGet(t, c, ingressGVR, name)
					gslb = trafficGet(t, c, gslbGVR, name)
					excluded := operation == "excludeSite"
					wantService := "updated-backend"
					if excluded {
						wantService = ExcludedServiceName(string(u.GetUID()))
					}
					if text(firstIngressService(global), "name") != wantService || (global.GetAnnotations()[SiteExcludedAnnotation] == "true") != excluded || (gslb.GetAnnotations()[SiteExcludedAnnotation] == "true") != excluded {
						t.Fatalf("wrong global exclusion/backend: %v %v", global.Object, gslb.Object)
					}
					if !excluded {
						if _, ok := global.GetAnnotations()[SiteExcludedAnnotation]; ok {
							t.Fatal("include retained global Ingress marker")
						}
						if _, ok := gslb.GetAnnotations()[SiteExcludedAnnotation]; ok {
							t.Fatal("include retained global Gslb marker")
						}
					}
					if global.GetAnnotations()["custom.example/keep"] != "keep" || gslb.GetAnnotations()[mutation.IPOverrideStateAnnotation] != "keep" || gslb.GetAnnotations()["k8gb.io/override-ip"] != "192.0.2.1" {
						t.Fatal("metadata or IP override state lost")
					}
					for _, path := range [][]string{{"spec", "extension"}, {"status", "extension"}} {
						if value, _, _ := unstructured.NestedString(gslb.Object, path...); value != "keep" {
							t.Fatal("unmanaged Gslb fields lost")
						}
					}
					if n, _, _ := unstructured.NestedInt64(gslb.Object, "spec", "strategy", "splitBrainThresholdSeconds"); n != 120 {
						t.Fatal("strategy extension lost")
					}
					if !mtls && (!reflect.DeepEqual(localIngress.Object, trafficGet(t, c, ingressGVR, childName(u, true)).Object) || !reflect.DeepEqual(localGslb.Object, trafficGet(t, c, gslbGVR, childName(u, true)).Object)) {
						t.Fatal("site toggle changed local siblings")
					}
					for _, action := range writes(c) {
						if !slices.Contains([]string{"ingresses", "gslbs"}, action.GetResource().Resource) || action.GetVerb() != "update" || action.(ktesting.UpdateAction).GetObject().(metav1.Object).GetName() != name {
							t.Fatal("site toggle wrote outside the global generated pair")
						}
					}
					c.ClearActions()
					noop, err := p.Prepare(ctx, v1.MutationRequest{Provider: "traefik", Operation: operation, Resource: reference(u)})
					if err != nil || !reflect.DeepEqual(noop.Current.Object, noop.Desired.Object) {
						t.Fatalf("non-idempotent site operation: %v", err)
					}
					if err := r.Reconcile(ctx, "app", "api", api); err != nil || len(writes(c)) != 0 {
						t.Fatalf("non-idempotent reconciliation: %v", err)
					}
					if candidate := Candidate(*u, cfg); candidate.Traffic == nil || candidate.Traffic.Excluded != excluded {
						t.Fatal("candidate does not reflect requested parent exclusion")
					}
					if excluded {
						// Config changes must not re-include the global backend. Inclusion
						// later restores these current settings, never a saved snapshot.
						routes := u.Object["spec"].(map[string]interface{})["routes"].([]interface{})
						service := routes[0].(map[string]interface{})["services"].([]interface{})[0].(map[string]interface{})
						service["name"], service["port"] = "updated-backend", int64(9090)
						trafficPut(t, c, gvr, u)
						update, err := p.Prepare(ctx, v1.MutationRequest{Provider: "traefik", Operation: "update", Resource: reference(u), Policy: &v1.Policy{Strategy: "failover", PrimaryGeoTag: "east", DNSTTLSeconds: 45}})
						if err != nil {
							t.Fatal(err)
						}
						u = update.Desired.DeepCopy()
						u.SetResourceVersion(u.GetResourceVersion() + "1")
						trafficPut(t, c, gvr, u)
						if err := r.Reconcile(ctx, "app", "api", api); err != nil {
							t.Fatal(err)
						}
						global = trafficGet(t, c, ingressGVR, name)
						if text(firstIngressService(global), "name") != ExcludedServiceName(string(u.GetUID())) || global.GetAnnotations()[SiteExcludedAnnotation] != "true" {
							t.Fatal("config update removed site exclusion")
						}
						if port, _, _ := unstructured.NestedInt64(firstIngressService(global), "port", "number"); port != 9090 {
							t.Fatal("excluded backend port did not track current desired config")
						}
						gslb = trafficGet(t, c, gslbGVR, name)
						if strategy, _, _ := unstructured.NestedString(gslb.Object, "spec", "strategy", "type"); strategy != "failover" || gslb.GetAnnotations()[SiteExcludedAnnotation] != "true" {
							t.Fatal("policy update or exclusion marker lost")
						}
						if !mtls {
							localIngress = trafficGet(t, c, ingressGVR, childName(u, true))
							localGslb = trafficGet(t, c, gslbGVR, childName(u, true))
							if text(firstIngressService(localIngress), "name") != "updated-backend" || localIngress.GetAnnotations()[SiteExcludedAnnotation] != "" || localGslb.GetAnnotations()[SiteExcludedAnnotation] != "" {
								t.Fatal("local siblings inherited exclusion")
							}
						}
					}
					c.ClearActions()
				}
				if !reflect.DeepEqual(liveIngress.Object, trafficGet(t, c, ingressGVR, liveIngress.GetName()).Object) || !reflect.DeepEqual(liveService.Object, trafficGet(t, c, serviceGVR, liveService.GetName()).Object) {
					t.Fatal("native Ingress or live Service was modified")
				}
			})
		}
	}
}

func TestSiteExcludedAdmissionFlags(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		for _, value := range []string{"absent", "true", "false", "", "yes", "TRUE", "1", " false "} {
			t.Run(enabled+"/"+value, func(t *testing.T) {
				a := map[string]string{"gslb": enabled}
				if value != "absent" {
					a[SiteExcludedAnnotation] = value
				}
				u := route(t, a)
				valid := value == "absent" || value == "true" || value == "false"
				for _, dry := range []bool{true, false} {
					for _, old := range []*unstructured.Unstructured{nil, route(t, map[string]string{"gslb": enabled})} {
						response, result := admit(t, NewAdmissionHandler(config()), u, old, dry)
						if response.Allowed != valid || len(response.Patch) != 0 || !reflect.DeepEqual(result.Object, u.Object) {
							t.Fatalf("unexpected flag validation or admission mutation: %+v", response)
						}
					}
				}
				if candidate := Candidate(*u, config()); candidate.Eligible != valid || valid && (candidate.Traffic == nil || candidate.Traffic.Excluded != (value == "true")) {
					t.Fatal("candidate accepted an invalid marker or omitted requested traffic state")
				}
				if !valid {
					c := client(u)
					if err := (Reconciler{Client: c, Config: config()}).Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err == nil || len(writes(c)) != 0 {
						t.Fatal("reconciler accepted malformed exclusion")
					}
					for _, op := range []string{"enable", "update", "deactivate", "excludeSite", "includeSite"} {
						_, err := NewProvider(c, config()).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: op, Resource: reference(u)})
						requireTrafficFailure(t, err, "invalid_traefik_policy")
					}
				}
			})
		}
	}
}

func TestSiteTrafficRejectsOptions(t *testing.T) {
	u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
	for _, op := range []string{"enable", "update", "deactivate", "excludeSite", "includeSite"} {
		req := v1.MutationRequest{Provider: "traefik", Operation: op, Resource: reference(u), Traffic: &v1.TrafficOptions{}}
		_, err := NewProvider(c, cfg).Prepare(context.Background(), req)
		requireTrafficFailure(t, err, "invalid_options")
		if op != "excludeSite" && op != "includeSite" {
			continue
		}
		for _, set := range []func(*v1.MutationRequest){
			func(r *v1.MutationRequest) { r.Policy = &v1.Policy{} },
			func(r *v1.MutationRequest) { r.Traefik = &v1.TraefikOptions{} },
		} {
			req.Traffic, req.Policy, req.Traefik = nil, nil, nil
			set(&req)
			_, err := NewProvider(c, cfg).Prepare(context.Background(), req)
			requireTrafficFailure(t, err, "invalid_options")
		}
	}
	if len(writes(c)) != 0 {
		t.Fatal("rejected options caused writes")
	}
}

func TestSiteTrafficRequiresSupportedGlobalChildren(t *testing.T) {
	for _, variant := range []string{"missing-global", "only-local", "unowned", "wrong-owner", "unpinned-uid", "unpinned-rv", "deleting", "wrong-ref", "extra-path", "non-service", "bad-marker", "disabled", "parent-deleting"} {
		t.Run(variant, func(t *testing.T) {
			u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
			name := childName(u, false)
			ingress := trafficGet(t, c, ingressGVR, name)
			gslb := trafficGet(t, c, gslbGVR, name)
			gvr, _ := routeGVR(u.GetAPIVersion())
			switch variant {
			case "missing-global":
				_ = c.Tracker().Delete(gslbGVR, "app", name)
			case "only-local":
				_ = c.Tracker().Delete(gslbGVR, "app", name)
				_ = c.Tracker().Delete(ingressGVR, "app", name)
			case "unowned":
				ingress.SetOwnerReferences(nil)
			case "wrong-owner":
				labels := gslb.GetLabels()
				labels[OwnerLabel] = "other-parent"
				gslb.SetLabels(labels)
			case "unpinned-uid":
				ingress.SetUID("")
			case "unpinned-rv":
				gslb.SetResourceVersion("")
			case "deleting":
				now := metav1.Now()
				ingress.SetDeletionTimestamp(&now)
			case "wrong-ref":
				mustSet(t, gslb, "native-ingress", "spec", "resourceRef", "name")
			case "extra-path":
				rules := ingress.Object["spec"].(map[string]interface{})["rules"].([]interface{})
				http := rules[0].(map[string]interface{})["http"].(map[string]interface{})
				http["paths"] = append(http["paths"].([]interface{}), map[string]interface{}{"path": "/extra"})
			case "non-service":
				rules := ingress.Object["spec"].(map[string]interface{})["rules"].([]interface{})
				path := rules[0].(map[string]interface{})["http"].(map[string]interface{})["paths"].([]interface{})[0].(map[string]interface{})
				path["backend"] = map[string]interface{}{"resource": map[string]interface{}{"name": "other"}}
			case "bad-marker":
				gslb.SetAnnotations(map[string]string{SiteExcludedAnnotation: "yes"})
			case "disabled":
				u.SetAnnotations(map[string]string{"gslb": "false"})
			case "parent-deleting":
				now := metav1.Now()
				u.SetDeletionTimestamp(&now)
			}
			if variant != "missing-global" && variant != "only-local" {
				trafficPut(t, c, ingressGVR, ingress)
				trafficPut(t, c, gslbGVR, gslb)
			}
			trafficPut(t, c, gvr, u)
			children := []unstructured.Unstructured{}
			for _, resource := range []schema.GroupVersionResource{ingressGVR, gslbGVR} {
				list, err := c.Resource(resource).Namespace("app").List(context.Background(), metav1.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				children = append(children, list.Items...)
			}
			if candidate := SiteTrafficCandidate(*u, children, cfg); candidate.Eligible || candidate.Reason == "" {
				t.Fatal("unsafe site candidate accepted")
			}
			c.ClearActions()
			for _, op := range []string{"excludeSite", "includeSite"} {
				_, err := NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: op, Resource: reference(u)})
				if err == nil {
					t.Fatal("unsafe site parent accepted")
				}
			}
			if len(writes(c)) != 0 {
				t.Fatal("failed preparation wrote resources")
			}
		})
	}
}

func TestSiteTrafficServiceCollisionBeforeAnyWrites(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("late=%v", late), func(t *testing.T) {
			u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", true)
			name := ExcludedServiceName(string(u.GetUID()))
			service := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": name, "namespace": "app"}}}
			gets := 0
			c.PrependReactor("get", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.(ktesting.GetAction).GetName() != name {
					t.Fatal("checked a live/non-synthetic Service")
				}
				gets++
				if !late || gets > 1 {
					return true, service, nil
				}
				return false, nil, nil
			})
			gvr, _ := routeGVR(u.GetAPIVersion())
			a := u.GetAnnotations()
			a[SiteExcludedAnnotation] = "true"
			u.SetAnnotations(a)
			trafficPut(t, c, gvr, u)
			// Also make the middleware stale to exercise the whole-set preflight,
			// not only the eventual global Ingress update.
			a["mtls_cn_check"] = "changed-client.example.test"
			u.SetAnnotations(a)
			trafficPut(t, c, gvr, u)
			c.ClearActions()
			err := (Reconciler{Client: c, Config: cfg}).Reconcile(context.Background(), "app", "api", u.GetAPIVersion())
			requireTrafficFailure(t, err, "resource_collision")
			if len(writes(c)) != 0 || late && gets < 2 {
				t.Fatal("missing absence recheck or wrote before collision")
			}
			gets = 1
			_, err = NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "excludeSite", Resource: reference(u)})
			requireTrafficFailure(t, err, "resource_collision")
			before := gets
			prepared, err := NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "includeSite", Resource: reference(u)})
			if err != nil || prepared.Desired.GetAnnotations()[SiteExcludedAnnotation] != "" || gets != before {
				t.Fatalf("collision trapped includeSite: %v", err)
			}
		})
	}
}

func TestSiteTrafficIncludeRecoversFromServiceCollision(t *testing.T) {
	u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
	gvr, _ := routeGVR(u.GetAPIVersion())
	a := u.GetAnnotations()
	a[SiteExcludedAnnotation] = "true"
	u.SetAnnotations(a)
	trafficPut(t, c, gvr, u)
	r := Reconciler{Client: c, Config: cfg}
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	service := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": ExcludedServiceName(string(u.GetUID())), "namespace": "app"}}}
	if err := c.Tracker().Add(service); err != nil {
		t.Fatal(err)
	}
	c.PrependReactor("get", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
		t.Fatal("includeSite must not depend on synthetic Service absence")
		return false, nil, nil
	})
	api := &trafficPlanClient{Interface: c, config: cfg}
	m := trafficManager(t, api, cfg)
	req := v1.MutationRequest{Provider: "traefik", Operation: "includeSite", Resource: reference(u), Actor: "admin", Reason: "restore traffic"}
	plan, err := m.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.Apply(context.Background(), plan)
	if err != nil || result.Status != "applied" {
		t.Fatalf("include failed with synthetic Service collision: %+v %v", result, err)
	}
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	global := trafficGet(t, c, ingressGVR, childName(u, false))
	gslb := trafficGet(t, c, gslbGVR, childName(u, false))
	if text(firstIngressService(global), "name") != "backend" || global.GetAnnotations()[SiteExcludedAnnotation] != "" || gslb.GetAnnotations()[SiteExcludedAnnotation] != "" {
		t.Fatal("include retained synthetic backend or exclusion markers")
	}
}

func TestSiteTrafficDefaultDoesNotGuessOrReadServices(t *testing.T) {
	u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
	c.PrependReactor("get", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
		t.Fatal("ordinary route handling must not guess/check Services")
		return false, nil, nil
	})
	for _, op := range []string{"enable", "update", "deactivate"} {
		if _, err := NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: op, Resource: reference(u)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := (Reconciler{Client: c, Config: cfg}).Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	if len(writes(c)) != 0 {
		t.Fatal("unchanged ordinary route caused writes")
	}
	if ExcludedServiceName("parent-uid") == ExcludedServiceName("other-parent") || ExcludedServiceName("parent-uid") != ExcludedServiceName(string(u.GetUID())) {
		t.Fatal("synthetic Service name is not deterministic per parent UID")
	}
}

func TestSiteTrafficWebhookPinsAndAdmissionScope(t *testing.T) {
	u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
	cfg.WebhookConfig = "adapter"
	ns := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": "app", "uid": "namespace-uid", "resourceVersion": "4"}}}
	ns.SetLabels(map[string]string{Prefix + "traefik-webhook": "true"})
	for _, obj := range []runtime.Object{ns, mwc()} {
		if err := c.Tracker().Add(obj); err != nil {
			t.Fatal(err)
		}
	}
	children := []unstructured.Unstructured{*trafficGet(t, c, ingressGVR, childName(u, false)), *trafficGet(t, c, gslbGVR, childName(u, false))}
	if candidate := SiteTrafficCandidate(*u, children, cfg); !candidate.Eligible || candidate.Traffic == nil || candidate.Traffic.Excluded {
		t.Fatalf("supported global pair not offered: %+v", candidate)
	}
	p := NewProvider(c, cfg)
	prepared, err := p.Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "excludeSite", Resource: reference(u)})
	if err != nil || len(prepared.Dependencies) != 4 {
		t.Fatalf("webhook or global children pins were dropped: %+v %v", prepared.Dependencies, err)
	}
	validator := p.(interface {
		ValidateAdmission(mutation.Prepared, *unstructured.Unstructured) error
	})
	if err := validator.ValidateAdmission(prepared, prepared.Desired); err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*unstructured.Unstructured){
		func(u *unstructured.Unstructured) { mustSet(t, u, "changed", "spec", "tls", "secretName") },
		func(u *unstructured.Unstructured) {
			a := u.GetAnnotations()
			delete(a, SiteExcludedAnnotation)
			u.SetAnnotations(a)
		},
	} {
		changed := prepared.Desired.DeepCopy()
		alter(changed)
		requireTrafficFailure(t, validator.ValidateAdmission(prepared, changed), "unexpected_mutation")
	}
	if len(writes(c)) != 0 {
		t.Fatal("candidate/provider/admission validation wrote resources")
	}
}

func TestSiteTrafficDoesNotAdoptLegacyOrUnexpectedBackends(t *testing.T) {
	for _, variant := range []string{"ordinary", "literal-qaz", "legacy-unmarked", "unexpected-unmarked", "synthetic-unmarked", "port-unmarked", "unexpected-marked", "legacy-marked", "managed-excluded", "stale-observed-exclusion"} {
		t.Run(variant, func(t *testing.T) {
			u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
			gvr, _ := routeGVR(u.GetAPIVersion())
			name := childName(u, false)
			ingress := trafficGet(t, c, ingressGVR, name)
			gslb := trafficGet(t, c, gslbGVR, name)
			service := firstIngressService(ingress)
			allowed := variant == "ordinary" || variant == "literal-qaz" || variant == "managed-excluded"
			switch variant {
			case "literal-qaz":
				routes := u.Object["spec"].(map[string]interface{})["routes"].([]interface{})
				routes[0].(map[string]interface{})["services"].([]interface{})[0].(map[string]interface{})["name"] = "backendqaz"
				service["name"] = "backendqaz"
			case "legacy-unmarked", "legacy-marked":
				service["name"] = "backendqaz"
			case "unexpected-unmarked", "unexpected-marked":
				service["name"] = "external-change"
			case "synthetic-unmarked", "managed-excluded", "stale-observed-exclusion":
				service["name"] = ExcludedServiceName(string(u.GetUID()))
			case "port-unmarked":
				service["port"].(map[string]interface{})["number"] = int64(9090)
			}
			if slices.Contains([]string{"unexpected-marked", "legacy-marked", "managed-excluded"}, variant) {
				a := u.GetAnnotations()
				a[SiteExcludedAnnotation] = "true"
				u.SetAnnotations(a)
			}
			if variant == "managed-excluded" || variant == "stale-observed-exclusion" {
				a := ingress.GetAnnotations()
				a[SiteExcludedAnnotation] = "true"
				ingress.SetAnnotations(a)
				gslb.SetAnnotations(map[string]string{SiteExcludedAnnotation: "true"})
			}
			trafficPut(t, c, gvr, u)
			trafficPut(t, c, ingressGVR, ingress)
			trafficPut(t, c, gslbGVR, gslb)
			children := []unstructured.Unstructured{*ingress, *gslb}
			if candidate := SiteTrafficCandidate(*u, children, cfg); candidate.Eligible != allowed || !allowed && !strings.Contains(candidate.Reason, "backend differs") {
				t.Fatalf("candidate failed backend conflict guard: %+v", candidate)
			}
			if candidate := Candidate(*u, cfg); !candidate.Eligible {
				t.Fatal("site conflict disabled ordinary settings candidate")
			}
			c.ClearActions()
			for _, operation := range []string{"excludeSite", "includeSite"} {
				_, err := NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: operation, Resource: reference(u)})
				if allowed && err != nil {
					t.Fatalf("refused supported backend: %v", err)
				}
				if !allowed {
					requireTrafficFailure(t, err, "site_traffic_conflict")
				}
			}
			if len(writes(c)) != 0 {
				t.Fatal("backend validation wrote resources")
			}
		})
	}
}

func TestMergeChildNewMarkedExclusionDoesNotRetainLegacySuffix(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true"})
	s, err := settings(u, config())
	if err != nil {
		t.Fatal(err)
	}
	desired := makeIngress(u, childName(u, false), s.target.host, s, config(), false)
	current := desired.DeepCopy()
	a := current.GetAnnotations()
	a[SiteExcludedAnnotation] = "true"
	current.SetAnnotations(a)
	firstIngressService(current)["name"] = "stale-backendqaz"
	merged := mergeChild(current, desired)
	if text(firstIngressService(merged), "name") != "backend" || merged.GetAnnotations()[SiteExcludedAnnotation] != "" {
		t.Fatal("marked exclusion retained legacy suffix or marker during inclusion")
	}
}

func TestSiteTrafficIncludeUsesLatestRouteWithoutIntermediateReconcile(t *testing.T) {
	u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
	gvr, _ := routeGVR(u.GetAPIVersion())
	p := NewProvider(c, cfg)
	r := Reconciler{Client: c, Config: cfg}
	exclude, err := p.Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "excludeSite", Resource: reference(u)})
	if err != nil {
		t.Fatal(err)
	}
	u = exclude.Desired.DeepCopy()
	trafficPut(t, c, gvr, u)
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	routes := u.Object["spec"].(map[string]interface{})["routes"].([]interface{})
	service := routes[0].(map[string]interface{})["services"].([]interface{})[0].(map[string]interface{})
	service["name"], service["port"] = "current-backendqaz", int64(9091)
	u.SetResourceVersion("new-route-config")
	trafficPut(t, c, gvr, u)
	include, err := p.Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "includeSite", Resource: reference(u)})
	if err != nil {
		t.Fatal(err)
	}
	trafficPut(t, c, gvr, include.Desired)
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	global := trafficGet(t, c, ingressGVR, childName(u, false))
	port, _, _ := unstructured.NestedInt64(firstIngressService(global), "port", "number")
	if text(firstIngressService(global), "name") != "current-backendqaz" || port != 9091 || global.GetAnnotations()[SiteExcludedAnnotation] != "" {
		t.Fatal("include used a stale backend/port or retained synthetic/legacy state")
	}
}

// Preserve DryRun at the dynamic interface boundary: client-go's fake patch
// tracker does not honor it. This exercises the real mutation Manager plan/apply.
type trafficPlanClient struct {
	dynamic.Interface
	config          Config
	dryRuns, writes int
	afterDryRun     func()
}

type trafficPlanNamespaceable struct {
	dynamic.NamespaceableResourceInterface
	client *trafficPlanClient
}

type trafficPlanResource struct {
	dynamic.ResourceInterface
	client *trafficPlanClient
}

func (c *trafficPlanClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return trafficPlanNamespaceable{c.Interface.Resource(gvr), c}
}

func (r trafficPlanNamespaceable) Namespace(ns string) dynamic.ResourceInterface {
	return trafficPlanResource{r.NamespaceableResourceInterface.Namespace(ns), r.client}
}

func (r trafficPlanResource) Patch(ctx context.Context, name string, typ types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if typ != types.JSONPatchType {
		return nil, fmt.Errorf("expected JSONPatch")
	}
	current, err := r.ResourceInterface.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.DecodePatch(data)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(current.Object)
	patched, err := patch.Apply(body)
	if err != nil {
		return nil, err
	}
	desired := &unstructured.Unstructured{}
	if err := json.Unmarshal(patched, &desired.Object); err != nil {
		return nil, err
	}
	if _, err := settings(desired, r.client.config); err != nil {
		return nil, err
	}
	if err := normalizeParent(desired, current, r.client.config); err != nil {
		return nil, err
	}
	if len(opts.DryRun) > 0 {
		r.client.dryRuns++
		if r.client.afterDryRun != nil {
			r.client.afterDryRun()
		}
		return desired, nil
	}
	r.client.writes++
	desired.SetResourceVersion(current.GetResourceVersion() + "1")
	return r.ResourceInterface.Update(ctx, desired, metav1.UpdateOptions{})
}

func trafficManager(t *testing.T, c dynamic.Interface, cfg Config) *mutation.Manager {
	t.Helper()
	m, err := mutation.NewManager(c, mutation.Options{ClusterID: "site-a", Enabled: true, SigningKey: []byte(strings.Repeat("k", 32)), AllowedNamespaces: []string{"app"}, Providers: map[string]mutation.Provider{"traefik": NewProvider(c, cfg)}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSiteTrafficPlanApplyAndDependencies(t *testing.T) {
	for _, drift := range []string{"none", "child-rv", "child-status", "child-spec", "child-uid", "service", "service-during-dry-run"} {
		t.Run(drift, func(t *testing.T) {
			u, c, cfg := trafficFixture(t, "traefik.io/v1alpha1", false)
			api := &trafficPlanClient{Interface: c, config: cfg}
			m := trafficManager(t, api, cfg)
			req := v1.MutationRequest{Provider: "traefik", Operation: "excludeSite", Resource: reference(u), Actor: "admin", Reason: "maintenance"}
			plan, err := m.Plan(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Noop || len(plan.Changes) != 1 || len(plan.Dependencies) != 3 || api.dryRuns != 1 || api.writes != 0 || len(writes(c)) != 0 {
				t.Fatal("plan did not dry-run only the parent, with pinned children")
			}
			for _, dep := range plan.Dependencies {
				if dep.UID == "" || dep.ResourceVersion == "" || dep.Kind == "Service" {
					t.Fatal("missing child pin or invented absence dependency")
				}
			}
			if strings.HasPrefix(drift, "child-") {
				gslb := trafficGet(t, c, gslbGVR, childName(u, false))
				gslb.SetResourceVersion("changed")
				switch drift {
				case "child-status":
					gslb.Object["status"] = map[string]interface{}{"hosts": "changed.apps.example.test"}
				case "child-spec":
					_ = unstructured.SetNestedField(gslb.Object, int64(120), "spec", "strategy", "dnsTtlSeconds")
				case "child-uid":
					gslb.SetUID("recreated")
				}
				trafficPut(t, c, gslbGVR, gslb)
			}
			createService := func() {
				svc := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Service", "metadata": map[string]interface{}{"name": ExcludedServiceName(string(u.GetUID())), "namespace": "app"}}}
				if err := c.Tracker().Add(svc); err != nil {
					t.Fatal(err)
				}
			}
			if drift == "service" {
				createService()
			} else if drift == "service-during-dry-run" {
				api.afterDryRun = createService
			}
			c.ClearActions()
			result, err := m.Apply(context.Background(), plan)
			if drift != "none" && drift != "child-rv" && drift != "child-status" {
				if err == nil || api.writes != 0 || len(writes(c)) != 0 {
					t.Fatalf("apply ignored drift/collision: %v", err)
				}
				return
			}
			if err != nil || result.Status != "applied" || result.Reconciliation != "pending" || len(result.Warnings) == 0 || api.writes != 1 {
				t.Fatalf("apply failed: %+v %v", result, err)
			}
			global := trafficGet(t, c, ingressGVR, childName(u, false))
			if text(firstIngressService(global), "name") != "backend" || global.GetAnnotations()[SiteExcludedAnnotation] != "" {
				t.Fatal("parent apply synchronously wrote child")
			}
			gvr, _ := routeGVR(u.GetAPIVersion())
			u = trafficGet(t, c, gvr, u.GetName())
			req.Resource = reference(u)
			noop, err := m.Plan(context.Background(), req)
			if err != nil || !noop.Noop {
				t.Fatalf("no-op plan failed: %v", err)
			}
			result, err = m.Apply(context.Background(), noop)
			if err != nil || result.Status != "noop" || api.writes != 1 {
				t.Fatalf("no-op issued another write: %+v %v", result, err)
			}
		})
	}
}
