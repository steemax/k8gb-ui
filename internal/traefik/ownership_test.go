package traefik

import (
	"context"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestHostnameOwnershipRequiresExplicitHandover(t *testing.T) {
	ctx, cfg := context.Background(), config()
	parent := route(t, map[string]string{"gslb": "true"})
	s, err := settings(parent, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"k8gb.io/v1beta1", "k8gb.absa.oss/v1beta1"} {
		for _, form := range []string{"embedded", "status", "reference"} {
			t.Run(version+"/"+form, func(t *testing.T) {
				g := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": version, "kind": "Gslb", "metadata": map[string]interface{}{"namespace": "app", "name": "independent", "uid": "native-uid"}, "spec": map[string]interface{}{}}}
				objects := []runtime.Object{parent, g}
				switch form {
				case "embedded":
					mustSet(t, g, []interface{}{map[string]interface{}{"host": s.target.host}}, "spec", "ingress", "rules")
				case "status":
					mustSet(t, g, strings.ToUpper(s.target.host)+".", "status", "hosts")
				case "reference":
					mustSet(t, g, map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "name": "live-route"}, "spec", "resourceRef")
					ing := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]interface{}{"namespace": "app", "name": "live-route"}, "spec": map[string]interface{}{"rules": []interface{}{map[string]interface{}{"host": s.target.host}}}}}
					objects = append(objects, ing)
				}
				c := client(objects...)
				err := (Reconciler{Client: c, Config: cfg}).uniqueHosts(ctx, parent, s, cfg)
				if err == nil || !strings.Contains(err.Error(), "handover") {
					t.Fatalf("expected an ownership conflict: %v", err)
				}
				if len(writes(c)) != 0 {
					t.Fatal("hostname preflight wrote resources")
				}
			})
		}
	}
	// Matching labels alone cannot bypass the owner UID/controller proof.
	ownedGslb := newChild(parent, "k8gb.io/v1beta1", "Gslb", "owned")
	mustSet(t, ownedGslb, s.target.host, "status", "hosts")
	c := client(parent, ownedGslb)
	if err := (Reconciler{Client: c, Config: cfg}).uniqueHosts(ctx, parent, s, cfg); err != nil {
		t.Fatal(err)
	}
	ownedGslb.SetOwnerReferences([]metav1.OwnerReference{})
	c = client(parent, ownedGslb)
	if err := (Reconciler{Client: c, Config: cfg}).uniqueHosts(ctx, parent, s, cfg); err == nil {
		t.Fatal("labels without valid ownership bypassed preflight")
	}
}

func TestProviderDoesNotEnableDuplicateNativeHostname(t *testing.T) {
	parent := route(t, map[string]string{"gslb": "false"})
	cfg := config()
	probe := parent.DeepCopy()
	probe.SetAnnotations(map[string]string{"gslb": "true"})
	s, err := settings(probe, cfg)
	if err != nil {
		t.Fatal(err)
	}
	g := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "k8gb.io/v1beta1", "kind": "Gslb", "metadata": map[string]interface{}{"namespace": "app", "name": "native"}, "status": map[string]interface{}{"hosts": s.target.host}}}
	c := client(parent, g)
	_, err = NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "enable", Resource: reference(parent), Policy: &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 30}})
	if err == nil || !strings.Contains(err.Error(), "handover") {
		t.Fatalf("expected ownership error before enabling: %v", err)
	}
	if len(writes(c)) != 0 {
		t.Fatal("failed preflight must not write")
	}
}
