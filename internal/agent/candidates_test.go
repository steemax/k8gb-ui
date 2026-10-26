package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestCatalogOptionalAPIs(t *testing.T) {
	for _, test := range []struct {
		name, wantWarning string
		canonicalMissing  bool
		legacyForbidden   bool
		gatewayForbidden  bool
	}{
		{name: "canonical Traefik without legacy or Gateway is normal"},
		{name: "neither Traefik API", canonicalMissing: true, wantWarning: "No readable Traefik"},
		{name: "legacy authorization error is not hidden", legacyForbidden: true, wantWarning: "Traefik API unavailable: traefik.containo.us"},
		{name: "Gateway authorization error is not hidden", gatewayForbidden: true, wantWarning: "Routing API unavailable: httproutes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fakeClient()
			client.PrependReactor("list", "httproutes", func(a ktesting.Action) (bool, runtime.Object, error) {
				if test.gatewayForbidden {
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				}
				return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
			})
			client.PrependReactor("list", "ingressroutes", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.GetResource().Group == "traefik.containo.us" && test.legacyForbidden {
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				}
				if a.GetResource().Group == "traefik.containo.us" || test.canonicalMissing {
					return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
				}
				return true, &unstructured.UnstructuredList{}, nil
			})
			catalog, err := collector(t, client).Catalog(context.Background(), "app", &traefik.Config{}, v1.Capabilities{})
			if err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(catalog.Warnings, "; ")
			if test.wantWarning == "" && warnings != "" || test.wantWarning != "" && !strings.Contains(warnings, test.wantWarning) {
				t.Fatalf("warnings = %q; want %q", warnings, test.wantWarning)
			}
			assertReadOnly(t, client, "app")
		})
	}
}

