package v1

import (
	"encoding/json"
	"time"
)

type TraefikOptions struct {
	Hostname       string              `json:"hostname,omitempty"`
	MTLS           bool                `json:"mtls"`
	CommonNames    []string            `json:"commonNames"`
	Local          bool                `json:"local"`
	Entrypoint     string              `json:"entrypoint,omitempty"`
	MTLSEntrypoint string              `json:"mtlsEntrypoint,omitempty"`
	TLSOption      *TLSOptionReference `json:"tlsOption,omitempty"`
}

type TLSOptionReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

type TraefikTLSOption struct {
	Resource       ResourceReference `json:"resource"`
	ClientAuthType string            `json:"clientAuthType"`
	CASecretNames  []string          `json:"caSecretNames"`
	Eligible       bool              `json:"eligible"`
	Reason         string            `json:"reason,omitempty"`
}

type TraefikSecurityCatalog struct {
	Entrypoints         []string           `json:"entrypoints"`
	OrdinaryEntrypoints []string           `json:"ordinaryEntrypoints"`
	TLSOptions          []TraefikTLSOption `json:"tlsOptions"`
	PluginAlias         string             `json:"pluginAlias,omitempty"`
	SANFilterConfigured bool               `json:"sanFilterConfigured"`
	Warnings            []string           `json:"warnings"`
}

// Actor is supplied by authenticated Console, never accepted from browser identity headers.
type MutationRequest struct {
	Provider  string             `json:"provider"`
	Operation string             `json:"operation"`
	Resource  ResourceReference  `json:"resource"`
	Target    *ResourceReference `json:"target,omitempty"`
	Policy    *Policy            `json:"policy,omitempty"`
	Traefik   *TraefikOptions    `json:"traefik,omitempty"`
	Traffic   *TrafficOptions    `json:"traffic,omitempty"`
	Actor     string             `json:"actor"`
	Reason    string             `json:"reason"`
	DNSZone   string             `json:"dnsZone,omitempty"`
}

// TrafficOptions deliberately accepts addresses, never environment-specific COD names.
type TrafficOptions struct {
	IPs []string `json:"ips,omitempty"`
}

type Change struct {
	Path   string          `json:"path"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

type MutationPlan struct {
	APIVersion   string              `json:"apiVersion"`
	ClusterID    string              `json:"clusterId"`
	Request      MutationRequest     `json:"request"`
	Changes      []Change            `json:"changes"`
	Dependencies []ResourceReference `json:"dependencies"`
	PlannedAt    time.Time           `json:"plannedAt"`
	ExpiresAt    time.Time           `json:"expiresAt"`
	Digest       string              `json:"digest"`
	Token        string              `json:"token"`
	Noop         bool                `json:"noop"`
	Warnings     []string            `json:"warnings"`
}

type MutationResult struct {
	APIVersion     string            `json:"apiVersion"`
	ClusterID      string            `json:"clusterId"`
	Resource       ResourceReference `json:"resource"`
	Status         string            `json:"status"`
	Digest         string            `json:"digest"`
	Reconciliation string            `json:"reconciliation"`
	Warnings       []string          `json:"warnings"`
}

type MutationError struct {
	Code           string `json:"code"`
	Error          string `json:"error"`
	OutcomeUnknown bool   `json:"outcomeUnknown"`
}

type MutationTarget struct {
	ClusterID string          `json:"clusterId"`
	Request   MutationRequest `json:"request"`
}

type BatchPlanRequest struct {
	Targets []MutationTarget `json:"targets"`
}
type TargetPlan struct {
	ClusterID string         `json:"clusterId"`
	Plan      *MutationPlan  `json:"plan,omitempty"`
	Error     *MutationError `json:"error,omitempty"`
}
type BatchPlan struct {
	APIVersion string       `json:"apiVersion"`
	Targets    []TargetPlan `json:"targets"`
	CanApply   bool         `json:"canApply"`
}
type BatchApplyRequest struct {
	Targets      []TargetPlan `json:"targets"`
	AllowPartial bool         `json:"allowPartial"`
}
type TargetResult struct {
	ClusterID string          `json:"clusterId"`
	Result    *MutationResult `json:"result,omitempty"`
	Error     *MutationError  `json:"error,omitempty"`
}
type BatchResult struct {
	APIVersion string         `json:"apiVersion"`
	Targets    []TargetResult `json:"targets"`
}

type Candidate struct {
	Resource            ResourceReference `json:"resource"`
	Provider            string            `json:"provider"`
	Hostnames           []string          `json:"hostnames"`
	Eligible            bool              `json:"eligible"`
	Reason              string            `json:"reason,omitempty"`
	Enabled             bool              `json:"enabled"`
	Policy              *Policy           `json:"policy,omitempty"`
	Traefik             *TraefikOptions   `json:"traefik,omitempty"`
	Traffic             *TrafficStatus    `json:"traffic,omitempty"`
	SiteTrafficEligible bool              `json:"siteTrafficEligible"`
	SiteTrafficReason   string            `json:"siteTrafficReason,omitempty"`
}
type Catalog struct {
	APIVersion      string                  `json:"apiVersion"`
	ClusterID       string                  `json:"clusterId"`
	Namespace       string                  `json:"namespace"`
	Capabilities    Capabilities            `json:"capabilities"`
	Candidates      []Candidate             `json:"candidates"`
	Warnings        []string                `json:"warnings"`
	DNSZones        *DNSZoneCatalog         `json:"dnsZones,omitempty"`
	TraefikDomains  []string                `json:"traefikDomains,omitempty"`
	TraefikSecurity *TraefikSecurityCatalog `json:"traefikSecurity,omitempty"`
}

type DNSZoneCatalog struct {
	Available bool     `json:"available"`
	Zones     []string `json:"zones"`
	Source    string   `json:"source"`
	Error     string   `json:"error,omitempty"`
}
type CatalogSite struct {
	ClusterID string   `json:"clusterId"`
	Catalog   *Catalog `json:"catalog,omitempty"`
	Error     string   `json:"error,omitempty"`
}
type CatalogOverview struct {
	APIVersion string        `json:"apiVersion"`
	Namespace  string        `json:"namespace"`
	Sites      []CatalogSite `json:"sites"`
}

type AuditEvent struct {
	ID             string            `json:"id,omitempty"`
	Time           time.Time         `json:"time"`
	ClusterID      string            `json:"clusterId"`
	Actor          string            `json:"actor"`
	Provider       string            `json:"provider"`
	Operation      string            `json:"operation"`
	Resource       ResourceReference `json:"resource"`
	Reason         string            `json:"reason"`
	Status         string            `json:"status"`
	Digest         string            `json:"digest,omitempty"`
	Changes        []Change          `json:"changes,omitempty"`
	OutcomeUnknown bool              `json:"outcomeUnknown"`
}
