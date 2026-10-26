package console

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	v1 "k8gb-ui/api/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

type Config struct {
	Listen        string          `json:"listen"`
	WritesEnabled bool            `json:"writesEnabled"`
	Auth          AuthConfig      `json:"auth"`
	Clusters      []ClusterConfig `json:"clusters"`
}

type ClusterConfig struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	URL             string   `json:"url"`
	GeoTags         []string `json:"geoTags"`
	Zones           []string `json:"zones"`
	Namespaces      []string `json:"namespaces"`
	BearerTokenFile string   `json:"bearerTokenFile"`
	CAFile          string   `json:"caFile"`
	ClientCertFile  string   `json:"clientCertFile"`
	ClientKeyFile   string   `json:"clientKeyFile"`
	WriteTokenFile  string   `json:"writeTokenFile"`
}

func (c ClusterConfig) Public() v1.Cluster {
	return v1.Cluster{ID: c.ID, Name: c.Name, GeoTags: c.GeoTags, Zones: c.Zones}
}

type AuthConfig struct {
	Mode             string    `json:"mode"`
	IssuerURL        string    `json:"issuerURL"`
	IssuerCAFile     string    `json:"issuerCAFile"`
	ClientID         string    `json:"clientID"`
	ClientSecretFile string    `json:"clientSecretFile"`
	RedirectURL      string    `json:"redirectURL"`
	CookieKeyFile    string    `json:"cookieKeyFile"`
	GroupsClaim      string    `json:"groupsClaim"`
	PublicURL        string    `json:"publicURL"`
	UsersFile        string    `json:"usersFile"`
	Bindings         []Binding `json:"bindings"`
	LocalAdmin       bool      `json:"localAdmin"`
}

