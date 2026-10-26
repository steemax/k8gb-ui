package traefik

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"slices"
	"strconv"
	"strings"

	v1 "k8gb-ui/api/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// New fields use Prefix; edits retain existing historical key spellings.
var aliases = map[string][]string{
	"gslb": {"gslb"}, "strategy": {"gslb_strategy"},
	"hostname": {"gslb_hostname"}, "geotag": {"gslb_geotag", "geotag"},
	"dnsttl": {"gslb_dnsttl", "dnsttl"}, "weight_enable": {"gslb_weight_enable", "weight_enable"},
	"weights": {"gslb_weights", "weights"}, "mtls": {"gslb_mtls", "mtls"},
	"mtls_cn_check": {"gslb_mtls_cn_check", "mtls_cn_check"},
	"local":         {"gslb_local", "local"}, "entrypoint": {"gslb_entrypoint", "entrypoint"},
	"mtls_entrypoint": {}, "mtls_tlsoption": {},
}

func annotation(a map[string]string, key string) (string, error) {
	value, present := a[Prefix+key]
	for _, alias := range aliases[key] {
		if v, ok := a[alias]; ok {
			if present && value != v {
				return "", fmt.Errorf("conflicting annotations for %s", key)
			}
			value, present = v, true
		}
	}
	return value, nil
}

func setAnnotation(a map[string]string, key, value string) {
	keys := append([]string{Prefix + key}, aliases[key]...)
	present := false
	for _, spelling := range keys {
		if _, exists := a[spelling]; !exists {
			continue
		}
		present = true
		if value == "" {
			delete(a, spelling)
		} else {
			a[spelling] = value
		}
	}
	if !present && value != "" {
		a[Prefix+key] = value
	}
}

func flag(a map[string]string, key string) (bool, error) {
	s, err := annotation(a, key)
	if err != nil {
		return false, err
	}
	if s != "" && s != "true" && s != "false" {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return s == "true", nil
}

type target struct {
	host, localHost, service string
	port                     int64
}
type routeSettings struct {
	enabled  bool
	excluded bool
	policy   v1.Policy
	options  v1.TraefikOptions
	target   target
}

// requiredHost proves the Host matcher is required on every successful branch.
// Other matchers contribute no Host guarantee, even if their arguments contain one.
func requiredHost(expr ast.Expr) bool {
	switch node := expr.(type) {
	case *ast.ParenExpr:
		return requiredHost(node.X)
	case *ast.BinaryExpr:
		if node.Op == token.LAND {
			return requiredHost(node.X) || requiredHost(node.Y)
		}
		if node.Op == token.LOR {
			return requiredHost(node.X) && requiredHost(node.Y)
		}
	case *ast.CallExpr:
		id, ok := node.Fun.(*ast.Ident)
		return ok && id.Name == "Host" && len(node.Args) == 1
	}
	return false
}

func literalHost(match string) (string, error) {
	if len(match) > 16384 {
		return "", fmt.Errorf("route match is too large")
	}
	expr, err := parser.ParseExpr(match)
	if err != nil {
		return "", fmt.Errorf("invalid route match expression: %w", err)
	}
	var hosts []string
	invalid := false
	ast.Inspect(expr, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "Host" {
			return true
		}
		if len(call.Args) != 1 {
			invalid = true
			return false
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			invalid = true
			return false
		}
		host, err := strconv.Unquote(literal.Value)
		if err != nil || !validHost(host) {
			invalid = true
			return false
		}
		hosts = append(hosts, host)
		return false
	})
	if invalid || len(hosts) != 1 || !requiredHost(expr) {
		return "", fmt.Errorf("selected route must require one unambiguous literal Host matcher on every branch")
	}
	return hosts[0], nil
}

