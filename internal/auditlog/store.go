// Package auditlog retains only a bounded convenience history. Structured stdout
// remains the external log sink; neither this buffer nor a plan is desired state.
package auditlog

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"sync"
	"time"

	v1 "k8gb-ui/api/v1"
)

type Journal interface {
	Append(v1.AuditEvent)
	ReadAudit(context.Context, string) (v1.AgentAudit, error)
}

type Store struct {
	mu         sync.RWMutex
	records    [][]byte
	size       int
	Limit      int
	instanceID string
	startedAt  time.Time
	evicted    map[string]uint64
}

func New() *Store {
	return &Store{Limit: 700 * 1024, instanceID: rand.Text(), startedAt: time.Now().UTC(), evicted: map[string]uint64{}}
}
func (s *Store) Append(event v1.AuditEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.Limit
	if limit <= 0 {
		limit = 700 * 1024
	}
	if len(data) > limit {
		if s.evicted == nil {
			s.evicted = map[string]uint64{}
		}
		s.evicted[event.Resource.Namespace]++
		return
	}
	for len(s.records) > 0 && s.size+len(data) > limit {
		var old v1.AuditEvent
		if json.Unmarshal(s.records[0], &old) == nil {
			if s.evicted == nil {
				s.evicted = map[string]uint64{}
			}
			s.evicted[old.Resource.Namespace]++
		}
		s.size -= len(s.records[0])
		s.records = s.records[1:]
	}
	s.records = append(s.records, data)
	s.size += len(data)
}
func (s *Store) Events(namespace string) []v1.AuditEvent {
	return s.Read(namespace).Events
}

func (s *Store) ReadAudit(_ context.Context, namespace string) (v1.AgentAudit, error) {
	return s.Read(namespace), nil
}

func (s *Store) Read(namespace string) v1.AgentAudit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := []v1.AuditEvent{}
	bytes := 0
	for i := len(s.records) - 1; i >= 0; i-- {
		var event v1.AuditEvent
		if json.Unmarshal(s.records[i], &event) == nil && event.Resource.Namespace == namespace {
			result = append(result, event)
			bytes += len(s.records[i])
		}
	}
	limit := s.Limit
	if limit <= 0 {
		limit = 700 * 1024
	}
	return v1.AgentAudit{Namespace: namespace, Events: result, Retention: &v1.AuditRetention{Storage: "memory", Scope: "replica", InstanceID: s.instanceID, StartedAt: s.startedAt, LimitBytes: limit, RetainedBytes: bytes, EvictedEvents: s.evicted[namespace]}}
}
