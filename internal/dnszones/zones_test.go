package dnszones

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, raw := range []string{"example.test:common.example.test:30; example.test:site.example.test:30;", "example.test:common.example.test;example.test:site.example.test"} {
		zones, err := Parse(raw)
		if err != nil || strings.Join(zones, ",") != "common.example.test,site.example.test" {
			t.Fatalf("%v %v", zones, err)
		}
	}
	for _, raw := range []string{"", "example.test:evil.test:30", "example.test:*.example.test:30", "example.test:example.test:30", "example.test:a.example.test:-1", "example.test:a.example.test:no", "example.test:a.example.test:30:40", "example.test:a.example.test:30;;"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if Contains("example.test", "evilexample.test") || Contains("example.test", "example.test") {
		t.Fatal("unsafe suffix match")
	}
}

func TestReadLiteralOnly(t *testing.T) {
	for _, tt := range []struct {
		name, env string
		valid     bool
	}{
		{"literal", `[{"name":"DNS_ZONES","value":"example.test:apps.example.test:30"}]`, true},
		{"legacy", `[{"name":"DNS_ZONE","value":"apps.example.test"},{"name":"EDGE_DNS_ZONE","value":"example.test"}]`, true},
		{"secret", `[{"name":"DNS_ZONES","valueFrom":{"secretKeyRef":{"name":"not-read","key":"zones"}}}]`, false},
		{"expansion", `[{"name":"DNS_ZONES","value":"example.test:$(ZONE):30"}]`, false},
		{"duplicates", `[{"name":"DNS_ZONES","value":"example.test:apps.example.test:30"},{"name":"DNS_ZONES","value":"example.test:other.example.test:30"}]`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			if err := obj.UnmarshalJSON([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"system","name":"controller","uid":"controller-uid","resourceVersion":"10"},"spec":{"template":{"spec":{"containers":[{"name":"k8gb","env":` + tt.env + `}]}}}}`)); err != nil {
				t.Fatal(err)
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), obj)
			reader := &Reader{Client: client, Config: Config{Namespace: "system", Deployment: "controller"}}
			result, ref, err := reader.Read(context.Background())
			if (err == nil) != tt.valid || result.Available != tt.valid {
				t.Fatalf("%+v %v", result, err)
			}
			if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" || client.Actions()[0].GetResource() != Deployments {
				t.Fatal("unexpected Kubernetes read")
			}
			if !reader.Matches(ref) || ref.UID != "controller-uid" {
				t.Fatal("missing source pin")
			}
		})
	}
}
