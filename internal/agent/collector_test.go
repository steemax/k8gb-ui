package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func object(t *testing.T, raw string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(raw), u); err != nil {
		t.Fatal(err)
	}
	return u
}

func fakeClient(objects ...runtime.Object) *fake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{canonicalGSLB: "GslbList", legacyGSLB: "GslbList", namespaces: "NamespaceList", ingresses: "IngressList", httpRoutes: "HTTPRouteList", dnsEndpoints: "DNSEndpointList", endpointSlices: "EndpointSliceList", services: "ServiceList"}
	for _, group := range []string{"traefik.io", "traefik.containo.us"} {
		kinds[schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "ingressroutes"}] = "IngressRouteList"
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
}

func collector(t *testing.T, client *fake.FakeDynamicClient, allowed ...string) *Collector {
	t.Helper()
	c, err := NewCollector(client, Options{ClusterID: "test", AllowedNamespaces: allowed})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gslb(t *testing.T, version, name, extra string) *unstructured.Unstructured {
	t.Helper()
	return object(t, fmt.Sprintf(`{"apiVersion":%q,"kind":"Gslb","metadata":{"namespace":"app","name":%q,"uid":%q},%s}`, version, name, version+"-"+name, extra))
}

func assertReadOnly(t *testing.T, client *fake.FakeDynamicClient, ns string) {
	t.Helper()
	for _, action := range client.Actions() {
		if action.GetVerb() != "list" {
			t.Fatalf("non-read verb: %v", action)
		}
		if action.GetResource() != namespaces && action.GetNamespace() != ns {
			t.Fatalf("unscoped or cross-namespace read: %v", action)
		}
	}
}

func TestSnapshotMultiHostPolicyDNSAndBackends(t *testing.T) {
	g := gslb(t, "k8gb.io/v1beta1", "web", `"spec":{"resourceRef":{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","name":"route"},"strategy":{"type":"failover","primaryGeoTag":"east","dnsTtlSeconds":60,"weight":{"east":80,"west":20}}},"status":{"hosts":"one.example.test, two.example.test","geoTag":"east","serviceHealth":{"one.example.test":"Healthy"},"servers":[{"host":"one.example.test","services":[{"name":"web"}]},{"host":"two.example.test","services":[{"name":"api","namespace":"app"}]}]}`)
	ing := object(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"namespace":"app","name":"route","uid":"ing-1"},"spec":{"rules":[{"host":"one.example.test","http":{"paths":[{"backend":{"service":{"name":"web","port":{"name":"http"}}}}]}},{"host":"two.example.test","http":{"paths":[{"backend":{"service":{"name":"api","port":{"number":8080}}}}]}}]}}`)
	dns := object(t, `{"apiVersion":"externaldns.k8s.io/v1alpha1","kind":"DNSEndpoint","metadata":{"namespace":"app","name":"different-name","ownerReferences":[{"apiVersion":"k8gb.io/v1beta1","kind":"Gslb","name":"web","uid":"k8gb.io/v1beta1-web"}]},"spec":{"endpoints":[{"dnsName":"ONE.EXAMPLE.TEST.","recordType":"A","targets":["192.0.2.1","192.0.2.1"]},{"dnsName":"one.example.test","recordType":"AAAA","targets":["2001:db8::1"]},{"dnsName":"two.example.test","recordType":"CNAME","targets":["target.example.test"]},{"dnsName":"localtargets.one.example.test","recordType":"A","targets":["192.0.2.99"]},{"dnsName":"unrelated.example.test","recordType":"A","targets":["192.0.2.98"]}]}}`)
	slice := object(t, `{"apiVersion":"discovery.k8s.io/v1","kind":"EndpointSlice","metadata":{"name":"web-1","namespace":"app","labels":{"kubernetes.io/service-name":"web"}},"endpoints":[{"addresses":["192.0.2.10"],"conditions":{"ready":true}},{"addresses":["192.0.2.11"],"conditions":{"ready":false}},{"addresses":["192.0.2.12"]}]}`)
	svc := object(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"api","namespace":"app"}}`)
	client := fakeClient(g, ing, dns, slice, svc)
	snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Objects) != 2 {
		t.Fatalf("objects: %+v", snapshot)
	}
	one, two := snapshot.Objects[0], snapshot.Objects[1]
	if one.Hostname != "one.example.test" || two.Hostname != "two.example.test" || one.Health != "Healthy" || two.Health != "Unknown" {
		t.Fatalf("host health: %+v", snapshot.Objects)
	}
	if one.Target == nil || one.Target.Name != "route" || one.Policy.Strategy != "failover" || one.Policy.PrimaryGeoTag != "east" || one.Policy.DNSTTLSeconds != 60 || one.Policy.Weights["east"] != 80 {
		t.Fatalf("target or policy: %+v", one)
	}
	if !reflect.DeepEqual(one.DNSTargets, []string{"192.0.2.1", "2001:db8::1"}) || !one.DNSObserved || !reflect.DeepEqual(two.DNSTargets, []string{"target.example.test"}) {
		t.Fatalf("DNS: %+v / %+v", one, two)
	}
	if len(one.Backends) != 1 || one.Backends[0].Port != "http" || one.Backends[0].Health != "Healthy" || one.Backends[0].TotalEndpoints != 3 || one.Backends[0].ReadyEndpoints != 2 || !one.Backends[0].EndpointDataAvailable {
		t.Fatalf("backend: %+v", one.Backends)
	}
	if len(two.Backends) != 1 || two.Backends[0].Port != "8080" || two.Backends[0].Health != "Unhealthy" {
		t.Fatalf("backend: %+v", two.Backends)
	}
	assertReadOnly(t, client, "app")
}

