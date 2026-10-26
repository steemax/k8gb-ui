package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func nativeTestTarget(kind string) *unstructured.Unstructured {
	version := "networking.k8s.io/v1"
	if kind == "HTTPRoute" {
		version = "gateway.networking.k8s.io/v1"
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": version, "kind": kind,
		"metadata": map[string]interface{}{"name": "route", "namespace": "app", "uid": "route-uid", "resourceVersion": "7", "labels": map[string]interface{}{"app": "web"}},
	}}
}

func nativeTestCreate(target *unstructured.Unstructured) v1.MutationRequest {
	req := fixtureRequest()
	req.Operation, req.Resource.Name, req.Resource.UID, req.Resource.ResourceVersion = "create", "new-name", "", ""
	ref := objectReference(target)
	req.Target = &ref
	return req
}

func nativeTestGslb(target *unstructured.Unstructured, version string) *unstructured.Unstructured {
	g := fixtureObject()
	g.SetAPIVersion(version)
	g.SetName("existing-owner")
	_ = unstructured.SetNestedMap(g.Object, map[string]interface{}{"apiVersion": target.GetAPIVersion(), "kind": target.GetKind(), "name": target.GetName()}, "spec", "resourceRef")
	return g
}

func nativeTestClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{}
	for _, group := range []string{"k8gb.io", "k8gb.absa.oss"} {
		kinds[schema.GroupVersionResource{Group: group, Version: "v1beta1", Resource: "gslbs"}] = "GslbList"
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
}

func assertNativePrepareReadOnly(t *testing.T, client *fake.FakeDynamicClient) {
	t.Helper()
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" || action.GetNamespace() != "app" {
			t.Fatalf("unexpected action: %+v", action)
		}
	}
}

func TestNativeRejectsExternalOwnershipWithoutCatalog(t *testing.T) {
	markers := map[string]func(*unstructured.Unstructured){
		"webhook-owner": func(u *unstructured.Unstructured) {
			u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "traefik.io/v1alpha1", Kind: "IngressRoute", Name: "parent"}})
		},
		"other-controller": func(u *unstructured.Unstructured) {
			controller := true
			u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "other.io/v1", Kind: "CustomRoute", Name: "parent", Controller: &controller}})
		},
		"non-controller-owner": func(u *unstructured.Unstructured) {
			controller := false
			u.SetOwnerReferences([]metav1.OwnerReference{{Kind: "CustomRoute", Name: "parent", Controller: &controller}})
		},
	}
	for _, key := range []string{"routeOwnerRef", "route-owner-ref", "gslb.io/route-owner-ref", "k8gb.io/route-owner-ref", "k8gb-ui.io/route-uid", "k8gb-ui.io/managed-by", "app.k8gb.io/managed-by", "created-by"} {
		for _, annotation := range []bool{false, true} {
			value := "external-controller"
			if key == "created-by" {
				value = "gslb-controller"
			}
			markers[key+"/annotation="+strconv.FormatBool(annotation)] = func(u *unstructured.Unstructured) {
				if annotation {
					u.SetAnnotations(map[string]string{key: value})
				} else {
					u.SetLabels(map[string]string{key: value})
				}
			}
		}
	}
	for _, operation := range []string{"create-Ingress", "create-HTTPRoute", "update"} {
		for name, mark := range markers {
			t.Run(operation+"/"+name, func(t *testing.T) {
				u := fixtureObject()
				req := fixtureRequest()
				if operation != "update" {
					u = nativeTestTarget(strings.TrimPrefix(operation, "create-"))
					req = nativeTestCreate(u)
				}
				mark(u)
				client := nativeTestClient(u)
				_, err := NewNativeProvider(client, []string{"east", "west"}).Prepare(context.Background(), req)
				if requireFailure(t, err, "owned_resource").Status != 409 {
					t.Fatal("ownership did not return conflict")
				}
				assertNativePrepareReadOnly(t, client)
			})
		}
	}
}

func TestNativeAllowsOrdinaryManagementLabels(t *testing.T) {
	if GeneratedChild(nil) {
		t.Fatal("nil is not generated")
	}
	u := fixtureObject()
	u.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm", "created-by": "human"})
	client := nativeTestClient(u)
	if _, err := NewNativeProvider(client, []string{"east", "west"}).Prepare(context.Background(), fixtureRequest()); err != nil {
		t.Fatal(err)
	}
	assertNativePrepareReadOnly(t, client)
}