func nativeSiteCatalogFixtures(t *testing.T, excluded bool) (*unstructured.Unstructured, *unstructured.Unstructured, *unstructured.Unstructured) {
	t.Helper()
	ingress := object(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"native-route","namespace":"app","uid":"ingress-uid","resourceVersion":"7"},"spec":{"rules":[{"host":"web.example.test","http":{"paths":[{"path":"/","pathType":"Prefix","backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`)
	g := gslb(t, "k8gb.io/v1beta1", "native-global", `"spec":{"resourceRef":{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","name":"native-route"}},"status":{"serviceHealth":{"web.example.test":"NotFound"}}`)
	g.SetResourceVersion("9")
	service := object(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"app","uid":"service-uid","resourceVersion":"1"}}`)
	if excluded {
		client := fakeClient(ingress, g, service)
		target := reference(*g)
		p, err := mutation.NewNativeProviderWithSiteTraffic(client, nil).Prepare(context.Background(), v1.MutationRequest{Provider: "native", Operation: "excludeSite", Resource: reference(*ingress), Target: &target})
		if err != nil {
			t.Fatal(err)
		}
		ingress = p.Desired
	}
	return ingress, g, service
}

func nativeSiteCatalogCaps(enabled bool) v1.Capabilities {
	ops := []string{"create", "update"}
	if enabled {
		ops = append(ops, "excludeSite", "includeSite")
	}
	return v1.Capabilities{Operations: map[string][]string{"native": ops}}
}

func TestCatalogNativeSiteTrafficUsesSnapshotNotHealth(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("excluded=%t/enabled=%t", excluded, enabled), func(t *testing.T) {
				ingress, g, service := nativeSiteCatalogFixtures(t, excluded)
				if excluded {
					_ = unstructured.SetNestedStringMap(g.Object, map[string]string{"web.example.test": "Healthy"}, "status", "serviceHealth")
				}
				client := fakeClient(ingress, g, service)
				catalog, err := collector(t, client).Catalog(context.Background(), "app", nil, nativeSiteCatalogCaps(enabled))
				if err != nil || len(catalog.Candidates) != 1 {
					t.Fatalf("catalog=%+v %v", catalog, err)
				}
				c := catalog.Candidates[0]
				if c.Provider != "native" || !c.Enabled || c.Eligible || c.SiteTrafficEligible != enabled || c.Resource != reference(*ingress) || c.Traffic == nil || c.Traffic.Excluded != excluded {
					t.Fatalf("wrong native site contract: %+v", c)
				}
				if enabled && !strings.Contains(c.SiteTrafficReason, "LOCAL Ingress routing") {
					t.Fatal("missing local-route impact")
				}
				if !enabled && !strings.Contains(c.SiteTrafficReason, "disabled") {
					t.Fatal("opt-in was hidden")
				}
				assertReadOnly(t, client, "app")
			})
		}
	}
}

func TestCatalogNativeSiteGuards(t *testing.T) {
	for _, mode := range []string{"gslb-owner", "extra-canonical", "extra-legacy", "selector", "http-route", "collision", "malformed", "services-forbidden", "inventory-forbidden", "read-only", "missing-backend"} {
		t.Run(mode, func(t *testing.T) {
			ingress, g, service := nativeSiteCatalogFixtures(t, mode == "malformed")
			objects := []runtime.Object{ingress, g, service}
			switch mode {
			case "gslb-owner":
				g.SetLabels(map[string]string{"routeOwnerRef": "webhook"})
			case "extra-canonical", "extra-legacy":
				extra := g.DeepCopy()
				extra.SetName("extra")
				extra.SetUID("extra-uid")
				if mode == "extra-legacy" {
					extra.SetAPIVersion("k8gb.absa.oss/v1beta1")
				}
				objects = append(objects, extra)
			case "selector":
				unstructured.RemoveNestedField(g.Object, "spec", "resourceRef", "name")
				_ = unstructured.SetNestedStringMap(g.Object, map[string]string{"app": "web"}, "spec", "resourceRef", "matchLabels")
				ingress.SetLabels(map[string]string{"app": "web"})
			case "http-route":
				ingress.SetKind("HTTPRoute")
				ingress.SetAPIVersion("gateway.networking.k8s.io/v1")
				_ = unstructured.SetNestedStringSlice(ingress.Object, []string{"web.example.test"}, "spec", "hostnames")
				_ = unstructured.SetNestedField(g.Object, "HTTPRoute", "spec", "resourceRef", "kind")
				_ = unstructured.SetNestedField(g.Object, ingress.GetAPIVersion(), "spec", "resourceRef", "apiVersion")
			case "collision":
				collision := service.DeepCopy()
				collision.SetName(mutation.NativeSiteServiceName(string(g.GetUID())))
				objects = append(objects, collision)
			case "malformed":
				a := ingress.GetAnnotations()
				a[mutation.NativeSiteStateAnnotation] = "{}"
				ingress.SetAnnotations(a)
			case "missing-backend":
				objects = []runtime.Object{ingress, g}
			}
			client := fakeClient(objects...)
			if mode == "services-forbidden" {
				client.PrependReactor("list", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				})
			}
			if mode == "inventory-forbidden" {
				client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.GetResource().Group != "k8gb.absa.oss" {
						return false, nil, nil
					}
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				})
			}
			caps := nativeSiteCatalogCaps(true)
			caps.ReadOnly = mode == "read-only"
			catalog, err := collector(t, client).Catalog(context.Background(), "app", nil, caps)
			if err != nil || len(catalog.Candidates) != 1 {
				t.Fatalf("catalog=%+v %v", catalog, err)
			}
			c := catalog.Candidates[0]
			if c.SiteTrafficEligible || c.SiteTrafficReason == "" || !c.Enabled || c.Eligible {
				t.Fatalf("unsafe native site candidate: %+v", c)
			}
			if c.Traffic != nil && c.Traffic.Excluded {
				t.Fatal("unverified snapshot reported excluded")
			}
			assertReadOnly(t, client, "app")
		})
	}
}

func nativeCatalogTarget(t *testing.T, kind, name string) *unstructured.Unstructured {
	t.Helper()
	version := "networking.k8s.io/v1"
	spec := `"rules":[{"host":"web.example.test"}]`
	if kind == "HTTPRoute" {
		version = "gateway.networking.k8s.io/v1"
		spec = `"hostnames":["web.example.test"]`
	}
	return object(t, fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":%q,"namespace":"app","uid":"route-uid","resourceVersion":"7","labels":{"app":"web"}},"spec":{%s}}`, version, kind, name, spec))
}

func TestCatalogNativeOccupiedRefs(t *testing.T) {
	for _, version := range []string{"k8gb.io/v1beta1", "k8gb.absa.oss/v1beta1"} {
		for _, kind := range []string{"Ingress", "HTTPRoute"} {
			for _, selector := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/selector=%t", version, kind, selector), func(t *testing.T) {
					target := nativeCatalogTarget(t, kind, "route")
					free := nativeCatalogTarget(t, kind, "unused")
					free.SetLabels(map[string]string{"app": "other"})
					ref := map[string]interface{}{"apiVersion": target.GetAPIVersion(), "kind": kind, "name": target.GetName()}
					if selector {
						delete(ref, "name")
						ref["matchLabels"] = map[string]interface{}{"app": "web"}
					}
					g := gslb(t, version, "already-managed", `"spec":{}`)
					_ = unstructured.SetNestedMap(g.Object, ref, "spec", "resourceRef")
					client := fakeClient(target, free, g)
					catalog, err := collector(t, client).Catalog(context.Background(), "app", nil, v1.Capabilities{})
					if err != nil || len(catalog.Candidates) != 2 || len(catalog.Warnings) != 0 {
						t.Fatalf("catalog=%+v err=%v", catalog, err)
					}
					for _, candidate := range catalog.Candidates {
						if candidate.Resource.Name == "route" {
							if candidate.Eligible || !candidate.Enabled || !strings.Contains(candidate.Reason, "already-managed") {
								t.Fatalf("occupied target offered as native: %+v", candidate)
							}
						} else if !candidate.Eligible || candidate.Enabled || candidate.Reason != "" {
							t.Fatalf("unrelated target blocked: %+v", candidate)
						}
					}
					assertReadOnly(t, client, "app")
				})
			}
		}
	}
}

