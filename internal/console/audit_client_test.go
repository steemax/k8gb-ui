package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteAuditValidatesIdentityAndKeepsLegacyCompatibility(t *testing.T) {
	for _, mode := range []string{"current", "legacy", "namespace", "cluster", "event-namespace", "event-cluster", "version"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/audit" || r.URL.Query().Get("namespace") != "demo" {
					t.Error("wrong audit transport")
				}
				log := auditFixtureLog("east")
				switch mode {
				case "legacy":
					log.Namespace = ""
					log.Retention = nil
				case "namespace":
					log.Namespace = "secret"
				case "cluster":
					log.ClusterID = "west"
				case "event-namespace":
					log.Events[0].Resource.Namespace = "secret"
				case "event-cluster":
					log.Events[0].ClusterID = "west"
				case "version":
					log.APIVersion = "unsupported"
				}
				_ = json.NewEncoder(w).Encode(log)
			}))
			defer server.Close()
			a := &remoteAgent{cluster: ClusterConfig{ID: "east", URL: server.URL}, client: server.Client()}
			log, err := a.ReadAudit(context.Background(), "demo")
			want := mode == "current" || mode == "legacy"
			if (err == nil) != want {
				t.Fatalf("invalid audit acceptance: mode=%s err=%v", mode, err)
			}
			if mode == "current" && (log.Retention == nil || log.Events[0].ID == "") {
				t.Fatal("source metadata/event ID dropped")
			}
		})
	}
}
