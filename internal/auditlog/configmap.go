package auditlog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

const ConfigMapLimit = 700 * 1024
const ConfigMapDataKey = "events.json"
const ConfigMapLabel = "k8gb-ui.io/audit-journal"

var configMaps = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

type ConfigMapOptions struct {
	ClusterID         string
	Name              string
	AllowedNamespaces []string
	Timeout           time.Duration
}

type pendingEvent struct {
	event v1.AuditEvent
	size  int
}

type ConfigMapStore struct {
	client       dynamic.Interface
	opts         ConfigMapOptions
	mu           sync.Mutex
	pending      []pendingEvent
	pendingBytes int
	dropped      map[string]uint64
	writeLock    chan struct{}
	logger       *slog.Logger
}

type position struct {
	Time time.Time `json:"time"`
	ID   string    `json:"id"`
}

type configMapJournal struct {
	Version   int             `json:"version"`
	ClusterID string          `json:"clusterId"`
	Namespace string          `json:"namespace"`
	CreatedAt time.Time       `json:"createdAt"`
	Evicted   uint64          `json:"evictedEvents"`
	Floor     *position       `json:"floor,omitempty"`
	Events    []v1.AuditEvent `json:"events"`
}

func NewConfigMap(client dynamic.Interface, opts ConfigMapOptions) (*ConfigMapStore, error) {
	if client == nil || len(validation.IsDNS1123Label(opts.ClusterID)) != 0 || len(validation.IsDNS1123Subdomain(opts.Name)) != 0 {
		return nil, fmt.Errorf("ConfigMap audit requires a client, DNS-label cluster ID and explicit valid ConfigMap name")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Second
	}
	if opts.Timeout < 100*time.Millisecond || opts.Timeout > 10*time.Second {
		return nil, fmt.Errorf("audit timeout must be between 100ms and 10s")
	}
	opts.AllowedNamespaces = append([]string(nil), opts.AllowedNamespaces...)
	for _, ns := range opts.AllowedNamespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return nil, fmt.Errorf("invalid audit namespace")
		}
	}
	return &ConfigMapStore{client: client, opts: opts, dropped: map[string]uint64{}, writeLock: make(chan struct{}, 1), logger: slog.New(slog.NewJSONHandler(os.Stdout, nil))}, nil
}

func (s *ConfigMapStore) allowed(ns string) bool {
	if len(validation.IsDNS1123Label(ns)) != 0 {
		return false
	}
	if len(s.opts.AllowedNamespaces) == 0 {
		return true
	}
	for _, allowed := range s.opts.AllowedNamespaces {
		if allowed == ns {
			return true
		}
	}
	return false
}

func eventKey(event v1.AuditEvent) string {
	if event.ID != "" {
		return event.ID
	}
	// Existing exported audit can be seeded before upgrading an agent. Legacy
	// records keep their original payload; content IDs are internal to the journal.
	raw, _ := json.Marshal(event)
	sum := sha256.Sum256(raw)
	return "legacy:" + hex.EncodeToString(sum[:])
}

func eventPosition(event v1.AuditEvent) position {
	return position{Time: event.Time, ID: eventKey(event)}
}
func comparePosition(a, b position) int {
	if a.Time.Before(b.Time) {
		return -1
	}
	if a.Time.After(b.Time) {
		return 1
	}
	if a.ID < b.ID {
		return -1
	}
	if a.ID > b.ID {
		return 1
	}
	return 0
}