func TestCanonicalCompatibility(t *testing.T) {
	for _, test := range []struct {
		name         string
		canonicalErr error
		legacyErr    error
		wantError    bool
		wantLegacy   bool
		wantComplete bool
	}{
		{name: "both served", wantLegacy: true, wantComplete: true},
		{name: "canonical absent", canonicalErr: apierrors.NewNotFound(canonicalGSLB.GroupResource(), ""), wantLegacy: true, wantComplete: true},
		{name: "canonical forbidden", canonicalErr: apierrors.NewForbidden(canonicalGSLB.GroupResource(), "", errors.New("denied")), wantError: true},
		{name: "canonical unavailable", canonicalErr: apierrors.NewServiceUnavailable("down"), wantError: true},
		{name: "legacy absent is normal", legacyErr: apierrors.NewNotFound(legacyGSLB.GroupResource(), ""), wantLegacy: true, wantComplete: true},
		{name: "legacy forbidden optional", legacyErr: apierrors.NewForbidden(legacyGSLB.GroupResource(), "", errors.New("denied")), wantLegacy: true},
		{name: "legacy context timeout", legacyErr: context.DeadlineExceeded, wantLegacy: true},
		{name: "legacy API timeout", legacyErr: apierrors.NewTimeoutError("timed out", 0), wantLegacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fakeClient(gslb(t, "k8gb.absa.oss/v1beta1", "old", `"status":{"hosts":["old.example.test"]}`))
			client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.GetResource() == canonicalGSLB && test.canonicalErr != nil {
					return true, nil, test.canonicalErr
				}
				if a.GetResource() == legacyGSLB && test.legacyErr != nil {
					return true, nil, test.legacyErr
				}
				return false, nil, nil
			})
			snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
			if (err != nil) != test.wantError {
				t.Fatalf("error: %v", err)
			}
			if snapshot.ObjectsComplete != test.wantComplete {
				t.Fatalf("objectsComplete=%v, want %v", snapshot.ObjectsComplete, test.wantComplete)
			}
			queriedLegacy := false
			for _, a := range client.Actions() {
				queriedLegacy = queriedLegacy || a.GetResource() == legacyGSLB
			}
			if queriedLegacy != test.wantLegacy {
				t.Fatalf("legacy queried=%v", queriedLegacy)
			}
			if !test.wantError && test.legacyErr == nil && (len(snapshot.Objects) != 1 || snapshot.Objects[0].Resource.APIVersion != "k8gb.absa.oss/v1beta1") {
				t.Fatalf("snapshot: %+v", snapshot)
			}
			if test.legacyErr != nil && !apierrors.IsNotFound(test.legacyErr) && len(snapshot.Warnings) == 0 {
				t.Fatal("missing legacy warning")
			}
			if !test.wantError && (test.legacyErr == nil || apierrors.IsNotFound(test.legacyErr)) && len(snapshot.Warnings) != 0 {
				t.Fatalf("normal compatibility is not a failure: %v", snapshot.Warnings)
			}
			assertReadOnly(t, client, "app")
		})
	}
}

func TestBothGslbAPIsAbsentIsTypedFailure(t *testing.T) {
	client := fakeClient()
	client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
	})
	snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
	var unavailable *APIUnavailableError
	if !errors.Is(err, ErrPrimaryAPIUnavailable) || !errors.As(err, &unavailable) {
		t.Fatalf("expected typed API unavailability, got %+v %v", snapshot, err)
	}
	if snapshot.ObjectsComplete {
		t.Fatal("missing primary APIs reported a complete inventory")
	}
	if len(client.Actions()) != 2 {
		t.Fatal("optional APIs queried after missing primary APIs")
	}
}

