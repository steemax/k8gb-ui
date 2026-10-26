package agent

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
)

type failingAudit struct{ reads int }

func (s *failingAudit) Append(v1.AuditEvent) {}
func (s *failingAudit) ReadAudit(context.Context, string) (v1.AgentAudit, error) {
	s.reads++
	return v1.AgentAudit{}, errors.New("private Kubernetes failure")
}

func TestAuditFailureAndScopeAreExplicit(t *testing.T) {
	store := &failingAudit{}
	handler, err := NewHandler(collector(t, fakeClient(), "app"), ServerOptions{Token: testToken, ListenAddress: ":8081", Audit: store})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, token string
		status      int
	}{{"/api/v1/audit?namespace=app", "", 401}, {"/api/v1/audit?namespace=other", "Bearer " + testToken, 403}, {"/api/v1/audit?namespace=app", "Bearer " + testToken, 503}} {
		r := httptest.NewRequest("GET", test.path, nil)
		r.Header.Set("Authorization", test.token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != test.status || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("audit response %d: %s", w.Code, w.Body.String())
		}
	}
	if store.reads != 1 {
		t.Fatal("unauthorized request reached audit storage")
	}
}
