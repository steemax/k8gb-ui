package agent

import (
	"testing"

	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func TestExcludedBackendIsNotReportedAsMissing(t *testing.T) {
	g := gslb(t, "k8gb.io/v1beta1", "global", `"spec":{"resourceRef":{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","name":"generated"}},"status":{"serviceHealth":{"web.example.test":"NotFound"}}`)
	g.SetAnnotations(map[string]string{mutation.SiteExcludedAnnotation: "true", mutation.ExposedIPsAnnotation: "192.0.2.1"})
	g.SetLabels(map[string]string{traefik.OwnerLabel: "parent", traefik.ManagedLabel: "traefik-adapter"})
	controller := true
	g.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "traefik.io/v1alpha1", Kind: "IngressRoute", Name: "web", UID: types.UID("parent"), Controller: &controller}})
	i := object(t, `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"namespace":"app","name":"generated"},"spec":{"rules":[{"host":"web.example.test","http":{"paths":[{"backend":{"service":{"name":"k8gb-ui-excluded-test","port":{"number":80}}}}]}}]}}`)
	rules, _, _ := unstructured.NestedSlice(i.Object, "spec", "rules")
	paths, _, _ := unstructured.NestedSlice(rules[0].(map[string]interface{}), "http", "paths")
	checkErr := unstructured.SetNestedField(paths[0].(map[string]interface{}), traefik.ExcludedServiceName("parent"), "backend", "service", "name")
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	_ = unstructured.SetNestedSlice(rules[0].(map[string]interface{}), paths, "http", "paths")
	_ = unstructured.SetNestedSlice(i.Object, rules, "spec", "rules")
	i.SetLabels(g.GetLabels())
	i.SetAnnotations(map[string]string{mutation.SiteExcludedAnnotation: "true"})
	data := map[schema.GroupVersionResource]collection{ingresses: {available: true, items: []unstructured.Unstructured{*i}}, services: {available: true}, endpointSlices: {available: true}}
	objects := buildObjects(*g, data)
	if !objects[0].Traffic.Excluded || objects[0].Health != "Excluded" || objects[0].Backends[0].Health != "Excluded" || len(objects[0].Traffic.IPs) != 1 {
		t.Fatalf("excluded=%+v", objects)
	}
	// Native resources cannot claim an exclusion merely by copying an annotation.
	g.SetOwnerReferences(nil)
	objects = buildObjects(*g, data)
	if objects[0].Traffic.Excluded || objects[0].Health != "NotFound" || objects[0].Backends[0].Health != "NotFound" {
		t.Fatalf("unverified marker hid fault: %+v", objects)
	}
}
