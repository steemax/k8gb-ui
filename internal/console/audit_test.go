package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
)

type auditFixture struct {
	fakeAgent
	read func(context.Context, string) (v1.AgentAudit, error)
}

func (a *auditFixture) ReadAudit(ctx context.Context, ns string) (v1.AgentAudit, error) {
	return a.read(ctx, ns)
}

func auditFixtureLog(cluster string) v1.AgentAudit {
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := v1.AuditEvent{ID: "stable-event", Time: when, ClusterID: cluster, Actor: "admin", Provider: "native", Operation: "update", Resource: v1.ResourceReference{Namespace: "demo", Name: "web"}, Status: "applied"}
	return v1.AgentAudit{APIVersion: v1.Version, ClusterID: cluster, Namespace: "demo", Events: []v1.AuditEvent{e, e}, Retention: &v1.AuditRetention{Storage: "memory", Scope: "replica", InstanceID: cluster + "-replica"}}
}

func TestAuditIdenticalAcrossConsolesAndClusterOrder(t *testing.T) {
	first, second := testServer(t), testServer(t)
	for _, s := range []*Server{first, second} {
		for _, id := range []string{"east", "west"} {
			id := id
			s.agents[id] = &auditFixture{read: func(context.Context, string) (v1.AgentAudit, error) { return auditFixtureLog(id), nil }}
		}
	}
	clusters := []ClusterConfig{{ID: "east"}, {ID: "west"}}
	a := first.collectAudit(context.Background(), "demo", clusters)
	b := second.collectAudit(context.Background(), "demo", []ClusterConfig{clusters[1], clusters[0]})
	if a.Partial || b.Partial || len(a.Events) != 2 || !reflect.DeepEqual(a.Events, b.Events) || !reflect.DeepEqual(a.Sources, b.Sources) {
		t.Fatalf("Console identity/order changed retained audit: %+v / %+v", a, b)
	}
	if a.Events[0].ClusterID != "east" || a.Events[1].ClusterID != "west" {
		t.Fatal("equal timestamps lack stable tie-breaking")
	}
}

func TestAuditParallelSourcesAndPartialFailure(t *testing.T) {
	s := testServer(t)
	ready := make(chan struct{})
	var started atomic.Int32
	for _, id := range []string{"east", "west"} {
		id := id
		s.agents[id] = &auditFixture{read: func(ctx context.Context, _ string) (v1.AgentAudit, error) {
			if started.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-ctx.Done():
				return v1.AgentAudit{}, ctx.Err()
			}
			if id == "west" {
				<-ctx.Done()
				return v1.AgentAudit{}, ctx.Err()
			}
			return auditFixtureLog(id), nil
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result := s.collectAudit(ctx, "demo", []ClusterConfig{{ID: "west"}, {ID: "east"}})
	if !result.Partial || len(result.Events) != 1 || len(result.Sources) != 2 || result.Sources[0].ClusterID != "east" || !result.Sources[0].Available || result.Sources[1].Available || len(result.Warnings) != 1 {
		t.Fatalf("failed source lost or hid healthy log: %+v", result)
	}
}

func TestAuditRejectsWrongScopeAndConflictingIDs(t *testing.T) {
	for _, mode := range []string{"namespace", "cluster", "duplicate", "zero-time", "unavailable", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			s := testServer(t)
			s.agents["east"] = &auditFixture{read: func(context.Context, string) (v1.AgentAudit, error) {
				log := auditFixtureLog("east")
				switch mode {
				case "namespace":
					log.Events[0].Resource.Namespace = "secret"
				case "cluster":
					log.ClusterID = "other"
				case "duplicate":
					log.Events[1].Status = "plan_failed"
				case "zero-time":
					log.Events[0].Time = time.Time{}
				case "unavailable":
					return log, errors.New("secret.internal.example")
				}
				return log, nil
			}}
			if mode == "unsupported" {
				s.agents["east"] = &fakeAgent{}
			}
			result := s.collectAudit(context.Background(), "demo", []ClusterConfig{{ID: "east"}})
			if !result.Partial || len(result.Events) != 0 || len(result.Sources) != 1 || result.Sources[0].Available {
				t.Fatalf("invalid source accepted: %+v", result)
			}
		})
	}
}

func TestAuditQueriesOnlyAuthorizedClusterSources(t *testing.T) {
	s := testServer(t)
	s.cfg.Auth.Mode = "oidc"
	s.cfg.Auth.Bindings = []Binding{{Subjects: []string{"viewer"}, Clusters: []string{"east"}, Namespaces: []string{"demo"}}}
	var deniedCalls atomic.Int32
	s.agents["east"] = &auditFixture{read: func(context.Context, string) (v1.AgentAudit, error) { return auditFixtureLog("east"), nil }}
	s.agents["west"] = &auditFixture{read: func(context.Context, string) (v1.AgentAudit, error) {
		deniedCalls.Add(1)
		return auditFixtureLog("west"), nil
	}}
	w := httptest.NewRecorder()
	s.audit(w, httptest.NewRequest("GET", "/api/v1/audit?namespace=demo", nil), Principal{Subject: "viewer"})
	var result v1.AuditOverview
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Sources) != 1 || result.Sources[0].ClusterID != "east" || deniedCalls.Load() != 0 {
		t.Fatalf("audit escaped RBAC: %d %+v", w.Code, result)
	}
	w = httptest.NewRecorder()
	s.audit(w, httptest.NewRequest("GET", "/api/v1/audit?namespace=secret", nil), Principal{Subject: "viewer"})
	if w.Code != 403 {
		t.Fatal("forbidden namespace audit accepted")
	}
}

func TestAuditPropagatesSharedStorageDegradation(t *testing.T) {
	s := testServer(t)
	s.agents["east"] = &auditFixture{read: func(context.Context, string) (v1.AgentAudit, error) {
		log := auditFixtureLog("east")
		log.Partial = true
		log.Warnings = []string{"Pending audit records"}
		log.Retention = &v1.AuditRetention{Storage: "configMap", Scope: "namespace", PendingEvents: 1}
		return log, nil
	}}
	result := s.collectAudit(context.Background(), "demo", []ClusterConfig{{ID: "east"}})
	if !result.Partial || !result.Sources[0].Available || !result.Sources[0].Partial || len(result.Events) != 1 || len(result.Warnings) != 1 {
		t.Fatalf("degraded shared history marked complete: %+v", result)
	}
}
