package mutation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

type mutationAPI struct {
	mu              sync.Mutex
	objects         map[string]*unstructured.Unstructured
	dryRuns, writes int
	patches         [][]patchOperation
	admit           func(*unstructured.Unstructured, bool)
	writeStatus     int
	retryAfter      bool
	disconnect      bool
}

func resourcePath(ref v1.ResourceReference) string {
	gvr, err := resourceGVR(ref)
	if err != nil {
		gvr, _ = dependencyGVR(ref, "app")
	}
	path := "/apis/" + gvr.Group + "/" + gvr.Version
	if gvr.Group == "" {
		path = "/api/" + gvr.Version
	}
	if ref.Namespace != "" {
		path += "/namespaces/" + ref.Namespace
	}
	return path + "/" + gvr.Resource + "/" + ref.Name
}

func (api *mutationAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	api.mu.Lock()
	defer api.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	status := func(code int) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReason(http.StatusText(code)), Message: http.StatusText(code), Code: int32(code)})
	}
	path := r.URL.Path
	current := api.objects[path]
	if r.Method == http.MethodGet {
		if current == nil {
			status(404)
			return
		}
		_ = json.NewEncoder(w).Encode(current)
		return
	}
	if r.Method != http.MethodPatch && r.Method != http.MethodPost {
		status(405)
		return
	}
	dry := r.URL.Query().Get("dryRun") == "All"
	if dry {
		api.dryRuns++
	} else {
		api.writes++
	}
	if !dry && api.writeStatus != 0 {
		if api.retryAfter {
			w.Header().Set("Retry-After", "1")
		}
		status(api.writeStatus)
		return
	}
	var desired *unstructured.Unstructured
	if r.Method == http.MethodPatch {
		if current == nil {
			status(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var ops []patchOperation
		_ = json.Unmarshal(body, &ops)
		api.patches = append(api.patches, ops)
		patch, err := jsonpatch.DecodePatch(body)
		if err != nil {
			status(422)
			return
		}
		original, _ := json.Marshal(current)
		changed, err := patch.Apply(original)
		if err != nil {
			status(409)
			return
		}
		desired = &unstructured.Unstructured{}
		if json.Unmarshal(changed, desired) != nil {
			status(422)
			return
		}
	} else {
		desired = &unstructured.Unstructured{}
		if json.NewDecoder(r.Body).Decode(desired) != nil {
			status(422)
			return
		}
		path += "/" + desired.GetName()
		if api.objects[path] != nil {
			status(409)
			return
		}
		desired.SetUID("new-uid")
		desired.SetResourceVersion("1")
	}
	if api.admit != nil {
		api.admit(desired, dry)
	}
	if !dry {
		desired.SetResourceVersion("2")
		api.objects[path] = desired.DeepCopy()
		if api.disconnect {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
	}
	_ = json.NewEncoder(w).Encode(desired)
}

func fixtureObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "k8gb.io/v1beta1", "kind": "Gslb",
		"metadata": map[string]interface{}{"name": "web", "namespace": "app", "uid": "gslb-uid", "resourceVersion": "1", "labels": map[string]interface{}{"keep": "yes"}},
		"spec":     map[string]interface{}{"strategy": map[string]interface{}{"type": "roundRobin", "dnsTtlSeconds": int64(30), "splitBrainThresholdSeconds": int64(120)}, "ingress": map[string]interface{}{"ingressClassName": "existing"}},
		"status":   map[string]interface{}{"health": "Healthy"},
	}}
}

func fixtureRequest() v1.MutationRequest {
	return v1.MutationRequest{Provider: "native", Operation: "update", Resource: objectReference(fixtureObject()), Actor: "authenticated-user", Reason: "planned change", Policy: &v1.Policy{Strategy: "failover", PrimaryGeoTag: "east", DNSTTLSeconds: 60}}
}

