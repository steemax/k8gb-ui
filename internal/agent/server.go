package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"k8gb-ui/internal/auditlog"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
)

type ServerOptions struct {
	Token                 string
	InsecureLocal         bool
	ListenAddress         string
	MaxConcurrentRequests int
	Mutations             *mutation.Manager
	WriteToken            string
	Audit                 auditlog.Journal
	TraefikConfig         *traefik.Config
}

// LoadToken accepts a single printable bearer token, with an optional trailing
// file newline. It never returns token contents in an error.
func LoadToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", errors.New("token file is too large")
	}
	token := strings.TrimRight(string(data), "\r\n")
	if err := validateToken(token); err != nil {
		return "", err
	}
	return token, nil
}

func validateToken(token string) error {
	if len(token) < 32 || len(token) > 4096 {
		return errors.New("bearer token must contain 32 to 4096 characters")
	}
	for _, ch := range token {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-._~+/=", ch)) {
			return errors.New("bearer token contains invalid characters")
		}
	}
	return nil
}

func ValidateServerOptions(opts ServerOptions) error {
	host, _, err := net.SplitHostPort(opts.ListenAddress)
	if err != nil {
		return errors.New("listen address must contain a host and port")
	}
	if opts.InsecureLocal {
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return errors.New("insecure-local requires a literal loopback listen address")
		}
		if opts.Token != "" {
			return errors.New("insecure-local and a bearer token are mutually exclusive")
		}
		return nil
	}
	return validateToken(opts.Token)
}

func NewHandler(collector *Collector, opts ServerOptions) (http.Handler, error) {
	if collector == nil {
		return nil, errors.New("collector is required")
	}
	if err := ValidateServerOptions(opts); err != nil {
		return nil, err
	}
	if opts.MaxConcurrentRequests == 0 {
		opts.MaxConcurrentRequests = 8
	}
	if opts.MaxConcurrentRequests < 1 || opts.MaxConcurrentRequests > 128 {
		return nil, errors.New("max concurrent requests must be between 1 and 128")
	}
	semaphore := make(chan struct{}, opts.MaxConcurrentRequests)
	expected := sha256.Sum256([]byte(opts.Token))
	writeExpected := sha256.Sum256([]byte(opts.WriteToken))
	if opts.Mutations != nil && !opts.Mutations.Capabilities().ReadOnly {
		if err := validateToken(opts.WriteToken); err != nil {
			return nil, err
		}
		if opts.WriteToken == opts.Token {
			return nil, errors.New("write token must differ from read token")
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mutating := r.URL.Path == "/api/v1/mutations/plan" || r.URL.Path == "/api/v1/mutations/apply"
		if r.Method != http.MethodGet && !(mutating && r.Method == http.MethodPost) {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, v1.Error{Error: "only GET is supported"})
			return
		}
		if r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if !opts.InsecureLocal || mutating {
			headers := r.Header.Values("Authorization")
			token := ""
			if len(headers) == 1 && len(headers[0]) >= 7 && strings.EqualFold(headers[0][:7], "Bearer ") {
				token = headers[0][7:]
			}
			provided := sha256.Sum256([]byte(token))
			want := expected
			if mutating {
				want = writeExpected
			}
			if token == "" || (mutating && opts.WriteToken == "") || subtle.ConstantTimeCompare(want[:], provided[:]) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeJSON(w, http.StatusUnauthorized, v1.Error{Error: "valid bearer token required"})
				return
			}
		}
		if r.URL.Path != "/api/v1/namespaces" && r.URL.Path != "/api/v1/snapshot" && r.URL.Path != "/api/v1/candidates" && r.URL.Path != "/api/v1/audit" && !mutating {
			writeJSON(w, http.StatusNotFound, v1.Error{Error: "not found"})
			return
		}
		if mutating {
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSON(w, 405, v1.Error{Error: "POST required"})
				return
			}
			serveMutation(w, r, opts.Mutations)
			return
		}
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		default:
			writeJSON(w, http.StatusServiceUnavailable, v1.Error{Error: "agent is busy"})
			return
		}
		if r.URL.Path == "/api/v1/namespaces" {
			result, err := collector.Namespaces(r.Context())
			if err != nil {
				writeCollectionError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		query, err := parseNamespace(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, v1.Error{Error: err.Error()})
			return
		}
		if err := collector.validateNamespace(query); err != nil {
			writeCollectionError(w, err)
			return
		}
		caps := v1.Capabilities{ReadOnly: true, Providers: []string{"native"}}
		if opts.Mutations != nil {
			caps = opts.Mutations.Capabilities()
		}
		if r.URL.Path == "/api/v1/audit" {
			result := v1.AgentAudit{Events: []v1.AuditEvent{}}
			if opts.Audit != nil {
				result, err = opts.Audit.ReadAudit(r.Context(), query)
				if err != nil {
					writeJSON(w, http.StatusServiceUnavailable, v1.Error{Error: "Recent audit storage unavailable"})
					return
				}
			}
			result.APIVersion, result.ClusterID, result.Namespace = v1.Version, collector.opts.ClusterID, query
			writeJSON(w, 200, result)
			return
		}
		if r.URL.Path == "/api/v1/candidates" {
			catalog, err := collector.Catalog(r.Context(), query, opts.TraefikConfig, caps)
			if err != nil {
				writeCollectionError(w, err)
			} else {
				writeJSON(w, 200, catalog)
			}
			return
		}
		result, err := collector.Snapshot(r.Context(), query)
		if err != nil {
			writeCollectionError(w, err)
			return
		}
		result.Capabilities = caps
		writeJSON(w, http.StatusOK, result)
	}), nil
}

