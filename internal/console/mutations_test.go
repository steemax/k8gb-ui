package console

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
)

type fakeWriter struct {
	fakeAgent
	plans, applies int
	actor          string
	fail           bool
}

func (f *fakeWriter) Plan(_ context.Context, r v1.MutationRequest) (v1.MutationPlan, error) {
	f.plans++
	f.actor = r.Actor
	return v1.MutationPlan{APIVersion: v1.Version, ClusterID: "east", Request: r, ExpiresAt: time.Now().Add(time.Minute), Digest: strings.Repeat("a", 64), Token: "signed"}, nil
}
func (f *fakeWriter) Apply(_ context.Context, p v1.MutationPlan) (v1.MutationResult, error) {
	f.applies++
	return v1.MutationResult{APIVersion: v1.Version, ClusterID: "east", Resource: p.Request.Resource, Digest: p.Digest, Status: "applied"}, nil
}
func (f *fakeWriter) Catalog(context.Context, string) (v1.Catalog, error) { return v1.Catalog{}, nil }
func (f *fakeWriter) Audit(context.Context, string) ([]v1.AuditEvent, error) {
	return []v1.AuditEvent{}, nil
}
func writerServer(t *testing.T) (*Server, *fakeWriter) {
	t.Helper()
	s := testServer(t)
	s.demo = false
	s.cfg.WritesEnabled = true
	s.cfg.Auth.LocalAdmin = true
	s.cfg.Clusters[0].WriteTokenFile = "write-token"
	s.cfg.Clusters[0].Namespaces = []string{"demo"}
	f := &fakeWriter{}
	s.agents["east"] = f
	return s, f
}
func mutationHTTP(s *Server, path string, body any, origin string) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(data)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-K8gb-Intent", "mutation")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func testPlanRequest(namespace string) v1.BatchPlanRequest {
	return v1.BatchPlanRequest{Targets: []v1.MutationTarget{{ClusterID: "east", Request: v1.MutationRequest{Provider: "native", Operation: "update", Resource: v1.ResourceReference{APIVersion: "k8gb.io/v1beta1", Kind: "Gslb", Name: "api", Namespace: namespace, UID: "uid", ResourceVersion: "1"}, Actor: "forged-user", Reason: "Test policy change", Policy: &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 10}}}}}
}

func TestMutationRequiresExplicitAdminAndSameOrigin(t *testing.T) {
	s, f := writerServer(t)
	input := testPlanRequest("demo")
	for _, origin := range []string{"", "https://evil.example.test", "http://localhost:8080"} {
		w := mutationHTTP(s, "/api/v1/mutations/plan", input, origin)
		if w.Code != 403 || f.plans != 0 {
			t.Fatalf("untrusted origin accepted: %s %d", origin, w.Code)
		}
	}
	s.cfg.Auth.LocalAdmin = false
	w := mutationHTTP(s, "/api/v1/mutations/plan", input, "http://127.0.0.1:8080")
	if w.Code != 403 || f.plans != 0 {
		t.Fatal("viewer planned writes")
	}
	s.cfg.Auth.LocalAdmin = true
	s.cfg.WritesEnabled = false
	w = mutationHTTP(s, "/api/v1/mutations/plan", input, "http://127.0.0.1:8080")
	if w.Code != 403 {
		t.Fatal("disabled write configuration ignored")
	}
}
func TestMutationReplacesClaimedActorAndRejectsScope(t *testing.T) {
	s, f := writerServer(t)
	w := mutationHTTP(s, "/api/v1/mutations/plan", testPlanRequest("secret"), "http://127.0.0.1:8080")
	if w.Code != 403 || f.plans != 0 {
		t.Fatal("cross-namespace write reached agent")
	}
	input := testPlanRequest("demo")
	input.Targets[0].Request.Reason = " "
	w = mutationHTTP(s, "/api/v1/mutations/plan", input, "http://127.0.0.1:8080")
	if w.Code != 400 {
		t.Fatal("empty reason accepted")
	}
	w = mutationHTTP(s, "/api/v1/mutations/plan", testPlanRequest("demo"), "http://127.0.0.1:8080")
	if w.Code != 200 || f.plans != 1 || f.actor != "local" {
		t.Fatalf("actor not authenticated: %d %s", w.Code, w.Body.String())
	}
	var planned v1.BatchPlan
	_ = json.Unmarshal(w.Body.Bytes(), &planned)
	inputApply := v1.BatchApplyRequest{Targets: planned.Targets}
	inputApply.Targets[0].Plan.Request.Actor = "other-user"
	w = mutationHTTP(s, "/api/v1/mutations/apply", inputApply, "http://127.0.0.1:8080")
	if w.Code != 409 || f.applies != 0 {
		t.Fatal("another actor's plan accepted")
	}
}
func TestPartialApplyAndExpiryRequireReplanning(t *testing.T) {
	s, f := writerServer(t)
	w := mutationHTTP(s, "/api/v1/mutations/plan", testPlanRequest("demo"), "http://127.0.0.1:8080")
	var plan v1.BatchPlan
	_ = json.Unmarshal(w.Body.Bytes(), &plan)
	input := v1.BatchApplyRequest{Targets: append(plan.Targets, v1.TargetPlan{ClusterID: "west", Error: &v1.MutationError{Code: "unreachable", Error: "Agent unavailable"}})}
	w = mutationHTTP(s, "/api/v1/mutations/apply", input, "http://127.0.0.1:8080")
	if w.Code != 409 || f.applies != 0 {
		t.Fatal("partial apply without consent")
	}
	input.AllowPartial = true
	input.Targets[0].Plan.ExpiresAt = time.Now().Add(-time.Minute)
	w = mutationHTTP(s, "/api/v1/mutations/apply", input, "http://127.0.0.1:8080")
	if w.Code != 409 || f.applies != 0 {
		t.Fatal("expired plan was applied")
	}
}
func TestWriteBindingDoesNotBroadenReadScope(t *testing.T) {
	cfg := Config{WritesEnabled: true, Clusters: []ClusterConfig{{ID: "east", WriteTokenFile: "write", Namespaces: []string{"team"}}}, Auth: AuthConfig{Mode: "oidc", Bindings: []Binding{{Role: "viewer", Groups: []string{"devs"}, Clusters: []string{"*"}, Namespaces: []string{"*"}}, {Role: "admin", Subjects: []string{"admin"}, Clusters: []string{"east"}, Namespaces: []string{"team"}}}}}
	if cfg.CanWrite(Principal{Groups: []string{"devs"}}, "east", "team") || !cfg.CanWrite(Principal{Subject: "admin"}, "east", "team") || cfg.CanWrite(Principal{Subject: "admin"}, "east", "other") {
		t.Fatal("incorrect write scopes")
	}
}
