package mutation

import (
	"context"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/dnszones"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestCreationZoneGuards(t *testing.T) {
	controller := &unstructured.Unstructured{}
	if err := controller.UnmarshalJSON([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"controller","namespace":"system","uid":"controller-id","resourceVersion":"7"},"spec":{"template":{"spec":{"containers":[{"name":"k8gb","env":[{"name":"DNS_ZONES","value":"example.test:apps.example.test:30;example.test:shared.example.test:30"}]}]}}}}`)); err != nil {
		t.Fatal(err)
	}
	ingress := &unstructured.Unstructured{}
	if err := ingress.UnmarshalJSON([]byte(`{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"app","namespace":"app","uid":"ingress-id","resourceVersion":"2"},"spec":{"rules":[{"host":"web.apps.example.test"}]}}`)); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), controller, ingress)
	reader := &dnszones.Reader{Client: client, Config: dnszones.Config{Namespace: "system", Deployment: "controller"}}
	manager := &Manager{client: client, opts: Options{AllowedNamespaces: []string{"app"}, DNSZones: reader}}
	target := objectReference(ingress)
	req := v1.MutationRequest{Provider: "native", Operation: "create", Target: &target, DNSZone: "apps.example.test"}
	prepared := Prepared{}
	if err := manager.creationZone(context.Background(), req, &prepared); err != nil {
		t.Fatal(err)
	}
	if len(prepared.Dependencies) != 1 || !reader.Matches(prepared.Dependencies[0]) {
		t.Fatal("controller source not pinned")
	}
	if err := manager.checkDependencies(context.Background(), "app", prepared.Dependencies); err != nil {
		t.Fatalf("explicit controller outside application allowlist: %v", err)
	}
	dep := prepared.Dependencies[0]
	dep.Name = "other"
	if err := manager.checkDependencies(context.Background(), "app", []v1.ResourceReference{dep}); err == nil {
		t.Fatal("arbitrary Deployment allowed")
	}
	for _, zone := range []string{"", "example.test", "shared.example.test", "evil.test"} {
		bad := req
		bad.DNSZone = zone
		if err := manager.creationZone(context.Background(), bad, &Prepared{}); err == nil {
			t.Fatalf("accepted native zone %q", zone)
		}
	}
	req.Provider, req.Operation, req.Target = "traefik", "enable", nil
	req.Traefik = &v1.TraefikOptions{Hostname: "api.apps.example.test"}
	if err := manager.creationZone(context.Background(), req, &Prepared{}); err != nil {
		t.Fatal(err)
	}
	req.Traefik.Hostname = "api.evil.test"
	if err := manager.creationZone(context.Background(), req, &Prepared{}); err == nil {
		t.Fatal("external host")
	}
	req.Traefik.Hostname = "api.apps.example.test"
	req.Traefik.Local = true
	if err := manager.creationZone(context.Background(), req, &Prepared{}); err == nil {
		t.Fatal("local creation")
	}
	req.Operation = "update"
	if err := manager.creationZone(context.Background(), req, &Prepared{}); err == nil {
		t.Fatal("zone on update")
	}
}
