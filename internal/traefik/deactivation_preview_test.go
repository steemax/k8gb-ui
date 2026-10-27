package traefik

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	v1 "k8gb-ui/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func previewSnapshot(t *testing.T, c dynamic.Interface, parent *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	gvr, err := routeGVR(parent.GetAPIVersion())
	if err != nil {
		t.Fatal(err)
	}
	resources := []schema.GroupVersionResource{gvr, ingressGVR, gslbGVR}
	middleware := gvr
	middleware.Resource = "middlewares"
	resources = append(resources, middleware)
	out := map[string]interface{}{}
	for _, resource := range resources {
		list, err := c.Resource(resource).Namespace(parent.GetNamespace()).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range list.Items {
			out[resource.String()+"/"+item.GetName()] = item.DeepCopy().Object
		}
	}
	return out
}

func TestDeactivationPreviewRegression(t *testing.T) {
	for _, apiVersion := range []string{"traefik.io/v1alpha1", "traefik.containo.us/v1alpha1"} {
		for _, mtls := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mtls=%t", apiVersion, mtls), func(t *testing.T) {
				parent, c, cfg := trafficFixture(t, apiVersion, mtls)
				before := previewSnapshot(t, c, parent)
				if len(before) < 4 {
					t.Fatalf("fixture has no existing generated resources: %d", len(before))
				}
				c.ClearActions()
				api := &trafficPlanClient{Interface: c, config: cfg}
				manager := trafficManager(t, api, cfg)
				request := v1.MutationRequest{Provider: "traefik", Operation: "deactivate", Resource: reference(parent), Actor: "admin", Reason: "Regression check for side-effect-free deactivation preview"}
				plan, err := manager.Plan(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				if plan.Noop || api.dryRuns != 1 || api.writes != 0 || len(writes(c)) != 0 {
					t.Fatalf("plan wrote or did not dry-run: noop=%t dry=%d writes=%d actions=%v", plan.Noop, api.dryRuns, api.writes, writes(c))
				}
				prepared, err := NewProvider(c, cfg).Prepare(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				response, _ := admit(t, NewValidatedAdmissionHandler(c, cfg), prepared.Desired, parent, true)
				if !response.Allowed || len(writes(c)) != 0 {
					t.Fatalf("dry-run admission denied or wrote: response=%+v actions=%v", response, writes(c))
				}
				reconciler := Reconciler{Client: c, Config: cfg}
				if err := reconciler.Reconcile(context.Background(), parent.GetNamespace(), parent.GetName(), apiVersion); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, previewSnapshot(t, c, parent)) || len(writes(c)) != 0 {
					t.Fatal("preview or reconciliation of the unchanged persisted parent mutated resources")
				}
				result, err := manager.Apply(context.Background(), plan)
				if err != nil || result.Status != "applied" || api.writes != 1 {
					t.Fatalf("apply: result=%+v err=%v writes=%d", result, err, api.writes)
				}
				gvr, _ := routeGVR(apiVersion)
				committed := trafficGet(t, c, gvr, parent.GetName())
				if enabled, err := flag(committed.GetAnnotations(), "gslb"); err != nil || enabled {
					t.Fatal("apply did not persist the disabled parent")
				}
				afterApply := previewSnapshot(t, c, parent)
				delete(before, gvr.String()+"/"+parent.GetName())
				delete(afterApply, gvr.String()+"/"+parent.GetName())
				if !reflect.DeepEqual(before, afterApply) {
					t.Fatal("apply deleted children synchronously")
				}
				c.ClearActions()
				if err := reconciler.Reconcile(context.Background(), parent.GetNamespace(), parent.GetName(), apiVersion); err != nil {
					t.Fatal(err)
				}
				remaining := previewSnapshot(t, c, parent)
				if len(remaining) != 1 || len(writes(c)) == 0 {
					t.Fatalf("expected child cleanup only after committed disable: remaining=%v actions=%v", remaining, writes(c))
				}
				t.Log("preview: 0 persistent writes/deletes; children unchanged; cleanup only after Apply + Reconcile")
			})
		}
	}
}