func TestReferencesNativeTarget(t *testing.T) {
	for _, kind := range []string{"Ingress", "HTTPRoute"} {
		for _, test := range []struct {
			name string
			edit func(*unstructured.Unstructured)
			want bool
		}{
			{name: "named", want: true},
			{name: "same-name-other-version", want: true, edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, strings.Split(nativeTestTarget(kind).GetAPIVersion(), "/")[0]+"/v1beta1", "spec", "resourceRef", "apiVersion")
			}},
			{name: "stale-ref-uid-does-not-free-name", want: true, edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, "old-uid", "spec", "resourceRef", "uid")
			}},
			{name: "other-name", edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, "other", "spec", "resourceRef", "name")
			}},
			{name: "other-kind", edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, "Service", "spec", "resourceRef", "kind")
			}},
			{name: "other-group", edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, "other.io/v1", "spec", "resourceRef", "apiVersion")
			}},
			{name: "other-namespace", edit: func(g *unstructured.Unstructured) { g.SetNamespace("other") }},
			{name: "explicit-other-namespace", edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, "other", "spec", "resourceRef", "namespace")
			}},
			{name: "matching-labels", want: true, edit: func(g *unstructured.Unstructured) {
				unstructured.RemoveNestedField(g.Object, "spec", "resourceRef", "name")
				_ = unstructured.SetNestedStringMap(g.Object, map[string]string{"app": "web"}, "spec", "resourceRef", "matchLabels")
			}},
			{name: "named-reference-ignores-labels", want: true, edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedStringMap(g.Object, map[string]string{"app": "other"}, "spec", "resourceRef", "matchLabels")
			}},
			{name: "matching-expression", want: true, edit: func(g *unstructured.Unstructured) {
				unstructured.RemoveNestedField(g.Object, "spec", "resourceRef", "name")
				_ = unstructured.SetNestedSlice(g.Object, []interface{}{map[string]interface{}{"key": "app", "operator": "In", "values": []interface{}{"web"}}}, "spec", "resourceRef", "matchExpressions")
			}},
			{name: "malformed-selector-fails-closed", want: true, edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(g.Object, "broken", "spec", "resourceRef", "matchLabels")
			}},
			{name: "invalid-selector-fails-closed", want: true, edit: func(g *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(g.Object, []interface{}{map[string]interface{}{"key": "app", "operator": "Unknown"}}, "spec", "resourceRef", "matchExpressions")
			}},
			{name: "embedded-ingress", edit: func(g *unstructured.Unstructured) { unstructured.RemoveNestedField(g.Object, "spec", "resourceRef") }},
			{name: "no-name-or-selector", edit: func(g *unstructured.Unstructured) {
				unstructured.RemoveNestedField(g.Object, "spec", "resourceRef", "name")
			}},
		} {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				target := nativeTestTarget(kind)
				g := nativeTestGslb(target, "k8gb.io/v1beta1")
				if test.edit != nil {
					test.edit(g)
				}
				if got := ReferencesNativeTarget(g, target); got != test.want {
					t.Fatalf("got %v, want %v: %+v", got, test.want, g.Object)
				}
			})
		}
	}
}

func TestNativeCreateRejectsOccupiedRefs(t *testing.T) {
	for _, version := range []string{"k8gb.io/v1beta1", "k8gb.absa.oss/v1beta1"} {
		for _, kind := range []string{"Ingress", "HTTPRoute"} {
			for _, deleting := range []bool{false, true} {
				t.Run(version+"/"+kind+"/deleting="+strconv.FormatBool(deleting), func(t *testing.T) {
					target := nativeTestTarget(kind)
					g := nativeTestGslb(target, version)
					if deleting {
						now := metav1.Now()
						g.SetDeletionTimestamp(&now)
					}
					client := nativeTestClient(target, g)
					_, err := NewNativeProvider(client, []string{"east", "west"}).Prepare(context.Background(), nativeTestCreate(target))
					failure := requireFailure(t, err, "target_in_use")
					if failure.Status != 409 || !strings.Contains(failure.Message, "existing-owner") {
						t.Fatalf("unexpected conflict: %+v", failure)
					}
					assertNativePrepareReadOnly(t, client)
				})
			}
		}
	}
}

func TestNativeCreateOptionalInventoryAPIs(t *testing.T) {
	for _, missingGroup := range []string{"k8gb.io", "k8gb.absa.oss"} {
		for _, kind := range []string{"Ingress", "HTTPRoute"} {
			t.Run(missingGroup+"/"+kind, func(t *testing.T) {
				target := nativeTestTarget(kind)
				client := nativeTestClient(target)
				client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.GetResource().Group == missingGroup {
						return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
					}
					return false, nil, nil
				})
				req := nativeTestCreate(target)
				if missingGroup == "k8gb.io" {
					req.Resource.APIVersion = "k8gb.absa.oss/v1beta1"
				}
				prepared, err := NewNativeProvider(client, []string{"east", "west"}).Prepare(context.Background(), req)
				if err != nil || !reflect.DeepEqual(prepared.Dependencies, []v1.ResourceReference{*req.Target}) {
					t.Fatalf("missing optional API broke create or target pin: %+v %v", prepared, err)
				}
				assertNativePrepareReadOnly(t, client)
			})
		}
	}
}