func TestPrimaryInventoryCompletenessPreservesCanonicalObjects(t *testing.T) {
	for _, legacyErr := range []error{
		nil,
		apierrors.NewNotFound(legacyGSLB.GroupResource(), ""),
		apierrors.NewForbidden(legacyGSLB.GroupResource(), "", errors.New("denied")),
		context.DeadlineExceeded,
	} {
		t.Run(fmt.Sprint(legacyErr), func(t *testing.T) {
			client := fakeClient(gslb(t, "k8gb.io/v1beta1", "current", `"status":{"hosts":"one.example.test"}`))
			client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.GetResource() == legacyGSLB && legacyErr != nil {
					return true, nil, legacyErr
				}
				return false, nil, nil
			})
			snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Objects) != 1 || snapshot.Objects[0].Resource.Name != "current" {
				t.Fatalf("canonical objects lost: %+v", snapshot)
			}
			wantComplete := legacyErr == nil || apierrors.IsNotFound(legacyErr)
			if snapshot.ObjectsComplete != wantComplete {
				t.Fatalf("objectsComplete=%v, want %v", snapshot.ObjectsComplete, wantComplete)
			}
		})
	}
}

func TestOptionalTimeoutDoesNotChangeInventoryCompleteness(t *testing.T) {
	client := fakeClient(gslb(t, "k8gb.io/v1beta1", "current", `"status":{"hosts":"one.example.test"}`))
	client.PrependReactor("list", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		if a.GetResource() != canonicalGSLB && a.GetResource() != legacyGSLB {
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil
	})
	snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
	if err != nil || !snapshot.ObjectsComplete || len(snapshot.Objects) != 1 || len(snapshot.Warnings) != 4 {
		t.Fatalf("optional timeout affected primary inventory: %+v %v", snapshot, err)
	}
}

func TestMigrationDedupRequiresEvidence(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprint(marked), func(t *testing.T) {
			current := gslb(t, "k8gb.io/v1beta1", "same", `"status":{"hosts":"same.example.test"}`)
			old := gslb(t, "k8gb.absa.oss/v1beta1", "same", `"status":{"hosts":"same.example.test"}`)
			if marked {
				old.SetLabels(map[string]string{"k8gb.io/migrated-to-k8gb-io": "true"})
			}
			snapshot, err := collector(t, fakeClient(current, old)).Snapshot(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if marked {
				want = 1
			}
			if len(snapshot.Objects) != want {
				t.Fatalf("objects=%d, want %d", len(snapshot.Objects), want)
			}
		})
	}
	old := gslb(t, "k8gb.absa.oss/v1beta1", "orphan-marker", `"status":{"hosts":"old.example.test"}`)
	old.SetLabels(map[string]string{"k8gb.io/migrated-to-k8gb-io": "true"})
	snapshot, err := collector(t, fakeClient(old)).Snapshot(context.Background(), "app")
	if err != nil || len(snapshot.Objects) != 1 {
		t.Fatalf("orphan marker must stay visible: %+v %v", snapshot, err)
	}
}

func TestPartialDataUnknown(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(fmt.Sprint(absent), func(t *testing.T) {
			g := gslb(t, "k8gb.io/v1beta1", "partial", `"status":{"hosts":["one.example.test","two.example.test"],"servers":[{"host":"one.example.test","services":[{"name":"missing"}]}]}`)
			client := fakeClient(g)
			client.PrependReactor("list", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
				if a.GetResource() == canonicalGSLB || a.GetResource() == legacyGSLB {
					return false, nil, nil
				}
				if absent {
					return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
				}
				return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "", errors.New("denied"))
			})
			snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Warnings) != 4 || len(snapshot.Objects) != 2 || !snapshot.ObjectsComplete {
				t.Fatalf("partial snapshot: %+v", snapshot)
			}
			one := snapshot.Objects[0]
			if one.Health != "Unknown" || one.DNSObserved || len(one.Backends) != 1 || one.Backends[0].Health != "Unknown" || one.Backends[0].EndpointDataAvailable {
				t.Fatalf("unknown lost: %+v", one)
			}
		})
	}
}

func TestNamespaceScope(t *testing.T) {
	client := fakeClient(object(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"zeta"}}`), object(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"app"}}`))
	c := collector(t, client)
	list, err := c.Namespaces(context.Background())
	if err != nil || !reflect.DeepEqual(list.Namespaces, []string{"app", "zeta"}) {
		t.Fatalf("all namespaces: %+v %v", list, err)
	}
	client.ClearActions()
	c = collector(t, client, "app")
	list, err = c.Namespaces(context.Background())
	if err != nil || !reflect.DeepEqual(list.Namespaces, []string{"app"}) || len(client.Actions()) != 0 {
		t.Fatalf("allowlist: %+v %v", list, err)
	}
	for _, ns := range []string{"", "*", "ALL", "app/../other", "other"} {
		if _, err := c.Snapshot(context.Background(), ns); err == nil {
			t.Fatalf("scope accepted: %q", ns)
		}
	}
	if len(client.Actions()) != 0 {
		t.Fatal("denied scope reached Kubernetes")
	}
}

