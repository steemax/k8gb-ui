package agent

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"testing"
)

func TestNativeWeightMapRestoresWeightedFormMode(t *testing.T) {
	resource := unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "k8gb.io/v1beta1", "kind": "Gslb", "metadata": map[string]interface{}{"name": "weighted", "namespace": "demo"}, "spec": map[string]interface{}{"strategy": map[string]interface{}{"type": "roundRobin", "weight": map[string]interface{}{"east": int64(1), "west": int64(5)}}}, "status": map[string]interface{}{"hosts": "weighted.apps.example.test"}}}
	objects := buildObjects(resource, map[schema.GroupVersionResource]collection{})
	if len(objects) != 1 || objects[0].Policy.Strategy != "weightedRoundRobin" || objects[0].Policy.Weights["west"] != 5 {
		t.Fatalf("weighted mode lost: %+v", objects)
	}
}
