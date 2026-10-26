package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *Server) hasWriteAccess(p Principal) bool {
	if s.demo || !s.cfg.WritesEnabled {
		return false
	}
	if s.cfg.Auth.Mode == "local" {
		return s.cfg.Auth.LocalAdmin
	}
	for _, binding := range s.cfg.Auth.Bindings {
		if binding.Role != "admin" {
			continue
		}
		if contains(binding.Subjects, p.Subject) {
			return true
		}
		for _, g := range p.Groups {
			if contains(binding.Groups, g) {
				return true
			}
		}
	}
	return false
}
func (s *Server) capabilities(p Principal, cluster, namespace string, caps v1.Capabilities) v1.Capabilities {
	if s.demo || !s.cfg.CanWrite(p, cluster, namespace) {
		caps.ReadOnly = true
		caps.Operations = nil
	}
	return caps
}
func mergeCapabilities(sites []v1.Site) v1.Capabilities {
	result := v1.Capabilities{ReadOnly: true, Providers: []string{"native"}, Operations: map[string][]string{}}
	for _, site := range sites {
		if site.Capabilities.ReadOnly || !site.Available {
			continue
		}
		for provider, operations := range site.Capabilities.Operations {
			for _, op := range operations {
				if !contains(result.Operations[provider], op) {
					result.Operations[provider] = append(result.Operations[provider], op)
				}
			}
		}
	}
	for provider, ops := range result.Operations {
		if len(ops) > 0 {
			result.ReadOnly = false
			if !contains(result.Providers, provider) {
				result.Providers = append(result.Providers, provider)
			}
		}
	}
	return result
}
func (s *Server) safeMutationOrigin(r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.Header.Get("X-K8gb-Intent") != "mutation" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	expected := "http://" + s.cfg.Listen
	if s.cfg.Auth.Mode == "oidc" || s.cfg.Auth.Mode == "builtin" {
		return s.auth.sameOrigin(r)
	}
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == expected
}
func readMutationJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("invalid mutation JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("only one JSON object is allowed")
	}
	return nil
}
func publicMutationError(err error) *v1.MutationError {
	var problem *mutation.Error
	if errors.As(err, &problem) {
		value := problem.Public()
		return &value
	}
	return &v1.MutationError{Code: "agent_error", Error: "Agent mutation request failed"}
}
func (s *Server) mutationAgent(p Principal, cluster, namespace string) (MutationAgent, error) {
	if !s.cfg.CanWrite(p, cluster, namespace) || s.demo {
		return nil, mutation.Failure(403, "write_forbidden", "Administrator write access and explicit write configuration are required")
	}
	a, ok := s.agents[cluster].(MutationAgent)
	if !ok {
		return nil, mutation.Failure(503, "unsupported_agent", "Agent does not support mutation API")
	}
	return a, nil
}
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, p Principal) {
	if !s.hasWriteAccess(p) {
		writeJSON(w, 403, v1.MutationError{Code: "write_forbidden", Error: "Administrator write access is required"})
		return
	}
	if !s.safeMutationOrigin(r) {
		writeJSON(w, 403, v1.MutationError{Code: "invalid_origin", Error: "Same-origin JSON mutation request required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	if strings.HasSuffix(r.URL.Path, "/plan") {
		var input v1.BatchPlanRequest
		if err := readMutationJSON(w, r, &input); err != nil {
			writeJSON(w, 400, v1.Error{Error: err.Error()})
			return
		}
		if len(input.Targets) == 0 || len(input.Targets) > 32 {
			writeJSON(w, 400, v1.Error{Error: "Select between 1 and 32 targets"})
			return
		}
		clients := make([]MutationAgent, len(input.Targets))
		seen := map[string]bool{}
		for i, target := range input.Targets {
			ref := target.Request.Resource
			if len(validation.IsDNS1123Label(ref.Namespace)) != 0 || strings.TrimSpace(target.Request.Reason) == "" || len(target.Request.Reason) > 1024 || strings.IndexFunc(target.Request.Reason, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
				writeJSON(w, 400, v1.Error{Error: "A valid namespace and non-empty reason (up to 1024 bytes) are required"})
				return
			}
			key := target.ClusterID + "/" + ref.APIVersion + "/" + ref.Namespace + "/" + ref.Kind + "/" + ref.Name
			if seen[key] {
				writeJSON(w, 400, v1.Error{Error: "Duplicate mutation target"})
				return
			}
			seen[key] = true
			a, err := s.mutationAgent(p, target.ClusterID, ref.Namespace)
			if err != nil {
				writeJSON(w, 403, publicMutationError(err))
				return
			}
			clients[i] = a
			input.Targets[i].Request.Actor = p.Subject
		}
		result := v1.BatchPlan{APIVersion: v1.Version, Targets: make([]v1.TargetPlan, len(input.Targets)), CanApply: true}
		// Keep this deterministic and bounded. A plan never writes live resources.
		for i, target := range input.Targets {
			plan, err := clients[i].Plan(ctx, target.Request)
			result.Targets[i].ClusterID = target.ClusterID
			if err != nil {
				result.Targets[i].Error = publicMutationError(err)
				result.CanApply = false
			} else {
				result.Targets[i].Plan = &plan
			}
		}
		writeJSON(w, 200, result)
		return
	}
	var input v1.BatchApplyRequest
	if err := readMutationJSON(w, r, &input); err != nil {
		writeJSON(w, 400, v1.Error{Error: err.Error()})
		return
	}
	if len(input.Targets) == 0 || len(input.Targets) > 32 {
		writeJSON(w, 400, v1.Error{Error: "Select between 1 and 32 plans"})
		return
	}
	clients := make([]MutationAgent, len(input.Targets))
	seen := map[string]bool{}
	for i, target := range input.Targets {
		if target.Error != nil || target.Plan == nil {
			if !input.AllowPartial {
				writeJSON(w, 409, v1.Error{Error: "Partial plan requires explicit consent"})
				return
			}
			continue
		}
		plan := target.Plan
		ref := plan.Request.Resource
		if plan.ClusterID != target.ClusterID || plan.APIVersion != v1.Version || plan.Request.Actor != p.Subject || !time.Now().Before(plan.ExpiresAt) || plan.Token == "" || len(plan.Digest) != 64 {
			writeJSON(w, 409, v1.Error{Error: "Plan identity is invalid or expired; prepare a new plan"})
			return
		}
		key := target.ClusterID + "/" + ref.APIVersion + "/" + ref.Namespace + "/" + ref.Kind + "/" + ref.Name
		if seen[key] {
			writeJSON(w, 400, v1.Error{Error: "Duplicate mutation target"})
			return
		}
		seen[key] = true
		a, err := s.mutationAgent(p, target.ClusterID, ref.Namespace)
		if err != nil {
			writeJSON(w, 403, publicMutationError(err))
			return
		}
		clients[i] = a
	}
	result := v1.BatchResult{APIVersion: v1.Version, Targets: make([]v1.TargetResult, len(input.Targets))}
	for i, target := range input.Targets {
		result.Targets[i].ClusterID = target.ClusterID
		if clients[i] == nil {
			result.Targets[i].Error = target.Error
			continue
		}
		applied, err := clients[i].Apply(ctx, *target.Plan)
		if err != nil {
			result.Targets[i].Error = publicMutationError(err)
		} else {
			result.Targets[i].Result = &applied
		}
	}
	writeJSON(w, 200, result)
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request, p Principal) {
	values := r.URL.Query()["namespace"]
	if len(values) != 1 || len(validation.IsDNS1123Label(values[0])) != 0 {
		writeJSON(w, 400, v1.Error{Error: "One valid namespace is required"})
		return
	}
	namespace := values[0]
	clusters := s.scoped(p, namespace)
	if len(clusters) == 0 {
		writeJSON(w, 403, v1.Error{Error: "Namespace is outside configured scope"})
		return
	}
	result := v1.CatalogOverview{APIVersion: v1.Version, Namespace: namespace, Sites: make([]v1.CatalogSite, len(clusters))}
	eachCluster(r.Context(), clusters, func(ctx context.Context, c ClusterConfig, i int) {
		site := v1.CatalogSite{ClusterID: c.ID}
		a, ok := s.agents[c.ID].(MutationAgent)
		if !ok {
			site.Error = "Agent catalog is not supported"
		} else {
			catalog, err := a.Catalog(ctx, namespace)
			if err != nil {
				site.Error = "Agent catalog unavailable"
			} else {
				catalog.Capabilities = s.capabilities(p, c.ID, namespace, catalog.Capabilities)
				site.Catalog = &catalog
			}
		}
		result.Sites[i] = site
	})
	writeJSON(w, 200, result)
}
func (s *Server) audit(w http.ResponseWriter, r *http.Request, p Principal) {
	values := r.URL.Query()["namespace"]
	if len(values) != 1 || len(validation.IsDNS1123Label(values[0])) != 0 {
		writeJSON(w, 400, v1.Error{Error: "One valid namespace is required"})
		return
	}
	namespace := values[0]
	clusters := s.scoped(p, namespace)
	if len(clusters) == 0 {
		writeJSON(w, 403, v1.Error{Error: "Namespace is outside configured scope"})
		return
	}
	writeJSON(w, 200, s.collectAudit(r.Context(), namespace, clusters))
}
