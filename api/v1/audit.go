package v1

import "time"

// AuditRetention describes only the retained window, not a durable audit trail.
type AuditRetention struct {
	Storage       string    `json:"storage"`
	Scope         string    `json:"scope"`
	InstanceID    string    `json:"instanceId,omitempty"`
	StartedAt     time.Time `json:"startedAt,omitempty"`
	LimitBytes    int       `json:"limitBytes"`
	RetainedBytes int       `json:"retainedBytes"`
	EvictedEvents uint64    `json:"evictedEvents"`
	ConfigMap     string    `json:"configMap,omitempty"`
	PendingEvents int       `json:"pendingEvents,omitempty"`
	DroppedEvents uint64    `json:"droppedEvents,omitempty"`
}

type AgentAudit struct {
	APIVersion string          `json:"apiVersion"`
	ClusterID  string          `json:"clusterId"`
	Namespace  string          `json:"namespace"`
	Events     []AuditEvent    `json:"events"`
	Retention  *AuditRetention `json:"retention,omitempty"`
	Partial    bool            `json:"partial,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
}

type AuditSource struct {
	ClusterID string          `json:"clusterId"`
	Available bool            `json:"available"`
	Events    int             `json:"events"`
	Retention *AuditRetention `json:"retention,omitempty"`
	Error     string          `json:"error,omitempty"`
	Partial   bool            `json:"partial,omitempty"`
	Warnings  []string        `json:"warnings,omitempty"`
}

type AuditOverview struct {
	APIVersion  string        `json:"apiVersion"`
	Namespace   string        `json:"namespace"`
	CollectedAt time.Time     `json:"collectedAt"`
	Partial     bool          `json:"partial"`
	Events      []AuditEvent  `json:"events"`
	Sources     []AuditSource `json:"sources"`
	Warnings    []string      `json:"warnings"`
}
