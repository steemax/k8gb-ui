package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "k8gb-ui/api/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

type Server struct {
	cfg    Config
	agents map[string]Agent
	auth   *authenticator
	assets http.Handler
	demo   bool
}

func New(ctx context.Context, cfg Config, assets fs.FS, agents map[string]Agent, demo bool) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	auth, err := newAuthenticator(ctx, cfg.Auth)
	if err != nil {
		return nil, err
	}
	if agents == nil {
		agents = map[string]Agent{}
		for _, cluster := range cfg.Clusters {
			agent, err := newRemoteAgent(cluster)
			if err != nil {
				return nil, fmt.Errorf("initialize cluster %s: %w", cluster.ID, err)
			}
			agents[cluster.ID] = agent
		}
	}
	for _, cluster := range cfg.Clusters {
		if agents[cluster.ID] == nil {
			return nil, fmt.Errorf("missing client for cluster %s", cluster.ID)
		}
	}
	return &Server{cfg: cfg, agents: agents, auth: auth, assets: http.FileServer(http.FS(assets)), demo: demo}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/healthz":
		if r.Method != http.MethodGet {
			s.methodNotAllowed(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "apiVersion": v1.Version})
		return
	case "/auth/login":
		s.auth.Login(w, r)
		return
	case "/auth/callback":
		s.auth.Callback(w, r)
		return
	case "/auth/logout":
		s.auth.Logout(w, r)
		return
	case "/static/k8gb.css", "/static/k8gb-theme.js":
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.assets.ServeHTTP(w, r)
			return
		}
	}
	mutationRoute := r.URL.Path == "/api/v1/mutations/plan" || r.URL.Path == "/api/v1/mutations/apply"
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(mutationRoute && r.Method == http.MethodPost) {
		s.methodNotAllowed(w)
		return
	}
	p, ok := s.auth.Principal(r)
	if !ok {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, v1.Error{Error: "Authentication required"})
		} else {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
		}
		return
	}
	if mutationRoute {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeJSON(w, 405, v1.Error{Error: "POST required"})
			return
		}
		s.mutate(w, r, p)
		return
	}
	switch r.URL.Path {
	case "/api/v1/clusters":
		clusters := []v1.Cluster{}
		for _, c := range s.cfg.Clusters {
			if s.cfg.Allowed(p, c.ID, "") {
				clusters = append(clusters, c.Public())
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"apiVersion": v1.Version, "clusters": clusters, "readOnly": !s.hasWriteAccess(p), "demo": s.demo, "authMode": s.cfg.Auth.Mode})
	case "/api/v1/namespaces":
		s.namespaces(w, r, p)
	case "/api/v1/overview":
		s.overview(w, r, p)
	case "/api/v1/session":
		writeJSON(w, http.StatusOK, map[string]any{"subject": p.Subject, "mode": s.cfg.Auth.Mode, "readOnly": !s.hasWriteAccess(p)})
	case "/api/v1/catalog":
		s.catalog(w, r, p)
	case "/api/v1/audit":
		s.audit(w, r, p)
	case "/":
		s.assets.ServeHTTP(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/static/") {
			s.assets.ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
	}
}

func (s *Server) methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, http.StatusMethodNotAllowed, v1.Error{Error: "This build is read-only"})
}

func (s *Server) scoped(p Principal, namespace string) []ClusterConfig {
	result := []ClusterConfig{}
	for _, c := range s.cfg.Clusters {
		if s.cfg.Allowed(p, c.ID, namespace) && (namespace == "" || c.AllowsNamespace(namespace)) {
			result = append(result, c)
		}
	}
	return result
}

// Fan-out is bounded independently of the number of configured clusters.
func eachCluster(ctx context.Context, clusters []ClusterConfig, fn func(context.Context, ClusterConfig, int)) {
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, c := range clusters {
		wg.Add(1)
		go func(index int, cluster ClusterConfig) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				fn(ctx, cluster, index)
				return
			}
			child, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			fn(child, cluster, index)
		}(i, c)
	}
	wg.Wait()
}

func (s *Server) namespaces(w http.ResponseWriter, r *http.Request, p Principal) {
	clusters := s.scoped(p, "")
	sets := make([][]string, len(clusters))
	failures := make([]string, len(clusters))
	eachCluster(r.Context(), clusters, func(ctx context.Context, c ClusterConfig, i int) {
		names, err := s.agents[c.ID].Namespaces(ctx)
		if err != nil {
			failures[i] = c.ID + ": namespace list unavailable"
			slog.Warn("agent namespace list unavailable", "cluster", c.ID, "error", err)
			return
		}
		for _, name := range names {
			if len(validation.IsDNS1123Label(name)) == 0 && c.AllowsNamespace(name) && s.cfg.Allowed(p, c.ID, name) {
				sets[i] = append(sets[i], name)
			}
		}
	})
	set := map[string]bool{}
	warnings := []string{}
	for _, names := range sets {
		for _, name := range names {
			set[name] = true
		}
	}
	for _, failure := range failures {
		if failure != "" {
			warnings = append(warnings, failure)
		}
	}
	names := []string{}
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{"apiVersion": v1.Version, "namespaces": names, "warnings": warnings})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, p Principal) {
	values := r.URL.Query()["namespace"]
	if len(values) != 1 || len(validation.IsDNS1123Label(values[0])) != 0 {
		writeJSON(w, http.StatusBadRequest, v1.Error{Error: "One valid namespace is required"})
		return
	}
	namespace := values[0]
	clusters := s.scoped(p, namespace)
	if len(clusters) == 0 {
		writeJSON(w, http.StatusForbidden, v1.Error{Error: "Namespace is outside your configured scope"})
		return
	}
	sites := make([]v1.Site, len(clusters))
	snapshots := make([]v1.Snapshot, len(clusters))
	eachCluster(r.Context(), clusters, func(ctx context.Context, c ClusterConfig, i int) {
		sites[i] = v1.Site{Cluster: c.Public(), Warnings: []string{}}
		snapshot, err := s.agents[c.ID].Snapshot(ctx, namespace)
		if err != nil {
			sites[i].Error = "Cluster snapshot unavailable"
			slog.Warn("agent snapshot unavailable", "cluster", c.ID, "namespace", namespace, "error", err)
			return
		}
		sites[i].Available = true
		sites[i].ObjectsComplete = snapshot.ObjectsComplete
		sites[i].CollectedAt = snapshot.CollectedAt
		sites[i].Warnings = snapshot.Warnings
		sites[i].Capabilities = s.capabilities(p, c.ID, namespace, snapshot.Capabilities)
		snapshots[i] = snapshot
	})
	result := Aggregate(namespace, sites, snapshots)
	result.Capabilities = mergeCapabilities(sites)
	writeJSON(w, http.StatusOK, result)
}