func selectTarget(u *unstructured.Unstructured, c Config) (target, error) {
	var t target
	routes, _, err := unstructured.NestedSlice(u.Object, "spec", "routes")
	if err != nil || len(routes) == 0 {
		return t, fmt.Errorf("spec.routes is required")
	}
	i := 0
	if raw, ok := u.GetAnnotations()[RouteIndex]; ok {
		if raw != "0" && raw != "1" {
			return t, fmt.Errorf("%s must be 0 or 1", RouteIndex)
		}
		i, _ = strconv.Atoi(raw)
	} else if c.BlockingMiddleware != "" && len(routes) > 1 {
		if first, ok := routes[0].(map[string]interface{}); ok {
			mws, _, _ := unstructured.NestedSlice(first, "middlewares")
			for _, item := range mws {
				mw, _ := item.(map[string]interface{})
				if text(mw, "name") == c.BlockingMiddleware && (text(mw, "namespace") == "" || text(mw, "namespace") == u.GetNamespace()) {
					i = 1
				}
			}
		}
	}
	if i >= len(routes) {
		return t, fmt.Errorf("selected route does not exist")
	}
	route, ok := routes[i].(map[string]interface{})
	if !ok {
		return t, fmt.Errorf("invalid selected route")
	}
	match := text(route, "match")
	t.host, err = literalHost(match)
	if err != nil {
		return t, err
	}
	if explicit, err := annotation(u.GetAnnotations(), "hostname"); err != nil {
		return t, err
	} else if explicit != "" {
		t.host = explicit
	}
	if !validHost(t.host) {
		return t, fmt.Errorf("invalid gslb hostname")
	}
	matchedDomain := ""
	for _, domain := range Domains(c) {
		if strings.HasSuffix(t.host, "."+domain) && len(domain) > len(matchedDomain) {
			matchedDomain = domain
		}
	}
	if len(Domains(c)) > 0 && matchedDomain == "" {
		return t, fmt.Errorf("hostname must be below configured Domain; set gslb_hostname explicitly (no first-label inference)")
	}
	if c.LocalDomain != "" {
		prefix := t.host
		if matchedDomain != "" {
			prefix = strings.TrimSuffix(t.host, "."+matchedDomain)
		}
		t.localHost = prefix + "." + c.LocalDomain
		if !validHost(t.localHost) || t.localHost == t.host {
			return t, fmt.Errorf("invalid or colliding local hostname")
		}
	}
	services, _, _ := unstructured.NestedSlice(route, "services")
	if len(services) == 0 {
		return t, fmt.Errorf("selected route needs a first Service backend")
	}
	svc, ok := services[0].(map[string]interface{})
	if !ok || (text(svc, "kind") != "" && text(svc, "kind") != "Service") {
		return t, fmt.Errorf("first backend must be Service, not TraefikService")
	}
	t.service = text(svc, "name")
	if len(validation.IsDNS1035Label(t.service)) != 0 {
		return t, fmt.Errorf("invalid Service name")
	}
	if ns := text(svc, "namespace"); ns != "" && ns != u.GetNamespace() {
		return t, fmt.Errorf("cross-namespace backends are not supported")
	}
	switch p := svc["port"].(type) {
	case int64:
		t.port = p
	case float64:
		if p == math.Trunc(p) {
			t.port = int64(p)
		}
	default:
		return t, fmt.Errorf("first Service backend needs a numeric port")
	}
	if t.port < 1 || t.port > 65535 {
		return t, fmt.Errorf("Service port must be 1..65535")
	}
	return t, nil
}

func text(m map[string]interface{}, key string) string { s, _ := m[key].(string); return s }

