package auditlog

import (
	v1 "k8gb-ui/api/v1"
	"reflect"
	"sync"
	"testing"
)

func TestByteBoundedHistoryAndNamespaceFilter(t *testing.T) {
	s := New()
	s.Limit = 1000
	for i := 0; i < 100; i++ {
		s.Append(v1.AuditEvent{Actor: "admin", Resource: v1.ResourceReference{Namespace: "demo"}})
	}
	if s.size > s.Limit || len(s.Events("demo")) == 0 || len(s.Events("demo")) >= 100 || len(s.Events("secret")) != 0 {
		t.Fatal("incorrect retention or scope")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Append(v1.AuditEvent{Resource: v1.ResourceReference{Namespace: "demo"}})
			_ = s.Events("demo")
		}()
	}
	wg.Wait()
}

func TestRetentionMetadataAndRestartIdentity(t *testing.T) {
	s := New()
	s.Limit = 700
	for i := 0; i < 20; i++ {
		s.Append(v1.AuditEvent{ID: "event", Actor: "admin", Resource: v1.ResourceReference{Namespace: "demo"}})
	}
	log := s.Read("demo")
	if log.Retention.Storage != "memory" || log.Retention.Scope != "replica" || log.Retention.InstanceID == "" || log.Retention.StartedAt.IsZero() || log.Retention.EvictedEvents == 0 || log.Retention.RetainedBytes > 700 || !reflect.DeepEqual(log.Events, s.Events("demo")) {
		t.Fatalf("invalid retained window: %+v", log)
	}
	if s.Read("secret").Retention.EvictedEvents != 0 {
		t.Fatal("other namespace eviction count leaked")
	}
	if New().Read("demo").Retention.InstanceID == log.Retention.InstanceID {
		t.Fatal("restart identity was reused")
	}
}
