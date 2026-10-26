package auditlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type cmHarness struct {
	mu             sync.Mutex
	cm             *unstructured.Unstructured
	revision       int
	conflicts      int
	failGet        bool
	failUpdate     bool
	unknownUpdate  bool
	reads, updates int
}

func newCMHarness() *cmHarness {
	return &cmHarness{revision: 1, cm: &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"namespace": "demo", "name": "k8gb-ui-audit", "uid": "journal-uid", "resourceVersion": "1", "creationTimestamp": "2026-01-01T00:00:00Z", "labels": map[string]any{ConfigMapLabel: "true"}}, "data": map[string]any{}}}}
}

func (h *cmHarness) client() *fake.FakeDynamicClient {
	c := fake.NewSimpleDynamicClient(runtime.NewScheme())
	c.PrependReactor("*", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if action.GetNamespace() != "demo" {
			return true, nil, apierrors.NewForbidden(configMaps.GroupResource(), "k8gb-ui-audit", fmt.Errorf("outside test scope"))
		}
		switch a := action.(type) {
		case ktesting.GetAction:
			h.reads++
			if h.failGet {
				return true, nil, apierrors.NewServiceUnavailable("test API unavailable")
			}
			if h.cm == nil {
				return true, nil, apierrors.NewNotFound(configMaps.GroupResource(), a.GetName())
			}
			return true, h.cm.DeepCopy(), nil
		case ktesting.UpdateAction:
			h.updates++
			if h.failUpdate {
				return true, nil, apierrors.NewForbidden(configMaps.GroupResource(), "k8gb-ui-audit", fmt.Errorf("test denied"))
			}
			desired := a.GetObject().(*unstructured.Unstructured)
			if h.conflicts > 0 {
				h.conflicts--
				return true, nil, apierrors.NewConflict(configMaps.GroupResource(), "k8gb-ui-audit", fmt.Errorf("test concurrent writer"))
			}
			if desired.GetResourceVersion() != h.cm.GetResourceVersion() {
				return true, nil, apierrors.NewConflict(configMaps.GroupResource(), "k8gb-ui-audit", fmt.Errorf("stale resourceVersion"))
			}
			h.revision++
			h.cm = desired.DeepCopy()
			h.cm.SetResourceVersion(strconv.Itoa(h.revision))
			if h.unknownUpdate {
				h.unknownUpdate = false
				return true, nil, apierrors.NewTimeoutError("test lost committed reply", 1)
			}
			return true, h.cm.DeepCopy(), nil
		}
		return true, nil, fmt.Errorf("unexpected ConfigMap verb: %s", action.GetVerb())
	})
	return c
}