func validatePolicy(p v1.Policy, c Config) error {
	if p.Strategy != "roundRobin" && p.Strategy != "failover" && p.Strategy != "geoip" {
		return fmt.Errorf("unsupported GSLB strategy %q", p.Strategy)
	}
	allowed := func(tag string) bool {
		return identifier.MatchString(tag) && (len(c.AllowedGeoTags) == 0 || slices.Contains(c.AllowedGeoTags, tag))
	}
	if p.PrimaryGeoTag != "" && !allowed(p.PrimaryGeoTag) {
		return fmt.Errorf("invalid or disallowed primary geo tag")
	}
	if p.Strategy == "failover" && p.PrimaryGeoTag == "" {
		return fmt.Errorf("failover requires a primary geo tag")
	}
	if p.DNSTTLSeconds < 0 || p.DNSTTLSeconds > 2147483647 {
		return fmt.Errorf("DNS TTL out of range")
	}
	var total int64
	if len(p.Weights) > 128 {
		return fmt.Errorf("too many geo weights")
	}
	for geo, weight := range p.Weights {
		if !allowed(geo) || weight < 0 || weight > 100 {
			return fmt.Errorf("invalid geo weight (expected 0..100)")
		}
		total += weight
	}
	if len(p.Weights) > 0 && (p.Strategy != "roundRobin" || total == 0) {
		return fmt.Errorf("positive weights require roundRobin")
	}
	return nil
}

func settings(u *unstructured.Unstructured, c Config) (routeSettings, error) {
	s := routeSettings{policy: v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 60}}
	if u.GetKind() != "IngressRoute" {
		return s, fmt.Errorf("expected IngressRoute")
	}
	if _, err := routeGVR(u.GetAPIVersion()); err != nil {
		return s, err
	}
	a := u.GetAnnotations()
	var err error
	if s.excluded, err = siteExcluded(a); err != nil {
		return s, err
	}
	for key := range aliases {
		if _, err := annotation(a, key); err != nil {
			return s, err
		}
	}
	if s.enabled, err = flag(a, "gslb"); err != nil {
		return s, err
	}
	if s.options.MTLS, err = flag(a, "mtls"); err != nil {
		return s, err
	}
	if s.options.Local, err = flag(a, "local"); err != nil {
		return s, err
	}
	if s.options.Entrypoint, err = annotation(a, "entrypoint"); err != nil {
		return s, err
	}
	s.options.MTLSEntrypoint, _ = annotation(a, "mtls_entrypoint")
	if s.options.MTLS && s.options.MTLSEntrypoint == "" {
		s.options.MTLSEntrypoint = c.MTLSEntrypoint
	}
	option, _ := annotation(a, "mtls_tlsoption")
	if option == "" && s.options.MTLS {
		option = c.MTLSTLSOption
	}
	if option != "" {
		ref, err := tlsOptionReference(option, u.GetNamespace(), c)
		if err != nil {
			return s, err
		}
		s.options.TLSOption = &ref
	}
	cn, _ := annotation(a, "mtls_cn_check")
	if cn != "" {
		if len(cn) > 4096 {
			return s, fmt.Errorf("DNS SAN list too large")
		}
		seen := map[string]bool{}
		for _, name := range strings.Split(cn, ",") {
			name = strings.TrimSpace(name)
			if name == "" || len(name) > 253 || strings.ContainsAny(name, "\n\r\t") || seen[name] {
				return s, fmt.Errorf("invalid or repeated DNS SAN pattern")
			}
			if !sanPattern.MatchString(name) {
				return s, fmt.Errorf("DNS SAN patterns support only letters, digits, dots, hyphens and wildcards")
			}
			seen[name] = true
			s.options.CommonNames = append(s.options.CommonNames, name)
		}
		if !s.options.MTLS || c.CNPlugin == "" {
			return s, fmt.Errorf("DNS SAN filtering requires mTLS and an explicitly configured certauthz plugin alias")
		}
	}
	if !s.enabled {
		return s, nil
	}
	if s.options.MTLS && (s.options.TLSOption == nil || !slices.Contains(MTLSEntrypoints(c), s.options.MTLSEntrypoint)) {
		return s, fmt.Errorf("mTLS requires an allowed entrypoint and an explicit existing TLSOption")
	}
	if s.options.Local && !s.options.MTLS && c.LocalDomain == "" {
		return s, fmt.Errorf("local children require configured LocalDomain")
	}
	if s.options.Entrypoint != "" {
		if err := validEntrypoints(strings.Split(s.options.Entrypoint, ","), c); err != nil {
			return s, err
		}
	}
	s.target, err = selectTarget(u, c)
	if err != nil {
		return s, err
	}
	s.options.Hostname = s.target.host
	s.policy.Strategy, _ = annotation(a, "strategy")
	if s.policy.Strategy == "" {
		s.policy.Strategy = "roundRobin"
	}
	s.policy.PrimaryGeoTag, _ = annotation(a, "geotag")
	ttl, _ := annotation(a, "dnsttl")
	s.policy.DNSTTLSeconds = 60
	if ttl != "" {
		s.policy.DNSTTLSeconds, err = strconv.ParseInt(ttl, 10, 64)
		if err != nil {
			return s, fmt.Errorf("invalid DNS TTL")
		}
	}
	weighted, err := flag(a, "weight_enable")
	if err != nil {
		return s, err
	}
	w, _ := annotation(a, "weights")
	if weighted {
		if len(w) > 8192 || w == "" {
			return s, fmt.Errorf("weights are required and bounded")
		}
		if strings.HasPrefix(w, "{") {
			if err := json.Unmarshal([]byte(w), &s.policy.Weights); err != nil {
				return s, fmt.Errorf("invalid weights JSON")
			}
		} else {
			s.policy.Weights = map[string]int64{}
			for _, pair := range strings.Split(w, ",") {
				parts := strings.FieldsFunc(strings.TrimSpace(pair), func(r rune) bool { return r == ':' || r == '=' })
				if len(parts) != 2 {
					return s, fmt.Errorf("weights must be JSON or geo=weight CSV")
				}
				geo := strings.TrimSpace(parts[0])
				value, e := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
				if _, exists := s.policy.Weights[geo]; exists || e != nil {
					return s, fmt.Errorf("invalid or repeated geo weight")
				}
				s.policy.Weights[geo] = value
			}
		}
		if len(s.policy.Weights) == 0 {
			return s, fmt.Errorf("weights cannot be empty")
		}
	}
	return s, validatePolicy(s.policy, c)
}

