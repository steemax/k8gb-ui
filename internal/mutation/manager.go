package mutation

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/dnszones"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

const planLifetime = 5 * time.Minute

type Options struct {
	DNSZones  *dnszones.Reader
	ClusterID string
	Enabled   bool
	// SigningKey is a dedicated plan-signing secret, not an authentication secret.
	SigningKey []byte
	// An empty namespace allowlist permits all syntactically valid namespaces.
	AllowedNamespaces []string
	Providers         map[string]Provider
	Audit             func(v1.AuditEvent)
	Now               func() time.Time
}

type Manager struct {
	client dynamic.Interface
	opts   Options
	config string
	logger *slog.Logger
}

func NewManager(client dynamic.Interface, opts Options) (*Manager, error) {
	if client == nil || !boundedText(opts.ClusterID, 256) {
		return nil, Failure(500, "invalid_configuration", "client and bounded cluster ID are required")
	}
	if opts.Enabled && len(opts.SigningKey) < 32 || len(opts.SigningKey) != 0 && len(opts.SigningKey) < 32 {
		return nil, Failure(500, "invalid_configuration", "dedicated plan signing key must contain at least 32 bytes")
	}
	opts.SigningKey = append([]byte(nil), opts.SigningKey...)
	opts.AllowedNamespaces = append([]string(nil), opts.AllowedNamespaces...)
	sort.Strings(opts.AllowedNamespaces)
	for _, ns := range opts.AllowedNamespaces {
		if len(validation.IsDNS1123Label(ns)) != 0 {
			return nil, Failure(500, "invalid_configuration", "invalid allowed namespace")
		}
	}
	providers := make(map[string]Provider, len(opts.Providers))
	operations := map[string][]string{}
	for name, provider := range opts.Providers {
		if !boundedText(name, 64) || provider == nil {
			return nil, Failure(500, "invalid_configuration", "invalid provider")
		}
		providers[name] = provider
		operations[name] = append([]string(nil), provider.Operations()...)
		sort.Strings(operations[name])
	}
	opts.Providers = providers
	if opts.Now == nil {
		opts.Now = time.Now
	}
	config, _ := json.Marshal(struct {
		Namespaces []string
		Operations map[string][]string
	}{opts.AllowedNamespaces, operations})
	return &Manager{client: client, opts: opts, config: string(config), logger: slog.New(slog.NewJSONHandler(os.Stdout, nil))}, nil
}

func (manager *Manager) Capabilities() v1.Capabilities {
	caps := v1.Capabilities{ReadOnly: !manager.opts.Enabled, Providers: []string{}, Operations: map[string][]string{}}
	if !manager.opts.Enabled {
		return caps
	}
	for name, provider := range manager.opts.Providers {
		caps.Providers = append(caps.Providers, name)
		caps.Operations[name] = append([]string(nil), provider.Operations()...)
		sort.Strings(caps.Operations[name])
	}
	sort.Strings(caps.Providers)
	return caps
}

func (manager *Manager) Plan(ctx context.Context, req v1.MutationRequest) (plan v1.MutationPlan, err error) {
	// Detach maps and pointers from caller-owned request memory.
	data, marshalErr := json.Marshal(req)
	if marshalErr != nil || len(data) > 64*1024 {
		return plan, Failure(422, "invalid_request", "request is invalid or too large")
	}
	req = v1.MutationRequest{}
	if err = json.Unmarshal(data, &req); err != nil {
		return plan, err
	}
	manager.audit(req, "plan_started", "", nil, false)
	defer func() {
		status := "planned"
		if err != nil {
			status = "plan_failed"
		}
		manager.audit(req, status, plan.Digest, plan.Changes, false)
	}()
	plan, _, _, err = manager.prepare(ctx, req, manager.opts.Now().UTC())
	return plan, err
}