func sharedStore(t *testing.T, h *cmHarness) *ConfigMapStore {
	t.Helper()
	s, err := NewConfigMap(h.client(), ConfigMapOptions{ClusterID: "east", Name: "k8gb-ui-audit", AllowedNamespaces: []string{"demo"}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s.logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	return s
}

func auditEvent(id string) v1.AuditEvent {
	return v1.AuditEvent{ID: id, Time: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), ClusterID: "east", Actor: "admin", Provider: "native", Operation: "update", Status: "applied", Resource: v1.ResourceReference{Namespace: "demo", Name: "web"}, Reason: "test maintenance"}
}

func TestConfigMapSharedAcrossReplicasAndRestarts(t *testing.T) {
	h := newCMHarness()
	a, b := sharedStore(t, h), sharedStore(t, h)
	a.Append(auditEvent("first"))
	b.Append(auditEvent("second"))
	x, err := a.ReadAudit(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	y, err := sharedStore(t, h).ReadAudit(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(x, y) || len(x.Events) != 2 || x.Retention.Storage != "configMap" || x.Retention.Scope != "namespace" || x.Retention.InstanceID != "journal-uid" || x.Retention.ConfigMap != "demo/k8gb-ui-audit" || x.Partial {
		t.Fatalf("replica/restart changed journal: %+v / %+v", x, y)
	}
	if x.Retention.LimitBytes != ConfigMapLimit || x.Retention.RetainedBytes <= 0 {
		t.Fatal("missing byte retention metadata")
	}
}

func TestConfigMapConcurrentWritersAndConflictRetry(t *testing.T) {
	h := newCMHarness()
	h.conflicts = 2
	stores := []*ConfigMapStore{sharedStore(t, h), sharedStore(t, h), sharedStore(t, h)}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); stores[i%len(stores)].Append(auditEvent(fmt.Sprintf("event-%02d", i))) }(i)
	}
	wg.Wait()
	for _, s := range stores {
		if err := s.Flush(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	log, err := stores[0].ReadAudit(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Events) != 30 {
		t.Fatalf("concurrent writes lost events: %d", len(log.Events))
	}
	for _, e := range log.Events {
		if e.ID == "" {
			t.Fatal("event ID lost")
		}
	}
}

func TestConfigMapAmbiguousCommitIsNotDuplicated(t *testing.T) {
	h := newCMHarness()
	h.unknownUpdate = true
	s := sharedStore(t, h)
	event := auditEvent("committed-once")
	s.Append(event)
	if pending, _ := s.pendingFor("demo"); len(pending) != 1 {
		t.Fatal("ambiguous write must remain pending until a read confirms it")
	}
	log, err := s.ReadAudit(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Events) != 1 || log.Retention.PendingEvents != 0 {
		t.Fatal("recovery duplicated committed audit")
	}
	s.Append(event)
	log, err = s.ReadAudit(context.Background(), "demo")
	if err != nil || len(log.Events) != 1 {
		t.Fatal("same event was appended twice")
	}
}

func TestConfigMapFailureDoesNotReturnEmptySuccess(t *testing.T) {
	h := newCMHarness()
	s := sharedStore(t, h)
	h.failUpdate = true
	s.Append(auditEvent("pending"))
	if _, err := s.ReadAudit(context.Background(), "demo"); err == nil {
		t.Fatal("failed persistence returned a complete/empty log")
	}
	h.failUpdate = false
	log, err := s.ReadAudit(context.Background(), "demo")
	if err != nil || len(log.Events) != 1 {
		t.Fatalf("pending record was not recovered: %+v %v", log, err)
	}
	h.failGet = true
	if _, err := s.ReadAudit(context.Background(), "demo"); err == nil {
		t.Fatal("API outage returned stale per-replica history")
	}
}

func TestConfigMapScopeAndForeignDataAreNeverOverwritten(t *testing.T) {
	for _, mode := range []string{"unlabeled", "other-data", "invalid-json", "immutable", "other-cluster", "other-namespace", "duplicate-id", "missing", "wrong-uid"} {
		t.Run(mode, func(t *testing.T) {
			h := newCMHarness()
			switch mode {
			case "unlabeled":
				h.cm.SetLabels(map[string]string{})
			case "other-data":
				_ = unstructured.SetNestedStringMap(h.cm.Object, map[string]string{"unrelated": "keep"}, "data")
			case "invalid-json":
				_ = unstructured.SetNestedStringMap(h.cm.Object, map[string]string{ConfigMapDataKey: "{"}, "data")
			case "immutable":
				h.cm.Object["immutable"] = true
			case "missing":
				h.cm = nil
			case "wrong-uid":
				h.cm.SetUID(types.UID(""))
			default:
				j := configMapJournal{Version: 1, ClusterID: "east", Namespace: "demo", CreatedAt: time.Now().UTC(), Events: []v1.AuditEvent{auditEvent("one")}}
				if mode == "other-cluster" {
					j.ClusterID = "west"
				}
				if mode == "other-namespace" {
					j.Events[0].Resource.Namespace = "secret"
				}
				if mode == "duplicate-id" {
					j.Events = append(j.Events, j.Events[0])
				}
				raw, _ := json.Marshal(j)
				_ = unstructured.SetNestedStringMap(h.cm.Object, map[string]string{ConfigMapDataKey: string(raw)}, "data")
			}
			s := sharedStore(t, h)
			s.Append(auditEvent("new"))
			if _, err := s.ReadAudit(context.Background(), "demo"); err == nil || h.updates != 0 {
				t.Fatalf("foreign or invalid ConfigMap was adopted: err=%v updates=%d", err, h.updates)
			}
		})
	}
	h := newCMHarness()
	s := sharedStore(t, h)
	bad := auditEvent("bad")
	bad.Resource.Namespace = "secret"
	s.Append(bad)
	if _, err := s.ReadAudit(context.Background(), "secret"); err == nil || h.reads != 0 || h.updates != 0 {
		t.Fatal("forbidden namespace reached Kubernetes")
	}
}

func TestConfigMapByteEvictionAndNoResurrection(t *testing.T) {
	h := newCMHarness()
	s := sharedStore(t, h)
	base := configMapJournal{Version: 1, ClusterID: "east", Namespace: "demo", CreatedAt: time.Now().UTC(), Events: []v1.AuditEvent{}}
	events := []v1.AuditEvent{}
	for i := 0; i < 2000; i++ {
		e := auditEvent(fmt.Sprintf("id-%04d", i))
		e.Time = e.Time.Add(time.Duration(i) * time.Second)
		e.Reason = strings.Repeat("x", 300)
		events = append(events, e)
	}
	journal, err := s.merge(base, events)
	if err != nil {
		t.Fatal(err)
	}
	journal, raw, err := encodeJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > ConfigMapLimit || len(journal.Events) <= 500 || journal.Evicted == 0 || journal.Floor == nil {
		t.Fatalf("incorrect byte retention: bytes=%d records=%d evicted=%d", len(raw), len(journal.Events), journal.Evicted)
	}
	before := len(journal.Events)
	journal, err = s.merge(journal, events[:2])
	if err != nil || len(journal.Events) != before {
		t.Fatal("retry resurrected evicted records")
	}
	_ = unstructured.SetNestedStringMap(h.cm.Object, map[string]string{ConfigMapDataKey: raw}, "data")
	log, err := s.ReadAudit(context.Background(), "demo")
	if err != nil || log.Retention.EvictedEvents == 0 || len(log.Events) != before {
		t.Fatalf("trimmed journal unreadable: %v", err)
	}
}

func TestConfigMapPendingLimitAndCollision(t *testing.T) {
	h := newCMHarness()
	h.failUpdate = true
	s := sharedStore(t, h)
	oversized := auditEvent("oversized")
	oversized.Reason = strings.Repeat("x", ConfigMapLimit)
	s.Append(oversized)
	h.failUpdate = false
	log, err := s.ReadAudit(context.Background(), "demo")
	if err != nil || !log.Partial || log.Retention.DroppedEvents != 1 {
		t.Fatal("oversized pending record was silently dropped")
	}
	event := auditEvent("collision")
	s.Append(event)
	event.Status = "other"
	s.Append(event)
	if _, err := s.ReadAudit(context.Background(), "demo"); err == nil {
		t.Fatal("conflicting ID overwrote audit history")
	}
}

func TestConfigMapCanceledReadAndInvalidOptions(t *testing.T) {
	h := newCMHarness()
	s := sharedStore(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A held writer cannot turn context cancellation into an unbounded wait.
	s.writeLock <- struct{}{}
	if _, err := s.ReadAudit(ctx, "demo"); err == nil {
		t.Fatal("canceled read ignored deadline")
	}
	<-s.writeLock
	if _, err := NewConfigMap(h.client(), ConfigMapOptions{ClusterID: "east", Name: "INVALID"}); err == nil {
		t.Fatal("invalid ConfigMap name accepted")
	}
}