func reference(u *unstructured.Unstructured) v1.ResourceReference {
	return v1.ResourceReference{APIVersion: u.GetAPIVersion(), Kind: u.GetKind(), Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()), ResourceVersion: u.GetResourceVersion()}
}

func Candidates(resource unstructured.Unstructured, cfg Config) v1.Candidate {
	c := v1.Candidate{Resource: reference(&resource), Provider: "traefik", Hostnames: []string{}}
	if err := ValidateConfig(cfg); err != nil {
		c.Reason = err.Error()
		return c
	}
	if !namespaceAllowed(resource.GetNamespace(), cfg) {
		c.Reason = "namespace is not allowed"
		return c
	}
	s, err := settings(&resource, defaults(cfg))
	c.Enabled = s.enabled
	if err == nil && !s.enabled {
		// Creation chooses an explicit published hostname later; a local route Host
		// need not already be inside the adapter's global DNS zone.
		creation := defaults(cfg)
		creation.Domain, creation.Domains, creation.LocalDomain = "", nil, ""
		s.target, err = selectTarget(&resource, creation)
	}
	if err != nil {
		c.Reason = err.Error()
		return c
	}
	c.Eligible = true
	c.Hostnames = []string{s.target.host}
	s.options.Hostname = s.target.host
	c.Policy, c.Traefik = &s.policy, &s.options
	c.Traffic = &v1.TrafficStatus{Excluded: s.excluded, IPs: []string{}, Hostnames: []string{}}
	return c
}

// Candidate is the singular spelling used by catalog collectors.
func Candidate(resource unstructured.Unstructured, cfg Config) v1.Candidate {
	return Candidates(resource, cfg)
}
