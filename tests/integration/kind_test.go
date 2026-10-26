//go:build integration

// These tests never create or delete a cluster. Run against an isolated kind:
// K8GB_UI_E2E=1 K8GB_UI_KUBECONFIG=/absolute/path/kind.kubeconfig \
// GOCACHE=/private/tmp/k8gb-ui-go-cache GOPROXY=off \
// go test -tags=integration -count=1 -v ./tests/integration
// Add K8GB_UI_E2E_WEBHOOK_IMAGE=k8gb-ui-webhook:e2e after loading that image
// into kind to include real TLS admission. Required CRDs: both upstream Gslb
// groups, Traefik IngressRoute/Middleware/TLSOption, and optionally DNSEndpoint.
// Scope: real Kubernetes schema, dry-run, admission and explicit reconciliation;
// no k8gb DNS controller, DNS traffic or propagation assertions are implied.
package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	namespaces   = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	ingresses    = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
	gslbs        = schema.GroupVersionResource{Group: "k8gb.io", Version: "v1beta1", Resource: "gslbs"}
	routes       = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}
	middlewares  = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "middlewares"}
	dnsEndpoints = schema.GroupVersionResource{Group: "externaldns.k8s.io", Version: "v1alpha1", Resource: "dnsendpoints"}
)

type fixture struct {
	client dynamic.Interface
	ns     string
	ctx    context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if os.Getenv("K8GB_UI_E2E") != "1" {
		t.Skip("real kind tests require K8GB_UI_E2E=1 and K8GB_UI_KUBECONFIG")
	}
	path := os.Getenv("K8GB_UI_KUBECONFIG")
	if path == "" || !filepath.IsAbs(path) {
		t.Fatal("K8GB_UI_KUBECONFIG must explicitly name an absolute kubeconfig; default kubeconfig is never used")
	}
	raw, err := clientcmd.LoadFromFile(path)
	check(t, err)
	if !strings.HasPrefix(raw.CurrentContext, "kind-k8gb-ui-e2e-") {
		t.Fatalf("refusing context %q: expected kind-k8gb-ui-e2e- prefix", raw.CurrentContext)
	}
	selected := raw.Contexts[raw.CurrentContext]
	if selected == nil || raw.Clusters[selected.Cluster] == nil {
		t.Fatal("selected context or cluster is missing")
	}
	cluster := raw.Clusters[selected.Cluster]
	endpoint, err := url.Parse(cluster.Server)
	check(t, err)
	ip := net.ParseIP(endpoint.Hostname())
	if endpoint.Scheme != "https" || ip == nil || ip.To4() == nil || !ip.IsLoopback() || !strings.HasPrefix(endpoint.Hostname(), "127.") || cluster.ProxyURL != "" || cluster.InsecureSkipTLSVerify {
		t.Fatalf("refusing non-TLS/non-127-loopback API endpoint %q or proxy/insecure TLS", cluster.Server)
	}
	if auth := raw.AuthInfos[selected.AuthInfo]; auth != nil && (auth.Exec != nil || auth.AuthProvider != nil) {
		t.Fatal("exec/auth-provider credentials are not permitted for isolated integration tests")
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, raw.CurrentContext, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	check(t, err)
	config.Timeout, config.QPS, config.Burst = 10*time.Second, 25, 50
	client, err := dynamic.NewForConfig(config)
	check(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	ns, err := client.Resource(namespaces).Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{
			"generateName": "k8gb-ui-e2e-", "labels": map[string]interface{}{"k8gb-ui.io/e2e": "true"},
		},
	}}, metav1.CreateOptions{})
	check(t, err)
	f := &fixture{client: client, ns: ns.GetName(), ctx: ctx}
	t.Logf("isolated context=%s namespace=%s", raw.CurrentContext, f.ns)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 60*time.Second)
		defer done()
		uid := ns.GetUID()
		err := client.Resource(namespaces).Delete(cleanup, f.ns, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("cleanup own namespace %s: %v", f.ns, err)
			return
		}
		for cleanup.Err() == nil {
			_, err = client.Resource(namespaces).Get(cleanup, f.ns, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Errorf("own namespace %s did not terminate: %v", f.ns, cleanup.Err())
	})
	return f
}

func (f *fixture) api(gvr schema.GroupVersionResource) dynamic.ResourceInterface {
	return f.client.Resource(gvr).Namespace(f.ns)
}

