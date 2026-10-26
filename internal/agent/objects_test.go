package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestDNSRequiresAssociationAndExactHost(t *testing.T) {
	g := gslb(t, "k8gb.io/v1beta1", "web", `"status":{"hosts":"one.example.test"}`)
	for _, test := range []struct {
		label, name, uid, host string
		observed               bool
	}{
		{"same name", "web", "", "one.example.test", true},
		{"unrelated name", "unrelated", "", "one.example.test", false},
		{"wrong host", "web", "", "other.example.test", false},
		{"localtargets", "web", "", "localtargets.one.example.test", false},
		{"owner UID matches", "different", "k8gb.io/v1beta1-web", "one.example.test", true},
		{"wrong owner overrides name", "web", "different-uid", "one.example.test", false},
	} {
		t.Run(test.label, func(t *testing.T) {
			dns := object(t, `{"apiVersion":"externaldns.k8s.io/v1alpha1","kind":"DNSEndpoint","metadata":{"namespace":"app","name":"placeholder"},"spec":{"endpoints":[{"dnsName":"placeholder","recordType":"A","targets":["192.0.2.1"]}]}}`)
			dns.SetName(test.name)
			_ = unstructured.SetNestedSlice(dns.Object, []interface{}{map[string]interface{}{"dnsName": test.host, "recordType": "A", "targets": []interface{}{"192.0.2.1"}}}, "spec", "endpoints")
			if test.uid != "" {
				_ = unstructured.SetNestedSlice(dns.Object, []interface{}{map[string]interface{}{"apiVersion": "k8gb.io/v1beta1", "kind": "Gslb", "name": "web", "uid": test.uid}}, "metadata", "ownerReferences")
			}
			targets, observed := resolveDNS(*g, nil, "one.example.test", collection{available: true, items: []unstructured.Unstructured{*dns}})
			if observed != test.observed || (len(targets) == 1) != test.observed {
				t.Fatalf("association: %v %v", targets, observed)
			}
			dns.SetNamespace("other")
			if targets, observed := resolveDNS(*g, nil, "one.example.test", collection{available: true, items: []unstructured.Unstructured{*dns}}); observed || len(targets) != 0 {
				t.Fatal("cross-namespace DNS matched")
			}
		})
	}
}

func TestMissingEndpointDataOverridesMissingService(t *testing.T) {
	for _, endpointErr := range []error{apierrors.NewNotFound(endpointSlices.GroupResource(), ""), apierrors.NewForbidden(endpointSlices.GroupResource(), "", errors.New("denied"))} {
		client := fakeClient(gslb(t, "k8gb.io/v1beta1", "web", `"status":{"hosts":"one.example.test","servers":[{"host":"one.example.test","services":[{"name":"missing"}]}]}`))
		client.PrependReactor("list", "endpointslices", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, endpointErr })
		snapshot, err := collector(t, client).Snapshot(context.Background(), "app")
		if err != nil {
			t.Fatal(err)
		}
		backend := snapshot.Objects[0].Backends[0]
		if backend.Health != "Unknown" || backend.EndpointDataAvailable || len(snapshot.Warnings) == 0 {
			t.Fatalf("missing endpoint data: %+v", snapshot)
		}
	}
}

func TestEmptySlicesDoNotMeanMissingService(t *testing.T) {
	backend := v1.Backend{Namespace: "app", Service: "web", Health: "Unknown"}
	data := collection{available: true}
	if result := backendStatus(backend, data, collection{}); result.Health != "Unknown" {
		t.Fatalf("service is unknown: %+v", result)
	}
	if result := backendStatus(backend, data, data); result.Health != "NotFound" {
		t.Fatalf("service verified absent: %+v", result)
	}
	data.items = []unstructured.Unstructured{*object(t, `{"apiVersion":"v1","kind":"Service","metadata":{"namespace":"app","name":"web"}}`)}
	if result := backendStatus(backend, collection{available: true}, data); result.Health != "Unhealthy" {
		t.Fatalf("service exists without endpoints: %+v", result)
	}
}

func TestEmbeddedIngressWithoutStatus(t *testing.T) {
	g := gslb(t, "k8gb.absa.oss/v1beta1", "web", `"spec":{"resourceRef":{},"ingress":{"rules":[{"host":"one.example.test","http":{"paths":[{"backend":{"service":{"name":"web","port":{"number":80}}}}]}}]}}`)
	objects := buildObjects(*g, map[schema.GroupVersionResource]collection{})
	if len(objects) != 1 || objects[0].Hostname != "one.example.test" || objects[0].Health != "Unknown" || len(objects[0].Backends) != 1 || objects[0].Backends[0].Port != "80" {
		t.Fatalf("embedded ingress: %+v", objects)
	}
}

func TestHostRepresentations(t *testing.T) {
	for _, raw := range []interface{}{"ONE.EXAMPLE.TEST, two.example.test", []interface{}{"ONE.EXAMPLE.TEST", "two.example.test"}} {
		g := gslb(t, "k8gb.io/v1beta1", "web", `"status":{}`)
		_ = unstructured.SetNestedField(g.Object, raw, "status", "hosts")
		objects := buildObjects(*g, map[schema.GroupVersionResource]collection{})
		hosts := []string{}
		for _, obj := range objects {
			hosts = append(hosts, obj.Hostname)
		}
		if !reflect.DeepEqual(hosts, []string{"one.example.test", "two.example.test"}) {
			t.Fatalf("hosts: %v", hosts)
		}
	}
}
