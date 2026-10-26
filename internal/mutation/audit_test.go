package mutation

import (
	"io"
	"log/slog"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
)

func TestAuditIDsDistinguishSameTimeEvents(t *testing.T) {
	when := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	events := []v1.AuditEvent{}
	m := Manager{opts: Options{ClusterID: "east", Now: func() time.Time { return when }, Audit: func(e v1.AuditEvent) { events = append(events, e) }}, logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	for i := 0; i < 3; i++ {
		m.audit(v1.MutationRequest{Actor: "admin", Reason: "maintenance"}, "applied", "digest", nil, false)
	}
	seen := map[string]bool{}
	for _, e := range events {
		if e.ID == "" || seen[e.ID] || !e.Time.Equal(when) || e.ClusterID != "east" {
			t.Fatalf("event identity was lost or reused: %+v", e)
		}
		seen[e.ID] = true
	}
}