func (manager *Manager) Apply(ctx context.Context, plan v1.MutationPlan) (result v1.MutationResult, err error) {
	data, marshalErr := json.Marshal(plan)
	if marshalErr != nil || len(data) > 1024*1024 {
		return result, Failure(422, "invalid_plan", "plan is invalid or too large")
	}
	plan = v1.MutationPlan{}
	if err = json.Unmarshal(data, &plan); err != nil {
		return result, err
	}
	result = v1.MutationResult{APIVersion: v1.Version, ClusterID: manager.opts.ClusterID, Resource: plan.Request.Resource, Digest: plan.Digest, Warnings: []string{}}
	manager.audit(plan.Request, "apply_started", plan.Digest, plan.Changes, false)
	defer func() {
		status, unknown := result.Status, false
		if status == "" {
			status = "apply_failed"
		}
		var failure *Error
		if errors.As(err, &failure) {
			unknown = failure.OutcomeUnknown
		}
		manager.audit(plan.Request, status, plan.Digest, plan.Changes, unknown)
	}()
	if err = manager.verify(plan); err != nil {
		return result, err
	}
	request, prepErr := manager.refreshGslbRequest(ctx, plan)
	if prepErr != nil {
		result.Status = "drift"
		return result, classifyError(prepErr, false)
	}
	fresh, prepared, admitted, prepErr := manager.prepare(ctx, request, plan.PlannedAt)
	if prepErr != nil {
		result.Status = "drift"
		return result, classifyError(prepErr, false)
	}
	if !reflect.DeepEqual(comparablePlan(fresh), comparablePlan(plan)) {
		result.Status = "drift"
		return result, Failure(409, "plan_drift", "live resources, admission, provider configuration or dependencies changed; plan again")
	}
	if err = manager.checkDependencies(ctx, fresh.Request.Resource.Namespace, fresh.Dependencies); err != nil {
		result.Status = "drift"
		return result, err
	}
	if err = manager.verify(plan); err != nil {
		return result, err
	}
	result.Warnings = append(result.Warnings, fresh.Warnings...)
	result.Reconciliation = "pending"
	if plan.Noop {
		result.Status = "noop"
		return result, nil
	}
	actual, writeErr := manager.write(ctx, prepared, false)
	if writeErr != nil {
		return result, classifyError(writeErr, true)
	}
	result.Status = "applied"
	if actual != nil {
		result.Resource = objectReference(actual)
	}
	if actual == nil || !matchesReference(actual, plan.Request.Resource, false) || prepared.Current != nil && actual.GetUID() != prepared.Current.GetUID() || !reflect.DeepEqual(normalize(actual), normalize(admitted)) {
		result.Status = "drift"
		result.Warnings = append(result.Warnings, "The write completed but its response differs from the admitted plan; inspect the resource before another operation.")
	}
	return result, nil
}

func (manager *Manager) validateRequest(req v1.MutationRequest) (Provider, error) {
	if !manager.opts.Enabled {
		return nil, Failure(503, "mutations_disabled", "mutations are disabled")
	}
	if !boundedText(req.Actor, 256) || !boundedText(req.Reason, 1024) {
		return nil, Failure(422, "invalid_request", "actor and reason must be nonblank bounded text without control characters")
	}
	if _, err := resourceGVR(req.Resource); err != nil {
		return nil, err
	}
	if !manager.namespaceAllowed(req.Resource.Namespace) {
		return nil, Failure(403, "namespace_forbidden", "namespace is not allowed")
	}
	if req.Target != nil {
		if _, err := resourceGVR(*req.Target); err != nil {
			return nil, err
		}
		if req.Target.Namespace != req.Resource.Namespace || !manager.namespaceAllowed(req.Target.Namespace) {
			return nil, Failure(403, "namespace_forbidden", "cross-namespace targets are forbidden")
		}
	}
	provider, found := manager.opts.Providers[req.Provider]
	if !found {
		return nil, Failure(422, "unsupported_provider", "provider is not configured")
	}
	for _, operation := range provider.Operations() {
		if operation == req.Operation {
			return provider, nil
		}
	}
	return nil, Unsupported(req.Provider, req.Operation)
}

func (manager *Manager) namespaceAllowed(namespace string) bool {
	if len(manager.opts.AllowedNamespaces) == 0 {
		return true
	}
	for _, allowed := range manager.opts.AllowedNamespaces {
		if namespace == allowed {
			return true
		}
	}
	return false
}

