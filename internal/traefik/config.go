// Package traefik implements an optional, stateless IngressRoute adapter.
package traefik

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	Prefix                 = "k8gb-ui.io/"
	OriginalEntrypoints    = Prefix + "original-entrypoints"
	OriginalTLSOptions     = Prefix + "original-tls-options"
	RouteIndex             = Prefix + "route-index"
	ManagedLabel           = Prefix + "managed-by"
	OwnerLabel             = Prefix + "route-uid"
	SiteExcludedAnnotation = Prefix + "site-excluded"
	manager                = "traefik-adapter"
)

type Config struct {
	Domains                                                     []string
	Domain, LocalDomain, IngressClass                           string
	MTLSEntrypoint, MTLSTLSOption, CNPlugin, BlockingMiddleware string
	AllowedGeoTags                                              []string
	OrdinaryEntrypoints                                         []string
	MTLSEntrypoints                                             []string
	TLSOptionNamespaces                                         []string
	// WebhookConfig optionally requires a fail-closed, side-effect-free MWC.
	WebhookConfig string
	// Empty means all namespaces for admission/provider; sweeps require an allowlist.
	AllowedNamespaces []string
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func defaults(c Config) Config {
	if c.MTLSEntrypoint == "" {
		c.MTLSEntrypoint = "mtls"
	}
	if c.OrdinaryEntrypoints == nil {
		c.OrdinaryEntrypoints = []string{"websecure", "grpc", "grpctls"}
	}
	return c
}

func ValidateConfig(cfg Config) error {
	c := defaults(cfg)
	if len(c.OrdinaryEntrypoints) == 0 || len(c.OrdinaryEntrypoints) > 64 || len(c.AllowedNamespaces) > 128 || len(c.AllowedGeoTags) > 128 || len(c.MTLSEntrypoints) > 64 || len(c.TLSOptionNamespaces) > 32 {
		return fmt.Errorf("configuration list size is out of range")
	}
	if len(c.Domains) > 128 {
		return fmt.Errorf("too many configured DNS domains")
	}
	for _, d := range append(Domains(c), c.LocalDomain) {
		if d != "" && !validHost(d) {
			return fmt.Errorf("invalid configured DNS domain %q", d)
		}
	}
	if slices.Contains(Domains(c), c.LocalDomain) {
		return fmt.Errorf("Domain and LocalDomain must differ")
	}
	if !identifier.MatchString(c.MTLSEntrypoint) {
		return fmt.Errorf("invalid mTLS entrypoint")
	}
	if c.IngressClass != "" && len(validation.IsDNS1123Subdomain(c.IngressClass)) != 0 {
		return fmt.Errorf("invalid ingress class")
	}
	for _, value := range []string{c.CNPlugin, c.BlockingMiddleware} {
		if value != "" && !identifier.MatchString(value) {
			return fmt.Errorf("invalid plugin or blocking middleware identifier")
		}
	}
	if strings.ContainsAny(c.MTLSTLSOption, ", \t\n\r") {
		return fmt.Errorf("invalid TLS option reference")
	}
	if c.WebhookConfig != "" && len(validation.IsDNS1123Subdomain(c.WebhookConfig)) != 0 {
		return fmt.Errorf("invalid webhook configuration name")
	}
	if err := validEntrypoints(c.OrdinaryEntrypoints, c); err != nil {
		return err
	}
	seenEntry := map[string]bool{}
	for _, name := range c.MTLSEntrypoints {
		if !identifier.MatchString(name) || seenEntry[name] {
			return fmt.Errorf("invalid or repeated allowed mTLS entrypoint")
		}
		seenEntry[name] = true
	}
	seenNS := map[string]bool{}
	for _, ns := range c.TLSOptionNamespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 || seenNS[ns] {
			return fmt.Errorf("invalid or repeated TLSOption namespace")
		}
		seenNS[ns] = true
	}
	seen := map[string]bool{}
	for _, tag := range c.AllowedGeoTags {
		if !identifier.MatchString(tag) || seen[tag] {
			return fmt.Errorf("invalid or repeated geo tag %q", tag)
		}
		seen[tag] = true
	}
	seen = map[string]bool{}
	for _, ns := range c.AllowedNamespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 || seen[ns] {
			return fmt.Errorf("invalid or repeated namespace %q", ns)
		}
		seen[ns] = true
	}
	return nil
}

// Domains is the explicit adapter allowlist, not a replacement for controller zone discovery.
func Domains(c Config) []string {
	result := append([]string{}, c.Domains...)
	if c.Domain != "" && !slices.Contains(result, c.Domain) {
		result = append(result, c.Domain)
	}
	return result
}

func validHost(s string) bool {
	return s != "" && s == strings.ToLower(s) && len(validation.IsDNS1123Subdomain(s)) == 0 && strings.Contains(s, ".")
}

func validEntrypoints(names []string, c Config) error {
	seen := map[string]bool{}
	for _, name := range names {
		if !identifier.MatchString(name) || c.MTLSEntrypoints == nil && name == c.MTLSEntrypoint || seen[name] {
			return fmt.Errorf("invalid ordinary entrypoint %q", name)
		}
		seen[name] = true
	}
	return nil
}

func namespaceAllowed(ns string, c Config) bool {
	return ns != "" && (len(c.AllowedNamespaces) == 0 || slices.Contains(c.AllowedNamespaces, ns))
}

func routeGVR(apiVersion string) (schema.GroupVersionResource, error) {
	for _, group := range []string{"traefik.io", "traefik.containo.us"} {
		if apiVersion == group+"/v1alpha1" {
			return schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "ingressroutes"}, nil
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("only traefik.io and traefik.containo.us v1alpha1 IngressRoutes are supported")
}