func newHarness(t *testing.T) (*Manager, *mutationAPI, *time.Time) {
	t.Helper()
	api := &mutationAPI{objects: map[string]*unstructured.Unstructured{}}
	object := fixtureObject()
	api.objects[resourcePath(objectReference(object))] = object
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, Timeout: 2 * time.Second, WrapTransport: SingleAttemptTransport})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	manager, err := NewManager(client, Options{ClusterID: "test-cluster", Enabled: true, SigningKey: []byte(strings.Repeat("s", 32)), AllowedNamespaces: []string{"app"}, Providers: map[string]Provider{"native": NewNativeProvider(client, []string{"east", "west"})}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	manager.logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	return manager, api, &now
}

func requireFailure(t *testing.T, err error, code string) *Error {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || code != "" && failure.Code != code {
		t.Fatalf("expected failure %q, got %v", code, err)
	}
	return failure
}

func TestNativeUpdateDryRunAndApply(t *testing.T) {
	manager, api, _ := newHarness(t)
	var events []v1.AuditEvent
	manager.opts.Audit = func(event v1.AuditEvent) { events = append(events, event) }
	plan, err := manager.Plan(context.Background(), fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	if api.writes != 0 || api.dryRuns != 1 || len(plan.Dependencies) != 1 || plan.Noop {
		t.Fatalf("invalid plan: %+v", plan)
	}
	// Exercise exactly the same JSON round trip used by HTTP clients.
	data, _ := json.Marshal(plan)
	plan = v1.MutationPlan{}
	_ = json.Unmarshal(data, &plan)
	result, err := manager.Apply(context.Background(), plan)
	if err != nil || result.Status != "applied" || result.Reconciliation != "pending" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if api.writes != 1 || api.dryRuns != 2 {
		t.Fatalf("writes=%d dryRuns=%d", api.writes, api.dryRuns)
	}
	for _, patch := range api.patches {
		if len(patch) < 2 || patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" {
			t.Fatalf("unsafe patch: %+v", patch)
		}
	}
	saved := api.objects[resourcePath(fixtureRequest().Resource)]
	if value, _, _ := unstructured.NestedInt64(saved.Object, "spec", "strategy", "splitBrainThresholdSeconds"); value != 120 {
		t.Fatal("unrelated strategy field lost")
	}
	if saved.GetLabels()["keep"] != "yes" || !reflect.DeepEqual(saved.Object["status"], fixtureObject().Object["status"]) {
		t.Fatal("unrelated fields lost")
	}
	if len(events) != 4 || events[0].Status != "plan_started" || events[3].Status != "applied" {
		t.Fatalf("audit: %+v", events)
	}
	_, err = manager.Apply(context.Background(), plan)
	if err == nil || api.writes != 1 {
		t.Fatal("replayed stale update wrote again")
	}
}

func TestEveryPlanFieldIsSigned(t *testing.T) {
	manager, api, _ := newHarness(t)
	plan, err := manager.Plan(context.Background(), fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*v1.MutationPlan){
		"version":           func(p *v1.MutationPlan) { p.APIVersion += "x" },
		"cluster":           func(p *v1.MutationPlan) { p.ClusterID += "x" },
		"actor":             func(p *v1.MutationPlan) { p.Request.Actor += "x" },
		"reason":            func(p *v1.MutationPlan) { p.Request.Reason += "x" },
		"policy":            func(p *v1.MutationPlan) { p.Request.Policy.DNSTTLSeconds++ },
		"diff":              func(p *v1.MutationPlan) { p.Changes[0].After = json.RawMessage(`{}`) },
		"dependencies":      func(p *v1.MutationPlan) { p.Dependencies[0].UID += "x" },
		"configuration-pin": func(p *v1.MutationPlan) { p.Dependencies[0].ConfigurationDigest += "x" },
		"dependency-rv":     func(p *v1.MutationPlan) { p.Dependencies[0].ResourceVersion += "x" },
		"planned":           func(p *v1.MutationPlan) { p.PlannedAt = p.PlannedAt.Add(time.Second) },
		"expires":           func(p *v1.MutationPlan) { p.ExpiresAt = p.ExpiresAt.Add(time.Second) },
		"digest":            func(p *v1.MutationPlan) { p.Digest += "x" },
		"noop":              func(p *v1.MutationPlan) { p.Noop = !p.Noop },
		"warnings":          func(p *v1.MutationPlan) { p.Warnings = append(p.Warnings, "x") },
		"token":             func(p *v1.MutationPlan) { p.Token += "x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(plan)
			var changed v1.MutationPlan
			_ = json.Unmarshal(data, &changed)
			mutate(&changed)
			_, err := manager.Apply(context.Background(), changed)
			requireFailure(t, err, "invalid_signature")
		})
	}
	if api.writes != 0 || api.dryRuns != 1 {
		t.Fatal("tampering reached Kubernetes")
	}
}