func (manager *Manager) prepare(ctx context.Context, req v1.MutationRequest, plannedAt time.Time) (v1.MutationPlan, Prepared, *unstructured.Unstructured, error) {
	plan := v1.MutationPlan{}
	provider, err := manager.validateRequest(req)
	if err != nil {
		return plan, Prepared{}, nil, err
	}
	prepared, err := provider.Prepare(ctx, req)
	if err != nil {
		return plan, prepared, nil, classifyError(err, false)
	}
	if err := manager.creationZone(ctx, req, &prepared); err != nil {
		return plan, prepared, nil, err
	}
	if err := manager.validatePrepared(ctx, req, &prepared); err != nil {
		return plan, prepared, nil, err
	}
	if err := manager.checkDependencies(ctx, req.Resource.Namespace, prepared.Dependencies); err != nil {
		return plan, prepared, nil, err
	}
	admitted, err := manager.write(ctx, prepared, true)
	if err != nil {
		return plan, prepared, nil, classifyError(err, false)
	}
	if admitted == nil || !matchesReference(admitted, req.Resource, false) || prepared.Current != nil && admitted.GetUID() != prepared.Current.GetUID() || !reflect.DeepEqual(withoutFields(admitted, prepared.Fields), withoutFields(prepared.Desired, prepared.Fields)) {
		return plan, prepared, nil, Failure(409, "unexpected_mutation", "dry-run changed identity or fields outside the provider allowlist")
	}
	if validator, ok := provider.(interface {
		ValidateAdmission(Prepared, *unstructured.Unstructured) error
	}); ok {
		if err := validator.ValidateAdmission(prepared, admitted); err != nil {
			return plan, prepared, nil, err
		}
	}
	changes := []v1.Change{}
	for _, path := range prepared.Fields {
		before, beforeFound := jsonValue(prepared.Current, path)
		after, afterFound := jsonValue(admitted, path)
		if beforeFound != afterFound || !reflect.DeepEqual(before, after) {
			changes = append(changes, v1.Change{Path: path, Before: rawValue(before), After: rawValue(after)})
		}
	}
	plan = v1.MutationPlan{APIVersion: v1.Version, ClusterID: manager.opts.ClusterID, Request: req, Changes: changes, Dependencies: prepared.Dependencies, PlannedAt: plannedAt, ExpiresAt: plannedAt.Add(planLifetime), Noop: prepared.Current != nil && len(changes) == 0, Warnings: append([]string{}, prepared.Warnings...)}
	binding, err := json.Marshal(struct {
		Plan                  v1.MutationPlan
		Config                string
		ProviderConfiguration string
		GVR                   string
		Fields                []string
		Desired               map[string]interface{}
		Admitted              map[string]interface{}
	}{comparablePlan(plan), manager.config, providerFingerprint(provider), prepared.GVR.String(), prepared.Fields, normalize(prepared.Desired), normalize(admitted)})
	if err != nil {
		return v1.MutationPlan{}, prepared, nil, err
	}
	digest := sha256.Sum256(binding)
	plan.Digest = hex.EncodeToString(digest[:])
	plan.Token = manager.sign(plan)
	return plan, prepared, admitted, nil
}

