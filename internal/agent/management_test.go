package agent

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestManagementUsesOwnershipNotResourceName(t *testing.T) {
	for _, test := range []struct{ name, owner, marker, uid, want string }{
		{"gslb-generated-looking", "", "", "", "native"},
		{"plain-name", "IngressRoute", "traefik-adapter", "source-uid", "traefik"},
		{"wrong-label-owner", "IngressRoute", "traefik-adapter", "wrong-uid", "external"},
		{"unknown-route-controller", "IngressRoute", "", "", "external"},
		{"custom-controller", "CustomRoute", "", "", "external"},
		{"orphan-marker", "", "traefik-adapter", "", "external"},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := gslb(t, "k8gb.io/v1beta1", test.name, `"status":{"hosts":"app.example.test"}`)
			if test.owner != "" {
				g.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "traefik.io/v1alpha1", Kind: test.owner, Name: "source", UID: "source-uid"}})
			}
			if test.marker != "" {
				g.SetLabels(map[string]string{"k8gb-ui.io/managed-by": test.marker, "k8gb-ui.io/route-uid": test.uid})
			}
			objects := buildObjects(*g, map[schema.GroupVersionResource]collection{})
			if len(objects) != 1 || objects[0].Management != test.want {
				t.Fatalf("management classification: %+v", objects)
			}
		})
	}
}