func (s *ConfigMapStore) load(ctx context.Context, ns string) (*unstructured.Unstructured, configMapJournal, error) {
	var journal configMapJournal
	if err := ctx.Err(); err != nil {
		return nil, journal, err
	}
	if !s.allowed(ns) {
		return nil, journal, fmt.Errorf("audit namespace outside configured scope")
	}
	cm, err := s.client.Resource(configMaps).Namespace(ns).Get(ctx, s.opts.Name, metav1.GetOptions{})
	if err != nil {
		return nil, journal, err
	}
	if cm.GetNamespace() != ns || cm.GetName() != s.opts.Name || cm.GetUID() == "" || cm.GetResourceVersion() == "" || cm.GetLabels()[ConfigMapLabel] != "true" || cm.GetDeletionTimestamp() != nil {
		return nil, journal, fmt.Errorf("ConfigMap audit identity or ownership is invalid")
	}
	immutable, _, immutableErr := unstructured.NestedBool(cm.Object, "immutable")
	binary, _, binaryErr := unstructured.NestedMap(cm.Object, "binaryData")
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	if err != nil || immutableErr != nil || binaryErr != nil || immutable || len(binary) > 0 || len(data) > 1 {
		return nil, journal, fmt.Errorf("ConfigMap is immutable or contains unrelated data")
	}
	for key := range data {
		if key != ConfigMapDataKey {
			return nil, journal, fmt.Errorf("ConfigMap contains unrelated data")
		}
	}
	raw := data[ConfigMapDataKey]
	if raw == "" {
		journal = configMapJournal{Version: 1, ClusterID: s.opts.ClusterID, Namespace: ns, CreatedAt: cm.GetCreationTimestamp().Time, Events: []v1.AuditEvent{}}
		return cm, journal, nil
	}
	if len(raw) > ConfigMapLimit {
		return nil, journal, fmt.Errorf("audit journal exceeds byte limit")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&journal) != nil || decoder.Decode(new(any)) != io.EOF || journal.Version != 1 || journal.ClusterID != s.opts.ClusterID || journal.Namespace != ns || journal.CreatedAt.IsZero() || journal.Events == nil || journal.Floor != nil && (journal.Floor.Time.IsZero() || journal.Floor.ID == "") {
		return nil, journal, fmt.Errorf("audit journal schema or scope is invalid")
	}
	seen := map[string]bool{}
	for i, event := range journal.Events {
		key := eventKey(event)
		if event.ClusterID != s.opts.ClusterID || event.Resource.Namespace != ns || event.Time.IsZero() || seen[key] || journal.Floor != nil && comparePosition(eventPosition(event), *journal.Floor) <= 0 || i > 0 && comparePosition(eventPosition(journal.Events[i-1]), eventPosition(event)) >= 0 {
			return nil, journal, fmt.Errorf("audit journal has invalid, unordered or duplicate records")
		}
		seen[key] = true
	}
	return cm, journal, nil
}

// Check never creates/adopts ConfigMaps. The operator provisions the dedicated
// labeled journal and get/update permissions separately from application writes.
func (s *ConfigMapStore) Check(ctx context.Context, ns string) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	_, _, err := s.load(ctx, ns)
	return err
}

func (s *ConfigMapStore) Append(event v1.AuditEvent) {
	if !s.allowed(event.Resource.Namespace) || event.ClusterID != s.opts.ClusterID || event.Time.IsZero() {
		s.logger.Warn("audit_not_persisted", "reason", "invalid event scope or time", "eventId", event.ID)
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	s.mu.Lock()
	if len(data) > ConfigMapLimit {
		s.dropped[event.Resource.Namespace]++
		s.mu.Unlock()
		s.logger.Error("audit_pending_overflow", "eventId", event.ID, "namespace", event.Resource.Namespace)
		return
	}
	for len(s.pending) > 0 && s.pendingBytes+len(data) > ConfigMapLimit {
		old := s.pending[0]
		s.pending = s.pending[1:]
		s.pendingBytes -= old.size
		s.dropped[old.event.Resource.Namespace]++
	}
	// Clone request maps/slices: the stdout event and caller remain independent.
	var copy v1.AuditEvent
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	_ = decoder.Decode(&copy)
	s.pending = append(s.pending, pendingEvent{event: copy, size: len(data)})
	s.pendingBytes += len(data)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.Timeout)
	defer cancel()
	if err := s.flushNamespace(ctx, event.Resource.Namespace); err != nil {
		s.logger.Error("audit_persistence_failed", "cluster", s.opts.ClusterID, "namespace", event.Resource.Namespace, "eventId", event.ID, "error", err)
	}
}

func (s *ConfigMapStore) pendingFor(ns string) ([]v1.AuditEvent, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []v1.AuditEvent{}
	for _, event := range s.pending {
		if event.event.Resource.Namespace == ns {
			result = append(result, event.event)
		}
	}
	return result, s.dropped[ns]
}

func (s *ConfigMapStore) acknowledge(ns string, events []v1.AuditEvent) {
	seen := map[string]string{}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		seen[eventKey(event)] = string(raw)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.pending[:0]
	size := 0
	for _, event := range s.pending {
		raw, _ := json.Marshal(event.event)
		if event.event.Resource.Namespace != ns || seen[eventKey(event.event)] != string(raw) {
			kept = append(kept, event)
			size += event.size
		}
	}
	s.pending = kept
	s.pendingBytes = size
}

func (s *ConfigMapStore) merge(journal configMapJournal, events []v1.AuditEvent) (configMapJournal, error) {
	byID := map[string]v1.AuditEvent{}
	for _, event := range journal.Events {
		byID[eventKey(event)] = event
	}
	for _, event := range events {
		key := eventKey(event)
		if old, exists := byID[key]; exists {
			a, _ := json.Marshal(old)
			b, _ := json.Marshal(event)
			if !bytes.Equal(a, b) {
				return journal, fmt.Errorf("audit ID collision")
			}
			continue
		}
		// A committed event may have been evicted after an ambiguous Update reply.
		// The chronological floor prevents retry from resurrecting that history.
		if journal.Floor != nil && comparePosition(eventPosition(event), *journal.Floor) <= 0 {
			continue
		}
		byID[key] = event
	}
	journal.Events = []v1.AuditEvent{}
	for _, event := range byID {
		journal.Events = append(journal.Events, event)
	}
	sort.Slice(journal.Events, func(i, j int) bool {
		return comparePosition(eventPosition(journal.Events[i]), eventPosition(journal.Events[j])) < 0
	})
	if journal.CreatedAt.IsZero() {
		journal.CreatedAt = time.Now().UTC()
	}
	return journal, nil
}