func (manager *Manager) validatePrepared(ctx context.Context, req v1.MutationRequest, prepared *Prepared) error {
	gvr, _ := resourceGVR(req.Resource)
	invalid := func() error {
		return Failure(422, "invalid_preparation", "provider returned an invalid identity, dependency or field allowlist")
	}
	if prepared.GVR != gvr || !matchesReference(prepared.Desired, req.Resource, false) || prepared.Current == nil && len(prepared.Fields) == 0 || len(prepared.Fields) > 128 {
		return invalid()
	}
	prepared.Fields = append([]string(nil), prepared.Fields...)
	sort.Strings(prepared.Fields)
	for i, path := range prepared.Fields {
		if _, err := pointerParts(path); err != nil {
			return invalid()
		}
		for _, previous := range prepared.Fields[:i] {
			if path == previous || strings.HasPrefix(path, previous+"/") {
				return invalid()
			}
		}
	}
	prepared.Desired = prepared.Desired.DeepCopy()
	prepared.Dependencies = append([]v1.ResourceReference{}, prepared.Dependencies...)
	if prepared.Current != nil {
		if !matchesReference(prepared.Current, req.Resource, true) || !matchesReference(prepared.Desired, req.Resource, true) || prepared.Current.GetDeletionTimestamp() != nil {
			return Failure(409, "stale_resource", "resource identity or version is stale")
		}
		prepared.Current = prepared.Current.DeepCopy()
		if !reflect.DeepEqual(withoutFields(prepared.Current, prepared.Fields), withoutFields(prepared.Desired, prepared.Fields)) {
			return invalid()
		}
		// Every update, including a noop, pins the root resource in the signed plan.
		prepared.Dependencies = append(prepared.Dependencies, objectReference(prepared.Current))
	} else {
		if req.Resource.UID != "" || req.Resource.ResourceVersion != "" || prepared.Desired.GetUID() != "" || prepared.Desired.GetResourceVersion() != "" {
			return invalid()
		}
		_, err := manager.client.Resource(gvr).Namespace(req.Resource.Namespace).Get(ctx, req.Resource.Name, metav1.GetOptions{})
		if err == nil {
			return Failure(409, "resource_exists", "create target already exists")
		}
		if !apierrors.IsNotFound(err) {
			return classifyError(err, false)
		}
	}
	if len(prepared.Dependencies) > 128 {
		return invalid()
	}
	sort.Slice(prepared.Dependencies, func(i, j int) bool {
		a, _ := json.Marshal(prepared.Dependencies[i])
		b, _ := json.Marshal(prepared.Dependencies[j])
		return string(a) < string(b)
	})
	unique := prepared.Dependencies[:0]
	for _, dep := range prepared.Dependencies {
		if len(unique) == 0 || unique[len(unique)-1] != dep {
			unique = append(unique, dep)
		}
	}
	prepared.Dependencies = unique
	// k8gb continuously updates Gslb status. Pin its full configuration and UID,
	// not status RV churn. Other dependencies retain strict version checks.
	for i := range prepared.Dependencies {
		dep := &prepared.Dependencies[i]
		if dep.ConfigurationDigest != "" {
			return invalid()
		}
		if !isGslbReference(*dep) {
			continue
		}
		gvr, err := dependencyGVR(*dep, req.Resource.Namespace)
		if err != nil || !manager.namespaceAllowed(dep.Namespace) {
			return invalid()
		}
		live, err := manager.client.Resource(gvr).Namespace(dep.Namespace).Get(ctx, dep.Name, metav1.GetOptions{})
		if err != nil {
			return classifyError(err, false)
		}
		if !matchesReference(live, *dep, true) || live.GetDeletionTimestamp() != nil {
			return Failure(409, "stale_dependency", "dependency changed while preparing the plan")
		}
		dep.ConfigurationDigest = configurationDigest(live)
	}
	return nil
}

func (manager *Manager) checkDependencies(ctx context.Context, namespace string, deps []v1.ResourceReference) error {
	for _, dep := range deps {
		gvr, err := dependencyGVR(dep, namespace)
		if err != nil {
			return err
		}
		approvedRead := false
		for _, provider := range manager.opts.Providers {
			if policy, ok := provider.(interface {
				AllowsReadDependency(v1.ResourceReference) bool
			}); ok && policy.AllowsReadDependency(dep) {
				approvedRead = true
			}
		}
		if dep.UID == "" || dep.ResourceVersion == "" || dep.Namespace != "" && !manager.namespaceAllowed(dep.Namespace) && !manager.opts.DNSZones.Matches(dep) && !approvedRead || dep.Kind == "Deployment" && !manager.opts.DNSZones.Matches(dep) {
			return Failure(422, "invalid_dependency", "dependencies must be pinned and use an explicitly allowed scope")
		}
		var resource dynamic.ResourceInterface = manager.client.Resource(gvr)
		if dep.Namespace != "" {
			resource = manager.client.Resource(gvr).Namespace(dep.Namespace)
		}
		live, err := resource.Get(ctx, dep.Name, metav1.GetOptions{})
		if err != nil {
			return classifyError(err, false)
		}
		if !dependencyMatches(live, dep) || live.GetDeletionTimestamp() != nil {
			return Failure(409, "stale_dependency", "dependency UID or resourceVersion changed")
		}
	}
	return nil
}

func isGslbReference(ref v1.ResourceReference) bool {
	return ref.APIVersion == "k8gb.io/v1beta1" && ref.Kind == "Gslb"
}