func serveMutation(w http.ResponseWriter, r *http.Request, manager *mutation.Manager) {
	if manager == nil {
		writeJSON(w, 503, v1.MutationError{Code: "writes_disabled", Error: "Mutation API is disabled"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var result any
	var err error
	if strings.HasSuffix(r.URL.Path, "/plan") {
		var request v1.MutationRequest
		err = decoder.Decode(&request)
		if err == nil {
			if tail := decoder.Decode(new(any)); tail != io.EOF {
				err = errors.New("one JSON object required")
			} else {
				result, err = manager.Plan(r.Context(), request)
			}
		}
	} else {
		var plan v1.MutationPlan
		err = decoder.Decode(&plan)
		if err == nil {
			if tail := decoder.Decode(new(any)); tail != io.EOF {
				err = errors.New("one JSON object required")
			} else {
				result, err = manager.Apply(r.Context(), plan)
			}
		}
	}
	if err != nil {
		var problem *mutation.Error
		if errors.As(err, &problem) {
			writeJSON(w, problem.Status, problem.Public())
		} else {
			writeJSON(w, 400, v1.MutationError{Code: "invalid_request", Error: "Invalid mutation request"})
		}
		return
	}
	writeJSON(w, 200, result)
}

func parseNamespace(r *http.Request) (string, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(values["namespace"]) != 1 || values.Get("namespace") == "" {
		return "", ErrInvalidNamespace
	}
	return values.Get("namespace"), nil
}

func writeCollectionError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	message := "Kubernetes collection failed"
	switch {
	case errors.Is(err, ErrPrimaryAPIUnavailable):
		status, message = http.StatusServiceUnavailable, err.Error()
	case errors.Is(err, ErrInvalidNamespace):
		status, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrNamespaceDenied):
		status, message = http.StatusForbidden, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		status, message = http.StatusGatewayTimeout, "Kubernetes collection timed out"
	}
	writeJSON(w, status, v1.Error{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func NewHTTPServer(address string, handler http.Handler, collectionTimeout time.Duration) *http.Server {
	return &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: collectionTimeout + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
}