func TestGslbStatusChurnKeepsPlanButConfigurationDriftDoesNot(t *testing.T) {
	for _, mode := range []string{"status", "rv-only", "spec", "annotation", "label", "owner", "uid", "deleting"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, _ := newHarness(t)
			plan, err := manager.Plan(context.Background(), fixtureRequest())
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Dependencies[0].ConfigurationDigest) != 64 {
				t.Fatal("Gslb configuration was not pinned")
			}
			live := api.objects[resourcePath(fixtureRequest().Resource)]
			live.SetResourceVersion("background-status-rv")
			switch mode {
			case "status":
				live.Object["status"] = map[string]interface{}{"health": "Unhealthy"}
			case "spec":
				_ = unstructured.SetNestedField(live.Object, int64(99), "spec", "strategy", "dnsTtlSeconds")
			case "annotation":
				live.SetAnnotations(map[string]string{"other-writer": "changed"})
			case "label":
				live.SetLabels(map[string]string{"other-writer": "changed"})
			case "owner":
				live.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other"}})
			case "uid":
				live.SetUID("replacement")
			case "deleting":
				now := metav1.Now()
				live.SetDeletionTimestamp(&now)
			}
			result, err := manager.Apply(context.Background(), plan)
			if mode != "status" && mode != "rv-only" {
				if err == nil || api.writes != 0 {
					t.Fatalf("configuration drift was accepted: %v", err)
				}
				return
			}
			if err != nil || result.Status != "applied" || api.writes != 1 {
				t.Fatalf("status-only change invalidated plan: %+v %v", result, err)
			}
			last := api.patches[len(api.patches)-1]
			if string(last[1].Value) != `"background-status-rv"` {
				t.Fatalf("write did not use fresh CAS RV: %+v", last)
			}
			if !reflect.DeepEqual(api.objects[resourcePath(fixtureRequest().Resource)].Object["status"], live.Object["status"]) {
				t.Fatal("status was overwritten")
			}
		})
	}
}

func TestExpirationAndConfigurationDrift(t *testing.T) {
	for _, mode := range []string{"expired", "future", "namespace-config", "recreated", "admission"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, now := newHarness(t)
			plan, err := manager.Plan(context.Background(), fixtureRequest())
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "expired":
				*now = now.Add(planLifetime)
			case "future":
				*now = now.Add(-time.Second)
			case "namespace-config":
				manager.config += "changed"
			case "recreated":
				api.objects[resourcePath(fixtureRequest().Resource)].SetUID("replacement-uid")
			case "admission":
				api.admit = func(object *unstructured.Unstructured, _ bool) {
					_ = unstructured.SetNestedField(object.Object, int64(90), "spec", "strategy", "dnsTtlSeconds")
				}
			}
			_, err = manager.Apply(context.Background(), plan)
			if err == nil || api.writes != 0 {
				t.Fatalf("unsafe apply: %v", err)
			}
		})
	}
}