func configurationDigest(object *unstructured.Unstructured) string {
	data, _ := json.Marshal(normalize(object))
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func dependencyMatches(live *unstructured.Unstructured, dep v1.ResourceReference) bool {
	if dep.ConfigurationDigest == "" {
		return matchesReference(live, dep, true)
	}
	return isGslbReference(dep) && len(dep.ConfigurationDigest) == sha256.Size*2 && matchesReference(live, dep, false) && string(live.GetUID()) == dep.UID && configurationDigest(live) == dep.ConfigurationDigest
}

// Only an already signed, matching Gslb configuration may advance to a fresh RV.
// The actual patch still tests that fresh RV, without retrying a failed write.
func (manager *Manager) refreshGslbRequest(ctx context.Context, plan v1.MutationPlan) (v1.MutationRequest, error) {
	request := plan.Request
	for _, dep := range plan.Dependencies {
		if !isGslbReference(dep) || dep.ConfigurationDigest == "" || dep.APIVersion != request.Resource.APIVersion || dep.Kind != request.Resource.Kind || dep.Name != request.Resource.Name || dep.Namespace != request.Resource.Namespace || dep.UID != request.Resource.UID {
			continue
		}
		gvr, err := dependencyGVR(dep, request.Resource.Namespace)
		if err != nil || !manager.namespaceAllowed(dep.Namespace) {
			return request, Failure(403, "namespace_forbidden", "Gslb dependency is outside the allowed scope")
		}
		live, err := manager.client.Resource(gvr).Namespace(dep.Namespace).Get(ctx, dep.Name, metav1.GetOptions{})
		if err != nil {
			return request, err
		}
		if !dependencyMatches(live, dep) || live.GetDeletionTimestamp() != nil {
			return request, Failure(409, "stale_resource", "Gslb identity or configuration changed; plan again")
		}
		request.Resource.ResourceVersion = live.GetResourceVersion()
		break
	}
	return request, nil
}

func comparablePlan(plan v1.MutationPlan) v1.MutationPlan {
	plan.Token = ""
	plan.Dependencies = append([]v1.ResourceReference{}, plan.Dependencies...)
	for i := range plan.Dependencies {
		dep := &plan.Dependencies[i]
		if !isGslbReference(*dep) || dep.ConfigurationDigest == "" {
			continue
		}
		dep.ResourceVersion = ""
		ref := plan.Request.Resource
		if ref.APIVersion == dep.APIVersion && ref.Kind == dep.Kind && ref.Namespace == dep.Namespace && ref.Name == dep.Name && ref.UID == dep.UID {
			plan.Request.Resource.ResourceVersion = ""
		}
	}
	return plan
}

func providerFingerprint(provider Provider) string {
	if configured, ok := provider.(interface{ ConfigurationFingerprint() string }); ok {
		return configured.ConfigurationFingerprint()
	}
	return ""
}

func (manager *Manager) sign(plan v1.MutationPlan) string {
	plan.Token = ""
	data, _ := json.Marshal(plan)
	mac := hmac.New(sha256.New, manager.opts.SigningKey)
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (manager *Manager) verify(plan v1.MutationPlan) error {
	if !manager.opts.Enabled {
		return Failure(503, "mutations_disabled", "mutations are disabled")
	}
	got, err := base64.RawURLEncoding.DecodeString(plan.Token)
	want, _ := base64.RawURLEncoding.DecodeString(manager.sign(plan))
	if err != nil || len(got) != sha256.Size || !hmac.Equal(got, want) {
		return Failure(403, "invalid_signature", "plan signature is invalid")
	}
	now := manager.opts.Now()
	if plan.APIVersion != v1.Version || plan.ClusterID != manager.opts.ClusterID || plan.PlannedAt.IsZero() || now.Before(plan.PlannedAt) || !now.Before(plan.ExpiresAt) || !plan.ExpiresAt.Equal(plan.PlannedAt.Add(planLifetime)) {
		return Failure(409, "invalid_plan", "plan is expired, future-dated, or bound to another cluster or API version")
	}
	return nil
}

type patchOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

func (manager *Manager) write(ctx context.Context, prepared Prepared, dryRun bool) (*unstructured.Unstructured, error) {
	ctx = singleAttemptContext(ctx)
	resource := manager.client.Resource(prepared.GVR).Namespace(prepared.Desired.GetNamespace())
	dry := []string(nil)
	if dryRun {
		dry = []string{metav1.DryRunAll}
	}
	if prepared.Current == nil {
		return resource.Create(ctx, prepared.Desired.DeepCopy(), metav1.CreateOptions{DryRun: dry, FieldManager: "k8gb-ui-mutation"})
	}
	patch := []patchOperation{{Op: "test", Path: "/metadata/uid", Value: rawValue(string(prepared.Current.GetUID()))}, {Op: "test", Path: "/metadata/resourceVersion", Value: rawValue(prepared.Current.GetResourceVersion())}}
	working := prepared.Current.DeepCopy()
	for _, path := range prepared.Fields {
		before, beforeFound := jsonValue(prepared.Current, path)
		after, afterFound := jsonValue(prepared.Desired, path)
		if beforeFound == afterFound && reflect.DeepEqual(before, after) {
			continue
		}
		op := "add"
		if !afterFound {
			op = "remove"
		} else if beforeFound {
			op = "replace"
		}
		if afterFound {
			parts, _ := pointerParts(path)
			rawParts := strings.Split(path[1:], "/")
			for i := 1; i < len(parts); i++ {
				parent, found, err := unstructured.NestedFieldNoCopy(working.Object, parts[:i]...)
				if err != nil {
					return nil, Failure(422, "invalid_preparation", "patch parent is not an object")
				}
				if !found || parent == nil {
					patch = append(patch, patchOperation{Op: "add", Path: "/" + strings.Join(rawParts[:i], "/"), Value: json.RawMessage(`{}`)})
					if err := unstructured.SetNestedMap(working.Object, map[string]interface{}{}, parts[:i]...); err != nil {
						return nil, err
					}
				}
			}
		}
		var value json.RawMessage
		if afterFound {
			value = rawValue(after)
		}
		patch = append(patch, patchOperation{Op: op, Path: path, Value: value})
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}
	return resource.Patch(ctx, prepared.Desired.GetName(), types.JSONPatchType, data, metav1.PatchOptions{DryRun: dry, FieldManager: "k8gb-ui-mutation"})
}

func normalize(object *unstructured.Unstructured) map[string]interface{} {
	if object == nil {
		return nil
	}
	copy := object.DeepCopy()
	if copy.GetAPIVersion() == "k8gb.io/v1beta1" && copy.GetKind() == "Gslb" {
		unstructured.RemoveNestedField(copy.Object, "status")
	}
	for _, field := range []string{"uid", "resourceVersion", "creationTimestamp", "managedFields", "generation"} {
		unstructured.RemoveNestedField(copy.Object, "metadata", field)
	}
	return copy.Object
}

func withoutFields(object *unstructured.Unstructured, fields []string) map[string]interface{} {
	copy := normalize(object)
	for _, field := range fields {
		parts, _ := pointerParts(field)
		unstructured.RemoveNestedField(copy, parts...)
		for i := len(parts) - 1; i > 0; i-- {
			parent, found, _ := unstructured.NestedMap(copy, parts[:i]...)
			if !found || len(parent) != 0 {
				break
			}
			unstructured.RemoveNestedField(copy, parts[:i]...)
		}
	}
	return copy
}

func classifyError(err error, write bool) error {
	var failure *Error
	if errors.As(err, &failure) {
		return err
	}
	status, unknown := 502, write
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		code := int(apiStatus.Status().Code)
		if code >= 400 && code <= 599 {
			status = code
		}
		if code >= 400 && code < 500 && code != 408 {
			unknown = false
		}
	}
	code := "kubernetes_error"
	if unknown {
		code = "outcome_unknown"
	}
	return &Error{Status: status, Code: code, Message: fmt.Sprintf("Kubernetes mutation request failed: %v", err), OutcomeUnknown: unknown}
}

func (manager *Manager) audit(req v1.MutationRequest, status, digest string, changes []v1.Change, unknown bool) {
	event := v1.AuditEvent{ID: rand.Text(), Time: manager.opts.Now().UTC(), ClusterID: manager.opts.ClusterID, Actor: req.Actor, Provider: req.Provider, Operation: req.Operation, Resource: req.Resource, Reason: req.Reason, Status: status, Digest: digest, Changes: changes, OutcomeUnknown: unknown}
	manager.logger.Info("gslb_mutation", "audit", event)
	if manager.opts.Audit != nil {
		manager.opts.Audit(event)
	}
}