type Binding struct {
	Role       string   `json:"role,omitempty"`
	Subjects   []string `json:"subjects"`
	Groups     []string `json:"groups"`
	Clusters   []string `json:"clusters"`
	Namespaces []string `json:"namespaces"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return c, fmt.Errorf("parse configuration: %w", err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	for _, p := range []*string{&c.Auth.ClientSecretFile, &c.Auth.CookieKeyFile, &c.Auth.UsersFile, &c.Auth.IssuerCAFile} {
		resolveFile(base, p)
	}
	for i := range c.Clusters {
		for _, p := range []*string{&c.Clusters[i].BearerTokenFile, &c.Clusters[i].CAFile, &c.Clusters[i].ClientCertFile, &c.Clusters[i].ClientKeyFile, &c.Clusters[i].WriteTokenFile} {
			resolveFile(base, p)
		}
	}
	return c, c.Validate()
}

func resolveFile(base string, path *string) {
	if *path != "" && !filepath.IsAbs(*path) {
		*path = filepath.Join(base, *path)
	}
}

func LoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	switch c.Auth.Mode {
	case "local":
		if !LoopbackListen(c.Listen) {
			return fmt.Errorf("local authentication is allowed only on a literal loopback address")
		}
	case "oidc":
		if c.Auth.LocalAdmin {
			return fmt.Errorf("auth.localAdmin is only available in loopback local mode")
		}
		issuer, err := url.Parse(c.Auth.IssuerURL)
		if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
			return fmt.Errorf("auth.issuerURL must be an HTTPS issuer URL")
		}
		redirect, err := url.Parse(c.Auth.RedirectURL)
		if err != nil || redirect.Scheme != "https" || redirect.Hostname() == "" || redirect.Path != "/auth/callback" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" {
			return fmt.Errorf("auth.redirectURL must be an HTTPS URL ending in /auth/callback")
		}
		if c.Auth.ClientID == "" || c.Auth.CookieKeyFile == "" {
			return fmt.Errorf("OIDC requires clientID and cookieKeyFile")
		}
		if c.Auth.GroupsClaim == "" {
			c.Auth.GroupsClaim = "groups"
		}
		if len(c.Auth.Bindings) == 0 {
			return fmt.Errorf("OIDC requires explicit access bindings (default is deny)")
		}
	case "builtin":
		public, err := authHTTPSURL(c.Auth.PublicURL)
		if err != nil || (public.Path != "" && public.Path != "/") || public.RawQuery != "" || public.ForceQuery {
			return fmt.Errorf("builtin authentication requires an HTTPS auth.publicURL with no path or query")
		}
		if c.Auth.LocalAdmin || c.Auth.UsersFile == "" || c.Auth.CookieKeyFile == "" || len(c.Auth.Bindings) == 0 {
			return fmt.Errorf("builtin authentication requires usersFile, cookieKeyFile and explicit access bindings; localAdmin is not allowed")
		}
		if c.Auth.IssuerURL != "" || c.Auth.IssuerCAFile != "" || c.Auth.ClientID != "" || c.Auth.ClientSecretFile != "" || c.Auth.RedirectURL != "" {
			return fmt.Errorf("builtin authentication cannot be combined with OIDC settings")
		}
	default:
		return fmt.Errorf("auth.mode must explicitly be oidc, builtin or local")
	}
	if len(c.Clusters) == 0 {
		return fmt.Errorf("at least one cluster is required")
	}
	ids := map[string]bool{}
	for i := range c.Clusters {
		cluster := &c.Clusters[i]
		if len(validation.IsDNS1123Label(cluster.ID)) != 0 || ids[cluster.ID] {
			return fmt.Errorf("cluster IDs must be unique DNS labels: %q", cluster.ID)
		}
		ids[cluster.ID] = true
		if cluster.Name == "" {
			cluster.Name = cluster.ID
		}
		u, err := url.Parse(cluster.URL)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return fmt.Errorf("cluster %s: invalid agent root URL", cluster.ID)
		}
		loopback := net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()
		if u.Scheme != "https" && !(c.Auth.Mode == "local" && u.Scheme == "http" && loopback) {
			return fmt.Errorf("cluster %s: HTTPS required (HTTP only for loopback local development)", cluster.ID)
		}
		if cluster.BearerTokenFile == "" && !(c.Auth.Mode == "local" && loopback) {
			return fmt.Errorf("cluster %s: bearerTokenFile required", cluster.ID)
		}
		if (cluster.ClientCertFile == "") != (cluster.ClientKeyFile == "") {
			return fmt.Errorf("cluster %s: client certificate and key must be configured together", cluster.ID)
		}
		if len(cluster.Zones) == 0 {
			return fmt.Errorf("cluster %s: configure participating DNS zones explicitly", cluster.ID)
		}
		for j, zone := range cluster.Zones {
			zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
			if len(validation.IsDNS1123Subdomain(zone)) != 0 {
				return fmt.Errorf("cluster %s: invalid zone %q", cluster.ID, zone)
			}
			cluster.Zones[j] = zone
		}
		for _, ns := range cluster.Namespaces {
			if len(validation.IsDNS1123Label(ns)) != 0 {
				return fmt.Errorf("invalid namespace %q", ns)
			}
		}
		for _, tag := range cluster.GeoTags {
			if strings.TrimSpace(tag) == "" {
				return fmt.Errorf("cluster %s: empty GeoTag", cluster.ID)
			}
		}
	}
	for _, b := range c.Auth.Bindings {
		if b.Role != "" && b.Role != "viewer" && b.Role != "admin" {
			return fmt.Errorf("binding role must be viewer or admin")
		}
		if len(b.Subjects)+len(b.Groups) == 0 || len(b.Clusters) == 0 || len(b.Namespaces) == 0 {
			return fmt.Errorf("bindings need subjects/groups, clusters and namespaces")
		}
		for _, id := range b.Clusters {
			if id != "*" && !ids[id] {
				return fmt.Errorf("binding references unknown cluster %q", id)
			}
		}
		for _, ns := range b.Namespaces {
			if ns != "*" && len(validation.IsDNS1123Label(ns)) != 0 {
				return fmt.Errorf("binding has invalid namespace %q", ns)
			}
		}
	}
	return nil
}

type Principal struct {
	Subject string   `json:"subject"`
	Groups  []string `json:"groups"`
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (c Config) Allowed(p Principal, cluster, namespace string) bool {
	if c.Auth.Mode == "local" {
		return true
	}
	for _, b := range c.Auth.Bindings {
		match := contains(b.Subjects, p.Subject)
		for _, g := range p.Groups {
			match = match || contains(b.Groups, g)
		}
		if match && (contains(b.Clusters, "*") || contains(b.Clusters, cluster)) && (namespace == "" || contains(b.Namespaces, "*") || contains(b.Namespaces, namespace)) {
			return true
		}
	}
	return false
}

func (c ClusterConfig) AllowsNamespace(namespace string) bool {
	return len(c.Namespaces) == 0 || contains(c.Namespaces, namespace)
}

func (c Config) CanWrite(p Principal, cluster, namespace string) bool {
	if !c.WritesEnabled || namespace == "" {
		return false
	}
	configured := false
	for _, site := range c.Clusters {
		if site.ID == cluster && site.AllowsNamespace(namespace) && site.WriteTokenFile != "" {
			configured = true
		}
	}
	if !configured {
		return false
	}
	if c.Auth.Mode == "local" {
		return c.Auth.LocalAdmin
	}
	for _, binding := range c.Auth.Bindings {
		if binding.Role != "admin" {
			continue
		}
		match := contains(binding.Subjects, p.Subject)
		for _, group := range p.Groups {
			match = match || contains(binding.Groups, group)
		}
		if match && (contains(binding.Clusters, "*") || contains(binding.Clusters, cluster)) && (contains(binding.Namespaces, "*") || contains(binding.Namespaces, namespace)) {
			return true
		}
	}
	return false
}