func TestAdmissionScopeDefaultsAndActualDrift(t *testing.T) {
	for _, mode := range []string{"default", "outside-spec", "identity", "annotation", "actual"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, _ := newHarness(t)
			api.admit = func(object *unstructured.Unstructured, dry bool) {
				switch mode {
				case "default":
					_ = unstructured.SetNestedField(object.Object, int64(45), "spec", "strategy", "dnsTtlSeconds")
				case "outside-spec":
					_ = unstructured.SetNestedField(object.Object, "changed", "spec", "ingress", "ingressClassName")
				case "identity":
					object.SetUID("changed")
				case "annotation":
					object.SetAnnotations(map[string]string{"injected": "bad"})
				case "actual":
					if !dry {
						_ = unstructured.SetNestedField(object.Object, "changed", "spec", "ingress", "ingressClassName")
					}
				}
			}
			plan, err := manager.Plan(context.Background(), fixtureRequest())
			if mode != "default" && mode != "actual" {
				requireFailure(t, err, "unexpected_mutation")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "default" && !strings.Contains(string(plan.Changes[0].After), `45`) {
				t.Fatal("admitted defaults were not captured")
			}
			result, err := manager.Apply(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "actual" && result.Status != "drift" || mode == "default" && result.Status != "applied" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestNoopStillDryRunsWithoutRealWrite(t *testing.T) {
	manager, api, _ := newHarness(t)
	req := fixtureRequest()
	req.Policy = &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 30}
	plan, err := manager.Plan(context.Background(), req)
	if err != nil || !plan.Noop {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	result, err := manager.Apply(context.Background(), plan)
	if err != nil || result.Status != "noop" || api.writes != 0 || api.dryRuns != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestWriteFailuresHaveNoRetriesAndUnknownOutcome(t *testing.T) {
	for _, mode := range []string{"forbidden", "conflict", "server", "disconnect", "retry-after-429", "retry-after-503"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, _ := newHarness(t)
			plan, err := manager.Plan(context.Background(), fixtureRequest())
			if err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			switch mode {
			case "forbidden":
				api.writeStatus = 403
			case "conflict":
				api.writeStatus = 409
			case "server":
				api.writeStatus = 500
			case "disconnect":
				api.disconnect = true
			case "retry-after-429":
				api.writeStatus, api.retryAfter = 429, true
			case "retry-after-503":
				api.writeStatus, api.retryAfter = 503, true
			}
			api.mu.Unlock()
			_, err = manager.Apply(context.Background(), plan)
			failure := requireFailure(t, err, "")
			api.mu.Lock()
			writes := api.writes
			api.mu.Unlock()
			if failure.OutcomeUnknown != (mode == "server" || mode == "disconnect" || mode == "retry-after-503") || writes != 1 {
				t.Fatalf("failure=%+v writes=%d", failure, writes)
			}
		})
	}
}

func TestCreatePinnedTargetsAndLegacy(t *testing.T) {
	for _, version := range []string{"k8gb.io/v1beta1", "k8gb.absa.oss/v1beta1"} {
		for _, kind := range []string{"Ingress", "HTTPRoute"} {
			t.Run(version+kind, func(t *testing.T) {
				manager, api, _ := newHarness(t)
				req := fixtureRequest()
				req.Operation = "create"
				req.Resource.APIVersion, req.Resource.Name, req.Resource.UID, req.Resource.ResourceVersion = version, "new", "", ""
				target := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": kind, "metadata": map[string]interface{}{"name": "route", "namespace": "app", "uid": "target-uid", "resourceVersion": "7"}}}
				if kind == "HTTPRoute" {
					target.SetAPIVersion("gateway.networking.k8s.io/v1")
				}
				ref := objectReference(target)
				req.Target = &ref
				api.objects[resourcePath(ref)] = target
				req.Policy = &v1.Policy{Strategy: "weightedRoundRobin", Weights: map[string]int64{"east": 80, "west": 20}}
				plan, err := manager.Plan(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				result, err := manager.Apply(context.Background(), plan)
				if err != nil || result.Status != "applied" || api.writes != 1 {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				saved := api.objects[resourcePath(req.Resource)]
				strategy, _, _ := unstructured.NestedString(saved.Object, "spec", "strategy", "type")
				if strategy != "roundRobin" {
					t.Fatal("weighted form was not converted")
				}
				if _, found, _ := unstructured.NestedMap(saved.Object, "spec", "ingress"); found {
					t.Fatal("embedded ingress was created")
				}
			})
		}
	}
}

func TestCreateDependencyDriftAndOwnership(t *testing.T) {
	for _, mode := range []string{"unpinned", "recreated", "version", "owned", "cross-namespace"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, _ := newHarness(t)
			req := fixtureRequest()
			req.Operation, req.Resource.Name, req.Resource.UID, req.Resource.ResourceVersion = "create", "new", "", ""
			target := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]interface{}{"name": "route", "namespace": "app", "uid": "target-uid", "resourceVersion": "7"}}}
			ref := objectReference(target)
			req.Target = &ref
			api.objects[resourcePath(ref)] = target
			if mode == "unpinned" {
				req.Target.UID = ""
			}
			if mode == "cross-namespace" {
				req.Target.Namespace = "other"
			}
			if mode == "owned" {
				target.SetOwnerReferences([]metav1.OwnerReference{{Kind: "IngressRoute", Name: "parent"}})
			}
			plan, err := manager.Plan(context.Background(), req)
			if mode == "unpinned" || mode == "owned" || mode == "cross-namespace" {
				if err == nil {
					t.Fatal("unsafe target accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "recreated" {
				target.SetUID("replacement")
			} else {
				target.SetResourceVersion("8")
			}
			_, err = manager.Apply(context.Background(), plan)
			if err == nil || api.writes != 0 {
				t.Fatal("stale target wrote")
			}
		})
	}
}

func TestValidationAndDisabledDefaults(t *testing.T) {
	manager, api, _ := newHarness(t)
	disabled, err := NewManager(manager.client, Options{ClusterID: "test-cluster"})
	if err != nil || !disabled.Capabilities().ReadOnly {
		t.Fatal("default is not read-only", err)
	}
	_, err = disabled.Plan(context.Background(), fixtureRequest())
	requireFailure(t, err, "mutations_disabled")
	_, err = NewManager(manager.client, Options{ClusterID: "test", Enabled: true, SigningKey: []byte("short")})
	if err == nil {
		t.Fatal("weak key accepted")
	}
	for _, mutate := range []func(*v1.MutationRequest){
		func(r *v1.MutationRequest) { r.Actor = " " }, func(r *v1.MutationRequest) { r.Reason = strings.Repeat("x", 1025) },
		func(r *v1.MutationRequest) { r.Resource.Namespace = "other" }, func(r *v1.MutationRequest) { r.Resource.Namespace = "" },
		func(r *v1.MutationRequest) { r.Resource.APIVersion = "evil/v1" }, func(r *v1.MutationRequest) { r.Resource.Kind = "Service" },
		func(r *v1.MutationRequest) { r.Operation = "delete" }, func(r *v1.MutationRequest) { r.Resource.UID = "" },
		func(r *v1.MutationRequest) { r.Policy.PrimaryGeoTag = "unknown" }, func(r *v1.MutationRequest) { r.Policy.DNSTTLSeconds = -1 },
	} {
		req := fixtureRequest()
		mutate(&req)
		if _, err := manager.Plan(context.Background(), req); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if api.writes != 0 || api.dryRuns != 0 {
		t.Fatal("invalid requests reached writes")
	}
}

func TestNativeFakePreservesUnknownFieldsAndRejectsOwned(t *testing.T) {
	current := fixtureObject()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), current)
	provider := NewNativeProvider(client, []string{"east", "west"})
	prepared, err := provider.Prepare(context.Background(), fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Current.Object, current.Object) {
		t.Fatal("current object mutated")
	}
	current.SetOwnerReferences([]metav1.OwnerReference{{Kind: "IngressRoute", Name: "parent"}})
	gvr := schema.GroupVersionResource{Group: "k8gb.io", Version: "v1beta1", Resource: "gslbs"}
	_, _ = client.Resource(gvr).Namespace("app").Update(context.Background(), current, metav1.UpdateOptions{})
	_, err = provider.Prepare(context.Background(), fixtureRequest())
	requireFailure(t, err, "owned_resource")
}

func TestConcurrentPlanning(t *testing.T) {
	manager, api, _ := newHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.Plan(context.Background(), fixtureRequest()); err != nil {
				t.Error(err)
			}
			_ = manager.Capabilities()
		}()
	}
	wg.Wait()
	if api.writes != 0 || api.dryRuns != 12 {
		t.Fatal("concurrent plan mutated objects")
	}
}

