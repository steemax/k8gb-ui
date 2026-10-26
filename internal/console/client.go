package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"k8gb-ui/internal/mutation"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
)

type Agent interface {
	Namespaces(context.Context) ([]string, error)
	Snapshot(context.Context, string) (v1.Snapshot, error)
}

type remoteAgent struct {
	cluster    ClusterConfig
	client     *http.Client
	token      string
	writeToken string
}

func newRemoteAgent(c ClusterConfig) (Agent, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		tlsConfig.RootCAs = pool
	}
	if c.ClientCertFile != "" {
		cert, err := tls.LoadX509KeyPair(c.ClientCertFile, c.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	token := ""
	if c.BearerTokenFile != "" {
		raw, err := os.ReadFile(c.BearerTokenFile)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(raw))
		if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
			return nil, fmt.Errorf("agent token must contain 32..4096 non-whitespace bytes")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.MaxConnsPerHost = 4
	transport.ResponseHeaderTimeout = 30 * time.Second
	writeToken := ""
	if c.WriteTokenFile != "" {
		raw, err := os.ReadFile(c.WriteTokenFile)
		if err != nil {
			return nil, err
		}
		writeToken = strings.TrimSpace(string(raw))
		if len(writeToken) < 32 || len(writeToken) > 4096 || strings.ContainsAny(writeToken, " \t\r\n") || writeToken == token {
			return nil, fmt.Errorf("write token must be distinct from read token and contain 32..4096 non-whitespace bytes")
		}
	}
	return &remoteAgent{cluster: c, token: token, writeToken: writeToken, client: &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type MutationAgent interface {
	Plan(context.Context, v1.MutationRequest) (v1.MutationPlan, error)
	Apply(context.Context, v1.MutationPlan) (v1.MutationResult, error)
	Catalog(context.Context, string) (v1.Catalog, error)
	Audit(context.Context, string) ([]v1.AuditEvent, error)
}

func (a *remoteAgent) post(ctx context.Context, path string, payload, out any, applying bool) error {
	if a.writeToken == "" {
		return mutation.Failure(503, "writes_disabled", "Agent write credentials are not configured")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(a.cluster.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.writeToken)
	client := *a.client
	client.Timeout = 35 * time.Second
	response, err := client.Do(req)
	if err != nil {
		return &mutation.Error{Status: 502, Code: "agent_unreachable", Message: "Agent request failed (network or TLS)", OutcomeUnknown: applying}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(body) > 2<<20 {
		return &mutation.Error{Status: 502, Code: "invalid_agent_response", Message: "Agent response is incomplete or too large", OutcomeUnknown: applying}
	}
	if response.StatusCode != 200 {
		var problem v1.MutationError
		if json.Unmarshal(body, &problem) != nil || problem.Error == "" {
			problem = v1.MutationError{Code: "agent_error", Error: fmt.Sprintf("Agent returned HTTP %d", response.StatusCode), OutcomeUnknown: applying && response.StatusCode >= 500}
		}
		return &mutation.Error{Status: response.StatusCode, Code: problem.Code, Message: problem.Error, OutcomeUnknown: problem.OutcomeUnknown}
	}
	if json.Unmarshal(body, out) != nil {
		return &mutation.Error{Status: 502, Code: "invalid_agent_response", Message: "Agent returned invalid JSON", OutcomeUnknown: applying}
	}
	return nil
}

func (a *remoteAgent) Plan(ctx context.Context, request v1.MutationRequest) (v1.MutationPlan, error) {
	var plan v1.MutationPlan
	if err := a.post(ctx, "/api/v1/mutations/plan", request, &plan, false); err != nil {
		return plan, err
	}
	if plan.ClusterID != a.cluster.ID || plan.APIVersion != v1.Version || plan.Request.Actor != request.Actor || plan.Request.Resource != request.Resource {
		return plan, mutation.Failure(502, "invalid_agent_identity", "Agent returned a plan for a different identity or resource")
	}
	return plan, nil
}
func (a *remoteAgent) Apply(ctx context.Context, plan v1.MutationPlan) (v1.MutationResult, error) {
	var result v1.MutationResult
	if err := a.post(ctx, "/api/v1/mutations/apply", plan, &result, true); err != nil {
		return result, err
	}
	if result.ClusterID != a.cluster.ID || result.APIVersion != v1.Version || result.Digest != plan.Digest || result.Resource.Namespace != plan.Request.Resource.Namespace || result.Resource.Name != plan.Request.Resource.Name {
		return result, &mutation.Error{Status: 502, Code: "invalid_agent_identity", Message: "Agent returned an invalid result; inspect Kubernetes before retrying", OutcomeUnknown: true}
	}
	return result, nil
}
func (a *remoteAgent) Catalog(ctx context.Context, namespace string) (v1.Catalog, error) {
	var catalog v1.Catalog
	if err := a.get(ctx, "/api/v1/candidates?"+url.Values{"namespace": {namespace}}.Encode(), &catalog); err != nil {
		return catalog, err
	}
	if catalog.APIVersion != v1.Version || catalog.ClusterID != a.cluster.ID || catalog.Namespace != namespace {
		return catalog, fmt.Errorf("invalid catalog identity or namespace")
	}
	for _, candidate := range catalog.Candidates {
		if candidate.Resource.Namespace != namespace {
			return catalog, fmt.Errorf("out-of-scope candidate")
		}
	}
	return catalog, nil
}
func (a *remoteAgent) Audit(ctx context.Context, namespace string) ([]v1.AuditEvent, error) {
	result, err := a.ReadAudit(ctx, namespace)
	return result.Events, err
}

func (a *remoteAgent) ReadAudit(ctx context.Context, namespace string) (v1.AgentAudit, error) {
	var result v1.AgentAudit
	if err := a.get(ctx, "/api/v1/audit?"+url.Values{"namespace": {namespace}}.Encode(), &result); err != nil {
		return result, err
	}
	if result.ClusterID != a.cluster.ID || result.APIVersion != v1.Version || result.Namespace != "" && result.Namespace != namespace {
		return result, fmt.Errorf("invalid audit identity")
	}
	for _, event := range result.Events {
		if event.Resource.Namespace != namespace || event.ClusterID != a.cluster.ID {
			return result, fmt.Errorf("out-of-scope audit event")
		}
	}
	return result, nil
}

func (a *remoteAgent) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(a.cluster.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("agent request failed (network or TLS)")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return fmt.Errorf("read agent response: %w", err)
	}
	if len(data) > 8<<20 {
		return fmt.Errorf("agent response exceeds 8 MiB")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid agent JSON response")
	}
	return nil
}

func (a *remoteAgent) Namespaces(ctx context.Context) ([]string, error) {
	var result v1.NamespaceList
	if err := a.get(ctx, "/api/v1/namespaces", &result); err != nil {
		return nil, err
	}
	if result.APIVersion != v1.Version || result.ClusterID != a.cluster.ID {
		return nil, fmt.Errorf("agent identity or API version mismatch")
	}
	return result.Namespaces, nil
}

func (a *remoteAgent) Snapshot(ctx context.Context, namespace string) (v1.Snapshot, error) {
	var result v1.Snapshot
	err := a.get(ctx, "/api/v1/snapshot?"+url.Values{"namespace": {namespace}}.Encode(), &result)
	if err != nil {
		return result, err
	}
	if result.APIVersion != v1.Version || result.ClusterID != a.cluster.ID || result.Namespace != namespace {
		return result, fmt.Errorf("agent identity, namespace or API version mismatch")
	}
	if result.CollectedAt.IsZero() || result.CollectedAt.After(time.Now().Add(time.Minute)) {
		return result, fmt.Errorf("agent snapshot timestamp is invalid")
	}
	for _, object := range result.Objects {
		if object.Resource.Namespace != namespace {
			return result, fmt.Errorf("agent returned an object outside requested namespace")
		}
	}
	return result, nil
}
