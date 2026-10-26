package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8gb-ui/internal/mutation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func TestCollectorNativeExclusionRequiresVerifiedSnapshot(t *testing.T) {
	for _, mode := range []string{"valid", "malformed", "unmanaged-marker", "rules-drift", "uid", "owner", "extra-ref", "collision", "services-unavailable", "inventory-unavailable", "real-error"} {
		t.Run(mode, func(t *testing.T) {
			ingress, g, service := nativeSiteCatalogFixtures(t, true)
			g.SetAnnotations(map[string]string{mutation.ExposedIPsAnnotation: "192.0.2.10", mutation.SiteExcludedAnnotation: "true"})
			objects := []runtime.Object{ingress, g, service}
			switch mode {
			case "malformed":
				a := ingress.GetAnnotations()
				a[mutation.NativeSiteStateAnnotation] = "{"
				ingress.SetAnnotations(a)
			case "unmanaged-marker":
				a := ingress.GetAnnotations()
				delete(a, mutation.NativeSiteStateAnnotation)
				ingress.SetAnnotations(a)
			case "rules-drift":
				rules, _, _ := unstructured.NestedSlice(ingress.Object, "spec", "rules")
				paths, _, _ := unstructured.NestedSlice(rules[0].(map[string]interface{}), "http", "paths")
				_ = unstructured.SetNestedField(paths[0].(map[string]interface{}), "unmanaged-missing", "backend", "service", "name")
				_ = unstructured.SetNestedSlice(rules[0].(map[string]interface{}), paths, "http", "paths")
				_ = unstructured.SetNestedSlice(ingress.Object, rules, "spec", "rules")
			case "uid":
				g.SetUID("replacement")
			case "owner":
				g.SetLabels(map[string]string{"routeOwnerRef": "parent"})
			case "extra-ref":
				extra := g.DeepCopy()
				extra.SetName("extra")
				extra.SetUID("extra-uid")
				extra.SetAPIVersion("k8gb.absa.oss/v1beta1")
				objects = append(objects, extra)
			case "collision":
				collision := service.DeepCopy()
				collision.SetName(mutation.NativeSiteServiceName(string(g.GetUID())))
				objects = append(objects, collision)
			case "real-error":
				_ = unstructured.SetNestedSlice(g.Object, []interface{}{map[string]interface{}{"host": "web.example.test", "services": []interface{}{map[string]interface{}{"name": "other-broken"}}}}, "status", "servers")
			}
			client := fakeClient(objects...)
			if mode == "services-unavailable" {
				client.PrependReactor("list", "services", func(a ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				})
			}
			if mode == "inventory-unavailable" {
				client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
					if a.GetResource().Group != "k8gb.absa.oss" {
						return false, nil, nil
					}
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
				})
			}
			snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, o := range snapshot.Objects {
				if o.Resource.Name != g.GetName() {
					continue
				}
				found = true
				wantExcluded := mode == "valid" || mode == "real-error"
				if o.Traffic.Excluded != wantExcluded || len(o.Traffic.IPs) != 1 {
					t.Fatalf("wrong native traffic state: %+v", o)
				}
				if mode == "valid" {
					if o.Health != "Excluded" || len(o.Backends) != 1 || o.Backends[0].Health != "Excluded" || o.Backends[0].EndpointDataAvailable {
						t.Fatalf("intentional missing backend not excluded: %+v", o)
					}
				} else if mode == "real-error" {
					if o.Health != "NotFound" || !strings.Contains(strings.Join(o.Warnings, ";"), "Real backend fault") {
						t.Fatalf("real backend error hidden: %+v", o)
					}
					for _, backend := range o.Backends {
						if backend.Service == "other-broken" && backend.Health != "NotFound" {
							t.Fatal("unmanaged broken Service was hidden")
						}
					}
				} else if o.Health == "Excluded" {
					t.Fatalf("unverified state hid real fault: %+v", o)
				}
			}
			if !found {
				t.Fatal("Gslb missing from snapshot")
			}
			assertReadOnly(t, client, "app")
		})
	}
}