func TestErrorClassification(t *testing.T) {
	for _, err := range []error{context.DeadlineExceeded, errors.New("network failure"), apierrors.NewTimeoutError("timeout", 1)} {
		if !requireFailure(t, classifyError(err, true), "").OutcomeUnknown {
			t.Fatal("ambiguous write classified as safe")
		}
		if requireFailure(t, classifyError(err, false), "").OutcomeUnknown {
			t.Fatal("read/dry-run classified as actual write")
		}
	}
}

type configuredProvider struct {
	Provider
	dependencies []v1.ResourceReference
	fingerprint  string
}

func (provider *configuredProvider) Prepare(ctx context.Context, request v1.MutationRequest) (Prepared, error) {
	prepared, err := provider.Provider.Prepare(ctx, request)
	prepared.Dependencies = append(prepared.Dependencies, provider.dependencies...)
	return prepared, err
}

func (provider *configuredProvider) ConfigurationFingerprint() string { return provider.fingerprint }

func TestReadOnlyClusterDependencies(t *testing.T) {
	for _, mode := range []string{"valid", "webhook-rv", "webhook-uid", "namespace-rv", "wrong-namespace", "wrong-kind", "webhook-scoped"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, _ := newHarness(t)
			webhook := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingWebhookConfiguration", "metadata": map[string]interface{}{"name": "gslb-admission", "uid": "webhook-uid", "resourceVersion": "9"}}}
			namespace := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": "app", "uid": "ns-uid", "resourceVersion": "5"}}}
			webhookRef, namespaceRef := objectReference(webhook), objectReference(namespace)
			api.objects[resourcePath(webhookRef)] = webhook
			api.objects[resourcePath(namespaceRef)] = namespace
			if mode == "wrong-namespace" {
				namespaceRef.Name = "other"
			}
			if mode == "wrong-kind" {
				webhookRef.Kind = "ValidatingWebhookConfiguration"
			}
			if mode == "webhook-scoped" {
				webhookRef.Namespace = "app"
			}
			provider := &configuredProvider{Provider: manager.opts.Providers["native"], dependencies: []v1.ResourceReference{webhookRef, namespaceRef}}
			manager.opts.Providers["native"] = provider
			plan, err := manager.Plan(context.Background(), fixtureRequest())
			if mode == "wrong-namespace" || mode == "wrong-kind" || mode == "webhook-scoped" {
				if err == nil || api.dryRuns != 0 {
					t.Fatal("overbroad dependency read permitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "webhook-rv":
				webhook.SetResourceVersion("10")
			case "webhook-uid":
				webhook.SetUID("recreated")
			case "namespace-rv":
				namespace.SetResourceVersion("6")
			}
			result, err := manager.Apply(context.Background(), plan)
			if mode == "valid" {
				if err != nil || result.Status != "applied" {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil || api.writes != 0 {
				t.Fatal("stale cluster dependency accepted")
			}
			if _, err := resourceGVR(webhookRef); err == nil {
				t.Fatal("webhook permitted as a mutation target")
			}
			if _, err := resourceGVR(namespaceRef); err == nil {
				t.Fatal("namespace permitted as a mutation target")
			}
		})
	}
}

func TestProviderConfigurationFingerprint(t *testing.T) {
	manager, api, _ := newHarness(t)
	provider := &configuredProvider{Provider: manager.opts.Providers["native"], fingerprint: "domain-a/plugin-a"}
	manager.opts.Providers["native"] = provider
	plan, err := manager.Plan(context.Background(), fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	provider.fingerprint = "domain-b/plugin-b"
	_, err = manager.Apply(context.Background(), plan)
	requireFailure(t, err, "plan_drift")
	if api.writes != 0 {
		t.Fatal("configuration drift wrote unchanged parent patch")
	}
	// Native's explicit GeoTag configuration is also bound, even if both allow
	// the exact same requested policy and produce the same Kubernetes patch.
	manager, api, _ = newHarness(t)
	plan, err = manager.Plan(context.Background(), fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	manager.opts.Providers["native"] = NewNativeProvider(manager.client, []string{"east", "west", "additional"})
	_, err = manager.Apply(context.Background(), plan)
	requireFailure(t, err, "plan_drift")
	if api.writes != 0 {
		t.Fatal("native configuration drift wrote")
	}
}

func TestNativeServiceHealthDefaultIsCreateOnly(t *testing.T) {
	for _, mode := range []string{"Any", "All", "unexpected", "update"} {
		t.Run(mode, func(t *testing.T) {
			manager, api, _ := newHarness(t)
			req := fixtureRequest()
			if mode != "update" {
				req.Operation, req.Resource.Name, req.Resource.UID, req.Resource.ResourceVersion = "create", "new", "", ""
				target := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "metadata": map[string]interface{}{"name": "route", "namespace": "app", "uid": "target-uid", "resourceVersion": "7"}}}
				ref := objectReference(target)
				req.Target = &ref
				api.objects[resourcePath(ref)] = target
			}
			api.admit = func(object *unstructured.Unstructured, _ bool) {
				value := mode
				if mode == "update" {
					value = "Any"
				}
				_ = unstructured.SetNestedField(object.Object, value, "spec", "serviceHealthPolicy")
				if mode == "unexpected" {
					_ = unstructured.SetNestedField(object.Object, true, "spec", "unexpected")
				}
			}
			plan, err := manager.Plan(context.Background(), req)
			if mode != "Any" {
				requireFailure(t, err, "unexpected_mutation")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			captured := false
			for _, change := range plan.Changes {
				if change.Path == "/spec/serviceHealthPolicy" && string(change.After) == `"Any"` {
					captured = true
				}
			}
			if !captured {
				t.Fatal("create default missing from signed diff")
			}
			result, err := manager.Apply(context.Background(), plan)
			if err != nil || result.Status != "applied" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestNestedAnnotationPatchCreatesOnlyMissingParents(t *testing.T) {
	manager, api, _ := newHarness(t)
	current := fixtureObject()
	desired := current.DeepCopy()
	desired.SetAnnotations(map[string]string{"k8gb.io/setting": "yes"})
	gvr, _ := resourceGVR(objectReference(current))
	prepared := Prepared{GVR: gvr, Current: current, Desired: desired, Fields: []string{"/metadata/annotations/k8gb.io~1setting"}}
	if err := manager.validatePrepared(context.Background(), fixtureRequest(), &prepared); err != nil {
		t.Fatal(err)
	}
	admitted, err := manager.write(context.Background(), prepared, true)
	if err != nil || admitted.GetAnnotations()["k8gb.io/setting"] != "yes" {
		t.Fatalf("missing-parent patch failed: %v", err)
	}
	if api.writes != 0 {
		t.Fatal("dry-run persisted annotations")
	}
}

func TestProviderEmptyFieldsNoopAndOverlappingFields(t *testing.T) {
	manager, _, _ := newHarness(t)
	current := fixtureObject()
	gvr, _ := resourceGVR(objectReference(current))
	prepared := Prepared{GVR: gvr, Current: current, Desired: current.DeepCopy()}
	if err := manager.validatePrepared(context.Background(), fixtureRequest(), &prepared); err != nil {
		t.Fatal("empty provider fields must permit a noop", err)
	}
	prepared.Fields = []string{"/spec/a", "/spec/a-other", "/spec/a/child"}
	requireFailure(t, manager.validatePrepared(context.Background(), fixtureRequest(), &prepared), "invalid_preparation")
}