func encodeJournal(journal configMapJournal) (configMapJournal, string, error) {
	sizes := make([]int, len(journal.Events))
	total := 0
	for i, event := range journal.Events {
		raw, err := json.Marshal(event)
		if err != nil {
			return journal, "", err
		}
		sizes[i] = len(raw)
		total += len(raw)
	}
	dropped := 0
	for {
		header := journal
		header.Events = []v1.AuditEvent{}
		raw, err := json.Marshal(header)
		if err != nil {
			return journal, "", err
		}
		commas := len(journal.Events) - 1
		if commas < 0 {
			commas = 0
		}
		if len(raw)+total+commas <= ConfigMapLimit {
			encoded, err := json.Marshal(journal)
			return journal, string(encoded), err
		}
		if len(journal.Events) == 0 {
			return journal, "", fmt.Errorf("journal metadata exceeds byte limit")
		}
		floor := eventPosition(journal.Events[0])
		journal.Floor = &floor
		journal.Events = journal.Events[1:]
		journal.Evicted++
		total -= sizes[dropped]
		dropped++
	}
}

func (s *ConfigMapStore) flushNamespace(ctx context.Context, ns string) error {
	select {
	case s.writeLock <- struct{}{}:
		defer func() { <-s.writeLock }()
	case <-ctx.Done():
		return ctx.Err()
	}
	events, _ := s.pendingFor(ns)
	if len(events) == 0 {
		return nil
	}
	for attempt := 0; attempt < 8; attempt++ {
		cm, journal, err := s.load(ctx, ns)
		if err != nil {
			return err
		}
		journal, err = s.merge(journal, events)
		if err != nil {
			return err
		}
		_, raw, err := encodeJournal(journal)
		if err != nil {
			return err
		}
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		if data[ConfigMapDataKey] == raw {
			s.acknowledge(ns, events)
			return nil
		}
		desired := cm.DeepCopy()
		if err := unstructured.SetNestedStringMap(desired.Object, map[string]string{ConfigMapDataKey: raw}, "data"); err != nil {
			return err
		}
		_, err = s.client.Resource(configMaps).Namespace(ns).Update(ctx, desired, metav1.UpdateOptions{})
		if err == nil {
			s.acknowledge(ns, events)
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("ConfigMap audit conflict retry limit exceeded")
}

func (s *ConfigMapStore) Flush(ctx context.Context) error {
	s.mu.Lock()
	set := map[string]bool{}
	for _, event := range s.pending {
		set[event.event.Resource.Namespace] = true
	}
	s.mu.Unlock()
	names := []string{}
	for ns := range set {
		names = append(names, ns)
	}
	sort.Strings(names)
	for _, ns := range names {
		if err := s.flushNamespace(ctx, ns); err != nil {
			return err
		}
	}
	return nil
}

// Only unsaved audit records are retried. There is no mutation retry or polling
// of remote clusters, and idle replicas issue no background Kubernetes reads.
func (s *ConfigMapStore) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			child, cancel := context.WithTimeout(ctx, s.opts.Timeout)
			if err := s.Flush(child); err != nil {
				s.logger.Warn("audit_retry_failed", "cluster", s.opts.ClusterID, "error", err)
			}
			cancel()
		}
	}
}

func (s *ConfigMapStore) ReadAudit(ctx context.Context, ns string) (v1.AgentAudit, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	if !s.allowed(ns) {
		return v1.AgentAudit{}, fmt.Errorf("audit namespace outside configured scope")
	}
	if err := s.flushNamespace(ctx, ns); err != nil {
		return v1.AgentAudit{}, err
	}
	cm, journal, err := s.load(ctx, ns)
	if err != nil {
		return v1.AgentAudit{}, err
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	pending, dropped := s.pendingFor(ns)
	result := v1.AgentAudit{APIVersion: v1.Version, ClusterID: s.opts.ClusterID, Namespace: ns, Events: journal.Events, Retention: &v1.AuditRetention{Storage: "configMap", Scope: "namespace", InstanceID: string(cm.GetUID()), StartedAt: journal.CreatedAt, LimitBytes: ConfigMapLimit, RetainedBytes: len(data[ConfigMapDataKey]), EvictedEvents: journal.Evicted, ConfigMap: ns + "/" + s.opts.Name, PendingEvents: len(pending), DroppedEvents: dropped}}
	if len(pending) > 0 || dropped > 0 {
		result.Partial = true
		result.Warnings = []string{"Some local audit events are pending persistence or were dropped; stdout remains the full external log sink."}
	}
	return result, nil
}
