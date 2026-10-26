package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

const testToken = "12345678901234567890123456789012"

func TestHTTPAuthenticationMethodsAndScope(t *testing.T) {
	client := fakeClient(gslb(t, "k8gb.io/v1beta1", "web", `"status":{"hosts":"one.example.test"}`))
	handler, err := NewHandler(collector(t, client, "app"), ServerOptions{Token: testToken, ListenAddress: ":8081"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, token string
		status              int
	}{
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodGet, "/api/v1/namespaces", "", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/snapshot?namespace=app", "Bearer bad", http.StatusUnauthorized},
		{http.MethodGet, "/api/v1/namespaces", "Bearer " + testToken, http.StatusOK},
		{http.MethodGet, "/api/v1/snapshot?namespace=app", "Bearer " + testToken, http.StatusOK},
		{http.MethodGet, "/api/v1/snapshot", "Bearer " + testToken, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/snapshot?namespace=", "Bearer " + testToken, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/snapshot?namespace=app&namespace=other", "Bearer " + testToken, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/snapshot?namespace=app;other", "Bearer " + testToken, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/snapshot?namespace=other", "Bearer " + testToken, http.StatusForbidden},
		{http.MethodGet, "/api/v1/snapshot?namespace=*", "Bearer " + testToken, http.StatusBadRequest},
		{http.MethodGet, "/unknown", "Bearer " + testToken, http.StatusNotFound},
		{http.MethodPost, "/api/v1/snapshot?namespace=app", "Bearer " + testToken, http.StatusMethodNotAllowed},
		{http.MethodPut, "/api/v1/namespaces", "", http.StatusMethodNotAllowed},
		{http.MethodPatch, "/healthz", "", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/v1/snapshot?namespace=app", "Bearer " + testToken, http.StatusMethodNotAllowed},
		{http.MethodHead, "/healthz", "", http.StatusMethodNotAllowed},
	} {
		t.Run(test.method+test.path+fmt.Sprint(test.status), func(t *testing.T) {
			client.ClearActions()
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Authorization", test.token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("sensitive response can be cached")
			}
			if test.status == http.StatusMethodNotAllowed && response.Header().Get("Allow") != "GET" {
				t.Fatal("missing Allow")
			}
			if test.status != http.StatusOK && len(client.Actions()) != 0 {
				t.Fatalf("invalid request reached Kubernetes: %+v", client.Actions())
			}
			assertReadOnly(t, client, "app")
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil)
	request.Header.Add("Authorization", "Bearer "+testToken)
	request.Header.Add("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("duplicate auth headers accepted")
	}
}

func TestTokenAndInsecureLocalValidation(t *testing.T) {
	for _, test := range []struct {
		opts  ServerOptions
		valid bool
	}{
		{ServerOptions{ListenAddress: ":8081"}, false},
		{ServerOptions{Token: "short", ListenAddress: ":8081"}, false},
		{ServerOptions{Token: testToken + "\n", ListenAddress: ":8081"}, false},
		{ServerOptions{Token: testToken, ListenAddress: ":8081"}, true},
		{ServerOptions{InsecureLocal: true, ListenAddress: ":8081"}, false},
		{ServerOptions{InsecureLocal: true, ListenAddress: "0.0.0.0:8081"}, false},
		{ServerOptions{InsecureLocal: true, ListenAddress: "localhost:8081"}, false},
		{ServerOptions{InsecureLocal: true, ListenAddress: "127.0.0.1:8081"}, true},
		{ServerOptions{InsecureLocal: true, ListenAddress: "[::1]:8081"}, true},
		{ServerOptions{InsecureLocal: true, Token: testToken, ListenAddress: "127.0.0.1:8081"}, false},
	} {
		if err := ValidateServerOptions(test.opts); (err == nil) != test.valid {
			t.Errorf("options %+v: %v", test.opts, err)
		}
	}
	path := filepath.Join(t.TempDir(), "token")
	for _, value := range []string{testToken + "\n", "short", testToken + " space", strings.Repeat("x", 4097)} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		token, err := LoadToken(path)
		if value == testToken+"\n" {
			if err != nil || token != testToken {
				t.Fatalf("token loading: %v", err)
			}
		} else if err == nil {
			t.Fatal("invalid token accepted")
		}
	}
	handler, err := NewHandler(collector(t, fakeClient(), "app"), ServerOptions{InsecureLocal: true, ListenAddress: "127.0.0.1:8081"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/namespaces", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
}

func TestHTTPPartialDataContract(t *testing.T) {
	g := gslb(t, "k8gb.io/v1beta1", "web", `"status":{"hosts":"one.example.test"}`)
	handler, err := NewHandler(collector(t, fakeClient(g)), ServerOptions{Token: testToken, ListenAddress: ":8081"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?namespace=app", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var snapshot v1.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.APIVersion != v1.Version || !snapshot.Capabilities.ReadOnly || len(snapshot.Capabilities.Providers) != 1 || snapshot.Capabilities.Providers[0] != "native" || snapshot.Namespace != "app" || len(snapshot.Objects) != 1 || snapshot.Objects[0].Health != "Unknown" {
		t.Fatalf("contract: %+v", snapshot)
	}
	if strings.Contains(response.Body.String(), `"warnings":null`) || strings.Contains(response.Body.String(), `"backends":null`) {
		t.Fatalf("null collection: %s", response.Body.String())
	}
}

func TestHTTPMissingPrimaryAPIsIsUnavailable(t *testing.T) {
	client := fakeClient()
	client.PrependReactor("list", "gslbs", func(a ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(a.GetResource().GroupResource(), "")
	})
	handler, err := NewHandler(collector(t, client), ServerOptions{Token: testToken, ListenAddress: ":8081"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot?namespace=app", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), ErrPrimaryAPIUnavailable.Error()) {
		t.Fatalf("missing APIs reported as healthy: %d %s", response.Code, response.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDynamicTransportGETPaginationTimeoutAndNoClusterAccess(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.URL.Path != "/apis/k8gb.io/v1beta1/namespaces/app/gslbs" || r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("timeoutSeconds") != "5" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		deadline, bounded := r.Context().Deadline()
		if !bounded || time.Until(deadline) > 5*time.Second {
			t.Error("request context is not bounded")
		}
		body := ""
		if r.URL.Query().Get("continue") == "" {
			body = `{"apiVersion":"k8gb.io/v1beta1","kind":"GslbList","metadata":{"continue":"next"},"items":[{"apiVersion":"k8gb.io/v1beta1","kind":"Gslb","metadata":{"name":"first","namespace":"app"}}]}`
		} else {
			if r.URL.Query().Get("continue") != "next" {
				t.Errorf("continuation token: %s", r.URL)
			}
			body = `{"apiVersion":"k8gb.io/v1beta1","kind":"GslbList","metadata":{},"items":[{"apiVersion":"k8gb.io/v1beta1","kind":"Gslb","metadata":{"name":"second","namespace":"app"}}]}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	client, err := dynamic.NewForConfig(&rest.Config{Host: "http://fake-kubernetes.invalid", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(client, Options{ClusterID: "fake", PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.list(context.Background(), canonicalGSLB, "app")
	if err != nil || len(items) != 2 || requests != 2 {
		t.Fatalf("pagination: %d %d %v", len(items), requests, err)
	}
	if _, err := c.list(context.Background(), canonicalGSLB, ""); !errors.Is(err, ErrInvalidNamespace) {
		t.Fatal("unscoped read was allowed")
	}
}