func TestPaginationAndBoundedRequests(t *testing.T) {
	client := fakeClient()
	c := collector(t, client)
	c.opts.PageSize, c.opts.MaxPages = 1, 2
	page := 0
	client.PrependReactor("list", "namespaces", func(a ktesting.Action) (bool, runtime.Object, error) {
		// client-go v0.33's dynamic fake drops ListOptions. Check the actual
		// request parameters with the in-memory HTTP transport test instead.
		page++
		name, next := "first", "page2"
		if page == 2 {
			name, next = "second", ""
		}
		list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*object(t, fmt.Sprintf(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":%q}}`, name))}}
		list.SetContinue(next)
		return true, list, nil
	})
	result, err := c.Namespaces(context.Background())
	if err != nil || !reflect.DeepEqual(result.Namespaces, []string{"first", "second"}) {
		t.Fatalf("pagination: %+v %v", result, err)
	}
	c.opts.MaxPages = 1
	page = 0
	if _, err := c.Namespaces(context.Background()); err == nil {
		t.Fatal("page bound ignored")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client.ClearActions()
	if _, err := c.Namespaces(canceled); !errors.Is(err, context.Canceled) || len(client.Actions()) != 0 {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestCollectionBoundsValidation(t *testing.T) {
	if _, err := NewCollector(fakeClient(), Options{ClusterID: "test", RequestTimeout: time.Minute, SnapshotTimeout: time.Second}); err == nil {
		t.Fatal("invalid bounds accepted")
	}
}

func TestResourceRefUniqueAndNamespaceBound(t *testing.T) {
	g := gslb(t, "k8gb.io/v1beta1", "web", `"spec":{"resourceRef":{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","matchLabels":{"app":"web"}}},"status":{"hosts":"one.example.test"}`)
	one := object(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"namespace":"app","name":"one","labels":{"app":"web"}},"spec":{"rules":[{"host":"one.example.test","http":{"paths":[{"backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`)
	two := one.DeepCopy()
	two.SetName("two")
	c := collector(t, fakeClient(g, one, two))
	snapshot, err := c.Snapshot(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Objects[0].Target != nil || len(snapshot.Objects[0].Backends) != 0 || !strings.Contains(strings.Join(snapshot.Objects[0].Warnings, " "), "2 objects") {
		t.Fatalf("ambiguous match: %+v", snapshot.Objects[0])
	}
	c = collector(t, fakeClient(g, one))
	snapshot, err = c.Snapshot(context.Background(), "app")
	if err != nil || snapshot.Objects[0].Target == nil || snapshot.Objects[0].Target.Name != "one" {
		t.Fatalf("unique match: %+v %v", snapshot, err)
	}
	_ = unstructured.SetNestedField(g.Object, "other", "spec", "resourceRef", "namespace")
	c = collector(t, fakeClient(g, one))
	snapshot, err = c.Snapshot(context.Background(), "app")
	if err != nil || snapshot.Objects[0].Target != nil {
		t.Fatalf("cross-namespace match: %+v %v", snapshot, err)
	}
	assertReadOnly(t, c.client.(*fake.FakeDynamicClient), "app")
}

func TestHTTPRouteBackendReferences(t *testing.T) {
	g := gslb(t, "k8gb.io/v1beta1", "web", `"spec":{"resourceRef":{"apiVersion":"gateway.networking.k8s.io/v1","kind":"HTTPRoute","name":"route"}},"status":{}`)
	route := object(t, `{"apiVersion":"gateway.networking.k8s.io/v1","kind":"HTTPRoute","metadata":{"namespace":"app","name":"route"},"spec":{"hostnames":["one.example.test","two.example.test"],"rules":[{"backendRefs":[{"name":"web","port":80},{"name":"remote","namespace":"other","port":8080},{"name":"custom","kind":"Custom","group":"example.test"}]}]}}`)
	client := fakeClient(g, route)
	snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Objects) != 2 {
		t.Fatalf("hosts: %+v", snapshot.Objects)
	}
	for _, obj := range snapshot.Objects {
		if obj.Target == nil || obj.Target.Kind != "HTTPRoute" || len(obj.Backends) != 2 || obj.Health != "Unknown" || len(obj.Warnings) < 2 {
			t.Fatalf("HTTPRoute: %+v", obj)
		}
		if obj.Backends[1].Namespace != "other" || obj.Backends[1].Health != "Unknown" || obj.Backends[1].EndpointDataAvailable {
			t.Fatalf("cross-namespace backend: %+v", obj)
		}
	}
	assertReadOnly(t, client, "app")
}
