// Package v1 defines the versioned Console/agent contract.
package v1

import "time"

const Version = "k8gb-ui/v1alpha1"

type Capabilities struct {
	ReadOnly   bool                `json:"readOnly"`
	Providers  []string            `json:"providers"`
	Operations map[string][]string `json:"operations,omitempty"`
}

type ResourceReference struct {
	APIVersion      string `json:"apiVersion"`
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	// Only signed mutation dependencies use this server-derived Gslb configuration pin.
	ConfigurationDigest string `json:"configurationDigest,omitempty"`
}

type Policy struct {
	Strategy      string           `json:"strategy"`
	PrimaryGeoTag string           `json:"primaryGeoTag,omitempty"`
	DNSTTLSeconds int64            `json:"dnsTtlSeconds,omitempty"`
	Weights       map[string]int64 `json:"weights,omitempty"`
}

type Backend struct {
	Namespace             string `json:"namespace"`
	Service               string `json:"service"`
	Port                  string `json:"port,omitempty"`
	Health                string `json:"health"`
	EndpointDataAvailable bool   `json:"endpointDataAvailable"`
	ReadyEndpoints        int    `json:"readyEndpoints"`
	TotalEndpoints        int    `json:"totalEndpoints"`
}

// Object is one hostname of a Gslb. A multi-host Gslb produces several objects
// with the same resource reference, rather than hiding all but its first host.
type Object struct {
	Resource    ResourceReference  `json:"resource"`
	Target      *ResourceReference `json:"target,omitempty"`
	Hostname    string             `json:"hostname"`
	GeoTag      string             `json:"geoTag,omitempty"`
	Policy      Policy             `json:"policy"`
	Health      string             `json:"health"`
	DNSTargets  []string           `json:"dnsTargets"`
	DNSObserved bool               `json:"dnsObserved"`
	Backends    []Backend          `json:"backends"`
	Warnings    []string           `json:"warnings"`
	Source      *ResourceReference `json:"source,omitempty"`
	Management  string             `json:"management,omitempty"`
	Traffic     TrafficStatus      `json:"traffic"`
}

type TrafficStatus struct {
	Excluded         bool     `json:"excluded"`
	IPs              []string `json:"ips"`
	Hostnames        []string `json:"hostnames"`
	OverrideManaged  bool     `json:"overrideManaged"`
	OverrideConflict bool     `json:"overrideConflict"`
}

type Snapshot struct {
	APIVersion   string       `json:"apiVersion"`
	ClusterID    string       `json:"clusterId"`
	Namespace    string       `json:"namespace"`
	CollectedAt  time.Time    `json:"collectedAt"`
	Capabilities Capabilities `json:"capabilities"`
	// ObjectsComplete concerns the primary Gslb inventory, not optional endpoint reads.
	ObjectsComplete bool     `json:"objectsComplete"`
	Objects         []Object `json:"objects"`
	Warnings        []string `json:"warnings"`
}

type NamespaceList struct {
	APIVersion string   `json:"apiVersion"`
	ClusterID  string   `json:"clusterId"`
	Namespaces []string `json:"namespaces"`
}

type Error struct {
	Error string `json:"error"`
}

type Cluster struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	GeoTags []string `json:"geoTags"`
	Zones   []string `json:"zones"`
}

type Site struct {
	Cluster         Cluster      `json:"cluster"`
	Available       bool         `json:"available"`
	ObjectsComplete bool         `json:"objectsComplete"`
	CollectedAt     time.Time    `json:"collectedAt,omitempty"`
	Error           string       `json:"error,omitempty"`
	Warnings        []string     `json:"warnings"`
	Capabilities    Capabilities `json:"capabilities"`
}

type Signal struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type Instance struct {
	ClusterID string `json:"clusterId"`
	Object
}

type Group struct {
	ID        string     `json:"id"`
	Namespace string     `json:"namespace"`
	Hostname  string     `json:"hostname"`
	Zone      string     `json:"zone"`
	Instances []Instance `json:"instances"`
	Signals   []Signal   `json:"signals"`
	State     string     `json:"state"`
}

type Overview struct {
	APIVersion   string       `json:"apiVersion"`
	Namespace    string       `json:"namespace"`
	CollectedAt  time.Time    `json:"collectedAt"`
	Sites        []Site       `json:"sites"`
	Groups       []Group      `json:"groups"`
	Capabilities Capabilities `json:"capabilities"`
}