func TestCatalogOmitsExternallyOwnedNativeTargets(t *testing.T) {
	for _, kind := range []string{"Ingress", "HTTPRoute"} {
		for _, marker := range []string{"webhook-owner", "custom-controller", "routeOwnerRef-label", "routeOwnerRef-annotation", "created-by", "k8gb-ui.io/managed-by", "app.k8gb.io/managed-by", "k8gb-ui.io/route-uid"} {
			t.Run(kind+"/"+marker, func(t *testing.T) {
				target := nativeCatalogTarget(t, kind, "owned")
				switch marker {
				case "webhook-owner":
					target.SetOwnerReferences([]metav1.OwnerReference{{Kind: "IngressRoute", Name: "parent"}})
				case "custom-controller":
					controller := true
					target.SetOwnerReferences([]metav1.OwnerReference{{Kind: "CustomRoute", Name: "parent", Controller: &controller}})
				case "routeOwnerRef-label":
					target.SetLabels(map[string]string{"routeOwnerRef": "parent"})
				case "routeOwnerRef-annotation":
					target.SetAnnotations(map[string]string{"routeOwnerRef": "parent"})
				case "created-by":
					target.SetLabels(map[string]string{marker: "gslb-controller"})
				default:
					target.SetLabels(map[string]string{marker: "external-owner"})
				}
				client := fakeClient(target, nativeCatalogTarget(t, kind, "free"))
				catalog, err := collector(t, client).Catalog(context.Background(), "app", nil, v1.Capabilities{})
				if err != nil || len(catalog.Candidates) != 1 || catalog.Candidates[0].Resource.Name != "free" || !catalog.Candidates[0].Eligible {
					t.Fatalf("generated or owned target offered as native: %+v %v", catalog, err)
				}
				assertReadOnly(t, client, "app")
			})
		}
	}
}

func TestCatalogNativeInventoryAvailability(t *testing.T) {
	for _, test := range []struct {
		name, failingGroup string
		missing, allAbsent bool
		wantEligible       bool
	}{
		{name: "legacy-absent", failingGroup: "k8gb.absa.oss", missing: true, wantEligible: true},
		{name: "canonical-absent", failingGroup: "k8gb.io", missing: true, wantEligible: true},
		{name: "legacy-forbidden", failingGroup: "k8gb.absa.oss"},
		{name: "canonical-forbidden", failingGroup: "k8gb.io"},
		{name: "all-absent", allAbsent: true, missing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fakeClient(nativeCatalogTarget(t, "Ingress", "free"))
			client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
				if !test.allAbsent && a.GetResource().Group != test.failingGroup {
					return false, nil, nil
				}
				if test.missing {
					return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
				}
				return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
			})
			// Missing Gateway and Traefik APIs must not block a native Ingress.
			for _, resource := range []string{"httproutes", "ingressroutes"} {
				client.PrependReactor("list", resource, func(a ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
				})
			}
			catalog, err := collector(t, client).Catalog(context.Background(), "app", nil, v1.Capabilities{})
			if err != nil || len(catalog.Candidates) != 1 {
				t.Fatalf("catalog=%+v err=%v", catalog, err)
			}
			candidate := catalog.Candidates[0]
			if candidate.Eligible != test.wantEligible || candidate.Enabled {
				t.Fatalf("unknown inventory advertised as free: %+v", candidate)
			}
			if !test.wantEligible && (candidate.Reason == "" || len(catalog.Warnings) == 0) {
				t.Fatalf("inventory failure was hidden: %+v", catalog)
			}
			if test.wantEligible && len(catalog.Warnings) != 0 {
				t.Fatalf("missing optional API is not an error: %+v", catalog.Warnings)
			}
			assertReadOnly(t, client, "app")
		})
	}
}

func TestCatalogOccupancyIsNamespaceAndKindScoped(t *testing.T) {
	target := nativeCatalogTarget(t, "Ingress", "shared")
	g := gslb(t, "k8gb.io/v1beta1", "other-route", `"spec":{"resourceRef":{"apiVersion":"gateway.networking.k8s.io/v1","kind":"HTTPRoute","name":"shared"}}`)
	otherNamespace := gslb(t, "k8gb.io/v1beta1", "other-namespace", `"spec":{"resourceRef":{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","name":"shared"}}`)
	otherNamespace.SetNamespace("other")
	client := fakeClient(target, g, otherNamespace)
	catalog, err := collector(t, client).Catalog(context.Background(), "app", nil, v1.Capabilities{})
	if err != nil || len(catalog.Candidates) != 1 || !catalog.Candidates[0].Eligible || catalog.Candidates[0].Enabled {
		t.Fatalf("unrelated reference blocked candidate: %+v %v", catalog, err)
	}
	assertReadOnly(t, client, "app")
}
