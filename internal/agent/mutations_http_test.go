package agent

import (
	"net/http/httptest"
	"strings"
	"testing"

	"k8gb-ui/internal/mutation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestReadTokenCannotMutate(t *testing.T) {
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	collector, err := NewCollector(client, Options{ClusterID: "east", AllowedNamespaces: []string{"demo"}})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := mutation.NewManager(client, mutation.Options{ClusterID: "east", Enabled: true, SigningKey: []byte(strings.Repeat("k", 32)), AllowedNamespaces: []string{"demo"}, Providers: map[string]mutation.Provider{"native": mutation.NewNativeProvider(client, []string{"east"})}})
	if err != nil {
		t.Fatal(err)
	}
	readToken, writeToken := strings.Repeat("r", 32), strings.Repeat("w", 32)
	handler, err := NewHandler(collector, ServerOptions{ListenAddress: "127.0.0.1:8081", Token: readToken, WriteToken: writeToken, Mutations: manager})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", readToken} {
		r := httptest.NewRequest("POST", "/api/v1/mutations/apply", strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("read credentials reached mutation API: %d", w.Code)
		}
	}
	if len(client.Actions()) != 0 {
		t.Fatal("unauthorized request made Kubernetes calls")
	}
}
