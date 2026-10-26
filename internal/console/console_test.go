package console

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	v1 "k8gb-ui/api/v1"
)

func TestConfigRejectsUnsafeDefaults(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"no auth", func(c *Config) { c.Auth.Mode = "" }},
		{"public local", func(c *Config) { c.Listen = ":8080" }},
		{"hostname local", func(c *Config) { c.Listen = "localhost:8080" }},
		{"remote HTTP", func(c *Config) { c.Clusters[0].URL = "http://agent.example.test:8081" }},
		{"embedded credentials", func(c *Config) { c.Clusters[0].URL = "https://user:secret@agent.example.test" }},
		{"duplicate IDs", func(c *Config) { c.Clusters = append(c.Clusters, c.Clusters[0]) }},
		{"unknown YAML field", nil},
		{"missing zones", func(c *Config) { c.Clusters[0].Zones = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.change == nil {
				path := filepath.Join(t.TempDir(), "console.yaml")
				_ = os.WriteFile(path, []byte("lissten: ':8080'\n"), 0600)
				if _, err := LoadConfig(path); err == nil {
					t.Fatal("unknown configuration accepted")
				}
				return
			}
			cfg, _ := Demo("127.0.0.1:8080")
			test.change(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	good, _ := Demo("[::1]:8080")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAccessBindingsDenyByDefault(t *testing.T) {
	cfg := Config{Auth: AuthConfig{Mode: "oidc", Bindings: []Binding{{Groups: []string{"team-a"}, Clusters: []string{"east"}, Namespaces: []string{"sandbox"}}, {Subjects: []string{"admin-sub"}, Clusters: []string{"*"}, Namespaces: []string{"*"}}}}}
	for _, test := range []struct {
		p                  Principal
		cluster, namespace string
		allowed            bool
	}{
		{Principal{Subject: "unknown"}, "east", "sandbox", false},
		{Principal{Groups: []string{"team-a"}}, "east", "sandbox", true},
		{Principal{Groups: []string{"team-a"}}, "west", "sandbox", false},
		{Principal{Groups: []string{"team-a"}}, "east", "production", false},
		{Principal{Subject: "admin-sub"}, "west", "production", true},
	} {
		if got := cfg.Allowed(test.p, test.cluster, test.namespace); got != test.allowed {
			t.Fatalf("scope decision got %v for %+v", got, test)
		}
	}
}

func fixtureSites() []v1.Site {
	return []v1.Site{{Cluster: v1.Cluster{ID: "east", Zones: []string{"apps.example.test"}, GeoTags: []string{"east"}}, Available: true, ObjectsComplete: true, CollectedAt: time.Now()}, {Cluster: v1.Cluster{ID: "west", Zones: []string{"apps.example.test"}, GeoTags: []string{"west"}}, Available: true, ObjectsComplete: true, CollectedAt: time.Now()}}
}

func fixtureSnapshot(cluster, name string, targets ...string) v1.Snapshot {
	return v1.Snapshot{APIVersion: v1.Version, ClusterID: cluster, Namespace: "demo", CollectedAt: time.Now(), ObjectsComplete: true, Objects: []v1.Object{{Resource: v1.ResourceReference{Name: name, Namespace: "demo"}, Hostname: "api.apps.example.test", GeoTag: cluster, Health: "healthy", Policy: v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 10}, DNSObserved: true, DNSTargets: targets}}}
}

func hasSignal(group v1.Group, code string) bool {
	for _, s := range group.Signals {
		if s.Code == code {
			return true
		}
	}
	return false
}

func TestAggregateUsesHostnameNotResourceName(t *testing.T) {
	result := Aggregate("demo", fixtureSites(), []v1.Snapshot{fixtureSnapshot("east", "gslb-api", "192.0.2.1"), fixtureSnapshot("west", "different-name", "198.51.100.1")})
	if len(result.Groups) != 1 || len(result.Groups[0].Instances) != 2 || result.Groups[0].State != "healthy" {
		t.Fatalf("unexpected grouping: %+v", result.Groups)
	}
	// Different local addresses are expected, not drift.
	if hasSignal(result.Groups[0], "dns_target_count") {
		t.Fatal("IP differences treated as DNS drift")
	}
}

func TestAggregateDNSCountIsInformation(t *testing.T) {
	result := Aggregate("demo", fixtureSites(), []v1.Snapshot{fixtureSnapshot("east", "api", "192.0.2.1"), fixtureSnapshot("west", "api", "192.0.2.1", "198.51.100.1")})
	if result.Groups[0].State != "info" || !hasSignal(result.Groups[0], "dns_target_count") {
		t.Fatalf("expected informational DNS count: %+v", result.Groups[0])
	}
}

func TestAggregateUnavailableNotMissing(t *testing.T) {
	sites := fixtureSites()
	sites[1].Available = false
	result := Aggregate("demo", sites, []v1.Snapshot{fixtureSnapshot("east", "api", "192.0.2.1")})
	if hasSignal(result.Groups[0], "missing_object") || result.Groups[0].State != "unknown" {
		t.Fatalf("unavailable source presented as missing resource: %+v", result.Groups[0])
	}
}

func TestAggregatePartialInventoryNotMissing(t *testing.T) {
	sites := fixtureSites()
	sites[1].ObjectsComplete = false
	result := Aggregate("demo", sites, []v1.Snapshot{fixtureSnapshot("east", "api", "192.0.2.1")})
	if hasSignal(result.Groups[0], "missing_object") || !hasSignal(result.Groups[0], "source_partial") || result.Groups[0].State != "unknown" {
		t.Fatalf("partial inventory presented as missing resource: %+v", result.Groups[0])
	}
}

func TestAggregateInvalidPrimaryAndPartialZones(t *testing.T) {
	sites := fixtureSites()
	sites[1].Cluster.Zones = []string{"other.example.test"}
	snapshot := fixtureSnapshot("east", "api", "192.0.2.1")
	snapshot.Objects[0].Policy = v1.Policy{Strategy: "failover", PrimaryGeoTag: "not-in-zone"}
	result := Aggregate("demo", sites, []v1.Snapshot{snapshot})
	if !hasSignal(result.Groups[0], "invalid_primary") || hasSignal(result.Groups[0], "missing_object") {
		t.Fatalf("invalid primary or participation not handled: %+v", result.Groups[0])
	}
}

type fakeAgent struct {
	namespaces []string
	snapshot   v1.Snapshot
	err        error
	calls      int
}

func (f *fakeAgent) Namespaces(context.Context) ([]string, error) { return f.namespaces, f.err }
func (f *fakeAgent) Snapshot(context.Context, string) (v1.Snapshot, error) {
	f.calls++
	return f.snapshot, f.err
}

func testServer(t *testing.T) *Server {
	t.Helper()
	cfg, agents := Demo("127.0.0.1:8080")
	assets := fstest.MapFS{"index.html": {Data: []byte("standalone console"), Mode: fs.FileMode(0644)}}
	server, err := New(context.Background(), cfg, assets, agents, true)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestServerReadOnlyAndExplicitNamespace(t *testing.T) {
	s := testServer(t)
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200}, {"GET", "/healthz", 200}, {"GET", "/api/v1/clusters", 200}, {"GET", "/api/v1/overview", 400},
		{"GET", "/api/v1/overview?namespace=demo&namespace=other", 400},
		{"GET", "/api/v1/overview?namespace=demo", 200}, {"POST", "/api/v1/overview?namespace=demo", 405},
		{"POST", "/api/v1/gslb-settings/apply", 405}, {"DELETE", "/", 405},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(test.method, test.path, nil))
		if w.Code != test.status {
			t.Fatalf("%s %s got %d: %s", test.method, test.path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("private data may be cached")
		}
	}
}

func TestNamespaceScopesAvoidAgentCalls(t *testing.T) {
	s := testServer(t)
	// Local authentication still obeys the deployment's cluster namespace allowlist.
	s.cfg.Clusters[0].Namespaces = []string{"demo"}
	s.cfg.Clusters[1].Namespaces = []string{"demo"}
	east := &fakeAgent{namespaces: []string{"demo", "secret"}, snapshot: fixtureSnapshot("east", "api", "192.0.2.1")}
	s.agents["east"] = east
	west := &fakeAgent{err: errors.New("unreachable")}
	s.agents["west"] = west
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/overview?namespace=secret", nil))
	if w.Code != 403 || east.calls != 0 || west.calls != 0 {
		t.Fatal("unauthorized namespace was fetched")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/namespaces", nil))
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("namespace leaked")
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/overview?namespace=demo", nil))
	var result v1.Overview
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Groups) != 1 || result.Groups[0].State != "unknown" {
		t.Fatalf("partial failure not preserved: %s", w.Body.String())
	}
}

func TestRemoteRejectsRedirectWithoutLeakingToken(t *testing.T) {
	var secondCalls int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { secondCalls++; w.WriteHeader(200) }))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, second.URL, 302) }))
	defer first.Close()
	a, err := newRemoteAgent(ClusterConfig{ID: "east", URL: first.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Namespaces(context.Background())
	if err == nil || secondCalls != 0 {
		t.Fatal("agent redirect followed")
	}
}

func TestRemoteValidatesIdentityAndNamespace(t *testing.T) {
	snapshot := fixtureSnapshot("west", "api", "192.0.2.1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, snapshot) }))
	defer srv.Close()
	a, err := newRemoteAgent(ClusterConfig{ID: "east", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.Snapshot(context.Background(), "demo"); err == nil {
		t.Fatal("wrong agent identity accepted")
	}
	snapshot.ClusterID = "east"
	snapshot.Objects[0].Resource.Namespace = "secret"
	if _, err = a.Snapshot(context.Background(), "demo"); err == nil {
		t.Fatal("cross-namespace response accepted")
	}
}