func TestNativeCreateInventoryErrorsFailClosed(t *testing.T) {
	for _, group := range []string{"k8gb.io", "k8gb.absa.oss"} {
		for _, mode := range []string{"forbidden", "unavailable", "incomplete-page", "repeated-token", "occupied-second-page"} {
			t.Run(group+"/"+mode, func(t *testing.T) {
				target := nativeTestTarget("Ingress")
				client := nativeTestClient(target)
				pages := 0
				client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.GetResource().Group != group {
						return false, nil, nil
					}
					pages++
					switch mode {
					case "forbidden":
						return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
					case "unavailable":
						return true, nil, apierrors.NewServiceUnavailable("down")
					}
					if pages == 2 {
						if mode == "incomplete-page" {
							return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
						}
						if mode == "occupied-second-page" {
							return true, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nativeTestGslb(target, group+"/v1beta1")}}, nil
						}
					}
					page := &unstructured.UnstructuredList{}
					page.SetContinue("next")
					return true, page, nil
				})
				_, err := NewNativeProvider(client, []string{"east", "west"}).Prepare(context.Background(), nativeTestCreate(target))
				if err == nil {
					t.Fatal("unknown or occupied inventory accepted")
				}
				if mode == "occupied-second-page" {
					requireFailure(t, err, "target_in_use")
				}
				assertNativePrepareReadOnly(t, client)
			})
		}
	}
}

// Add namespace-scoped LIST support to the existing admission/write harness.
func nativePlanningHarness(t *testing.T) (*Manager, *mutationAPI) {
	t.Helper()
	manager, api, _ := newHarness(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/gslbs") {
			api.ServeHTTP(w, r)
			return
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		list := &unstructured.UnstructuredList{Object: map[string]interface{}{"apiVersion": "k8gb.io/v1beta1", "kind": "GslbList"}, Items: []unstructured.Unstructured{}}
		for path, object := range api.objects {
			if strings.HasPrefix(path, r.URL.Path+"/") {
				list.Items = append(list.Items, *object.DeepCopy())
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(server.Close)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, QPS: 1000, Burst: 1000, WrapTransport: SingleAttemptTransport})
	if err != nil {
		t.Fatal(err)
	}
	manager.client = client
	manager.opts.Providers["native"] = NewNativeProvider(client, []string{"east", "west"})
	return manager, api
}

func TestNativePlanApplyRechecksOccupancy(t *testing.T) {
	for _, kind := range []string{"Ingress", "HTTPRoute"} {
		for _, version := range []string{"k8gb.io/v1beta1", "k8gb.absa.oss/v1beta1"} {
			t.Run(kind+"/"+version, func(t *testing.T) {
				manager, api := nativePlanningHarness(t)
				target := nativeTestTarget(kind)
				api.objects[resourcePath(objectReference(target))] = target
				req := nativeTestCreate(target)
				first, err := manager.Plan(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				req.Resource.APIVersion, req.Resource.Name = version, "different-new-name"
				second, err := manager.Plan(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				if len(first.Dependencies) != 1 || first.Dependencies[0] != *first.Request.Target {
					t.Fatalf("target not pinned: %+v", first.Dependencies)
				}
				result, err := manager.Apply(context.Background(), second)
				if err != nil || result.Status != "applied" {
					t.Fatalf("first create failed: %+v %v", result, err)
				}
				result, err = manager.Apply(context.Background(), first)
				requireFailure(t, err, "target_in_use")
				if result.Status != "drift" || api.writes != 1 || api.dryRuns != 3 {
					t.Fatalf("stale competing plan wrote: %+v, writes=%d dryRuns=%d", result, api.writes, api.dryRuns)
				}
			})
		}
	}
}

func TestNativePlanApplyRechecksOwnership(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			manager, api := nativePlanningHarness(t)
			req := fixtureRequest()
			object := api.objects[resourcePath(req.Resource)]
			if operation == "create" {
				object = nativeTestTarget("Ingress")
				req = nativeTestCreate(object)
				api.objects[resourcePath(objectReference(object))] = object
			}
			plan, err := manager.Plan(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			object.SetAnnotations(map[string]string{"routeOwnerRef": "parent"})
			api.mu.Unlock()
			_, err = manager.Apply(context.Background(), plan)
			if err == nil || api.writes != 0 {
				t.Fatalf("new external ownership was ignored: %v writes=%d", err, api.writes)
			}
		})
	}
}
