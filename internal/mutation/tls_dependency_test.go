package mutation

import (
	"context"
	"testing"

	v1 "k8gb-ui/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

type tlsDependencyProvider struct{ approved bool }

func (tlsDependencyProvider) Operations() []string { return []string{"update"} }
func (tlsDependencyProvider) Prepare(context.Context, v1.MutationRequest) (Prepared, error) {
	return Prepared{}, nil
}
func (p tlsDependencyProvider) AllowsReadDependency(ref v1.ResourceReference) bool {
	return p.approved && ref.Kind == "TLSOption" && ref.Namespace == "tls-system"
}

func TestTLSOptionReadDependencyCannotBecomeWriteTarget(t *testing.T) {
	ref := v1.ResourceReference{APIVersion: "traefik.io/v1alpha1", Kind: "TLSOption", Namespace: "tls-system", Name: "client-auth", UID: "policy-uid", ResourceVersion: "7"}
	if _, err := resourceGVR(ref); err == nil {
		t.Fatal("TLSOption became a mutation target")
	}
	gvr, err := dependencyGVR(ref, "app")
	if err != nil {
		t.Fatal(err)
	}
	policy := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": ref.APIVersion, "kind": ref.Kind, "metadata": map[string]interface{}{"namespace": ref.Namespace, "name": ref.Name, "uid": ref.UID, "resourceVersion": ref.ResourceVersion}}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), policy)
	for _, approved := range []bool{false, true} {
		manager, err := NewManager(client, Options{ClusterID: "east", AllowedNamespaces: []string{"app"}, Providers: map[string]Provider{"traefik": tlsDependencyProvider{approved: approved}}})
		if err != nil {
			t.Fatal(err)
		}
		err = manager.checkDependencies(context.Background(), "app", []v1.ResourceReference{ref})
		if (err == nil) != approved {
			t.Fatalf("cross-namespace read approval ignored: %v", err)
		}
		if approved {
			changed := policy.DeepCopy()
			changed.SetResourceVersion("8")
			_, err = client.Resource(gvr).Namespace(ref.Namespace).Update(context.Background(), changed, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err = manager.checkDependencies(context.Background(), "app", []v1.ResourceReference{ref}); err == nil {
				t.Fatal("stale TLSOption dependency accepted")
			}
		}
	}
	secret := ref
	secret.APIVersion = "v1"
	secret.Kind = "Secret"
	if _, err := dependencyGVR(secret, "app"); err == nil {
		t.Fatal("Secret reads added to mutation dependencies")
	}
}
