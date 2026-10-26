package console

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	v1 "k8gb-ui/api/v1"
)

type auditReader interface {
	ReadAudit(context.Context, string) (v1.AgentAudit, error)
}

type legacyAuditReader interface {
	Audit(context.Context, string) ([]v1.AuditEvent, error)
}

// Each Console reads the same cluster-local sources; no Console retains or
// forwards another Console's history. Failure never masquerades as an empty log.
func (s *Server) collectAudit(ctx context.Context, namespace string, clusters []ClusterConfig) v1.AuditOverview {
	result := v1.AuditOverview{APIVersion: v1.Version, Namespace: namespace, CollectedAt: time.Now().UTC(), Events: []v1.AuditEvent{}, Sources: make([]v1.AuditSource, len(clusters)), Warnings: []string{}}
	records := make([][]v1.AuditEvent, len(clusters))
	eachCluster(ctx, clusters, func(ctx context.Context, cluster ClusterConfig, index int) {
		source := v1.AuditSource{ClusterID: cluster.ID}
		var log v1.AgentAudit
		var err error
		if reader, ok := s.agents[cluster.ID].(auditReader); ok {
			log, err = reader.ReadAudit(ctx, namespace)
			if err == nil && (log.ClusterID != cluster.ID || log.APIVersion != v1.Version || log.Namespace != "" && log.Namespace != namespace) {
				err = fmt.Errorf("invalid audit source identity")
			}
		} else if reader, ok := s.agents[cluster.ID].(legacyAuditReader); ok {
			log.Events, err = reader.Audit(ctx, namespace)
		} else {
			err = fmt.Errorf("audit is not supported")
		}
		seen := map[string]string{}
		for _, event := range log.Events {
			if event.Resource.Namespace != namespace || event.ClusterID != cluster.ID || event.Time.IsZero() {
				err = fmt.Errorf("invalid audit event scope or time")
				break
			}
			if event.ID != "" {
				raw, _ := json.Marshal(event)
				if old, exists := seen[event.ID]; exists && old != string(raw) {
					err = fmt.Errorf("conflicting audit event identities")
					break
				}
				seen[event.ID] = string(raw)
			}
		}
		if err != nil {
			source.Error = "Recent audit unavailable or invalid"
		} else {
			source.Available, source.Events, source.Retention = true, len(log.Events), log.Retention
			source.Partial, source.Warnings = log.Partial, append([]string(nil), log.Warnings...)
			records[index] = log.Events
		}
		result.Sources[index] = source
	})
	sort.Slice(result.Sources, func(i, j int) bool { return result.Sources[i].ClusterID < result.Sources[j].ClusterID })
	for _, source := range result.Sources {
		if !source.Available {
			result.Partial = true
			result.Warnings = append(result.Warnings, source.ClusterID+": recent audit unavailable")
		}
		if source.Partial {
			result.Partial = true
		}
		for _, warning := range source.Warnings {
			result.Warnings = append(result.Warnings, source.ClusterID+": "+warning)
		}
	}
	seen := map[string]bool{}
	for _, batch := range records {
		for _, event := range batch {
			if event.ID != "" {
				key := event.ClusterID + "/" + event.ID
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			result.Events = append(result.Events, event)
		}
	}
	// Equal timestamps are expected across sites and clocks need not agree. The
	// tie-break makes retained results independent of response/configuration order.
	sort.Slice(result.Events, func(i, j int) bool {
		a, b := result.Events[i], result.Events[j]
		if !a.Time.Equal(b.Time) {
			return a.Time.After(b.Time)
		}
		if a.ClusterID != b.ClusterID {
			return a.ClusterID < b.ClusterID
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) < string(y)
	})
	return result
}