func (f *fixture) manager(t *testing.T, allowed []string, cfg traefik.Config, audit func(v1.AuditEvent)) *mutation.Manager {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	check(t, err)
	m, err := mutation.NewManager(f.client, mutation.Options{
		ClusterID: "east", Enabled: true, SigningKey: key, AllowedNamespaces: allowed, Audit: audit,
		Providers: map[string]mutation.Provider{
			"traffic": mutation.NewTrafficProvider(f.client),
			"native":  mutation.NewNativeProvider(f.client, []string{"east", "west"}),
			"traefik": traefik.NewProvider(f.client, cfg),
		},
	})
	check(t, err)
	return m
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func expectFailure(t *testing.T, err error, status int, codes ...string) {
	t.Helper()
	var failure *mutation.Error
	if !errors.As(err, &failure) || failure.Status != status {
		t.Fatalf("expected mutation error HTTP %d (%v), got %T: %v", status, codes, err, err)
	}
	for _, code := range codes {
		if failure.Code == code {
			return
		}
	}
	t.Fatalf("expected error code in %v, got %s: %v", codes, failure.Code, err)
}

func ref(u *unstructured.Unstructured) v1.ResourceReference {
	return v1.ResourceReference{APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()), ResourceVersion: u.GetResourceVersion()}
}

func object(apiVersion, kind, ns, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]interface{}{"namespace": ns, "name": name}, "spec": spec}}
}

func (f *fixture) createIngress(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	u := object("networking.k8s.io/v1", "Ingress", f.ns, name, map[string]interface{}{
		"rules": []interface{}{map[string]interface{}{"host": name + ".apps.example.test", "http": map[string]interface{}{"paths": []interface{}{
			map[string]interface{}{"path": "/", "pathType": "Prefix", "backend": map[string]interface{}{"service": map[string]interface{}{"name": "backend", "port": map[string]interface{}{"number": int64(8080)}}}},
		}}}},
	})
	u, err := f.api(ingresses).Create(f.ctx, u, metav1.CreateOptions{})
	check(t, err)
	return u
}

func (f *fixture) get(t *testing.T, gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := f.api(gvr).Get(f.ctx, name, metav1.GetOptions{})
	check(t, err)
	return u
}

func (f *fixture) absent(t *testing.T, gvr schema.GroupVersionResource, name string) {
	t.Helper()
	_, err := f.api(gvr).Get(f.ctx, name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected %s/%s absent, got %v", gvr.Resource, name, err)
	}
}

func (f *fixture) unchanged(t *testing.T, gvr schema.GroupVersionResource, before *unstructured.Unstructured) {
	t.Helper()
	after := f.get(t, gvr, before.GetName())
	if !reflect.DeepEqual(before.Object, after.Object) {
		t.Fatalf("%s/%s changed without an accepted apply (RV %s -> %s)", gvr.Resource, before.GetName(), before.GetResourceVersion(), after.GetResourceVersion())
	}
}

func suffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, err := rand.Read(b)
	check(t, err)
	return hex.EncodeToString(b)
}

func nested(t *testing.T, u *unstructured.Unstructured, want interface{}, fields ...string) {
	t.Helper()
	got, found, err := unstructured.NestedFieldNoCopy(u.Object, fields...)
	check(t, err)
	if !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("%s %s: got %#v, want %#v", u.GetName(), strings.Join(fields, "."), got, want)
	}
}

func applied(t *testing.T, f *fixture, m *mutation.Manager, request v1.MutationRequest) v1.MutationResult {
	t.Helper()
	plan, err := m.Plan(f.ctx, request)
	check(t, err)
	result, err := m.Apply(f.ctx, plan)
	check(t, err)
	if result.Status != "applied" || result.Reconciliation != "pending" || result.Resource.UID == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	return result
}

func (f *fixture) createRequest(t *testing.T, group, name string) v1.MutationRequest {
	t.Helper()
	target := ref(f.createIngress(t, name+"-target"))
	return v1.MutationRequest{Provider: "native", Operation: "create", Actor: "integration-user", Reason: "isolated kind acceptance", Resource: v1.ResourceReference{APIVersion: group + "/v1beta1", Kind: "Gslb", Namespace: f.ns, Name: name}, Target: &target, Policy: &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 60}}
}
