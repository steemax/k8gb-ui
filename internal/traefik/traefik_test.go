package traefik

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func config() Config {
	return defaults(Config{MTLSTLSOption: "app-client-auth@kubernetescrd", CNPlugin: "example-authz", AllowedGeoTags: []string{"east", "west"}})
}

func route(t *testing.T, annotations map[string]string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal([]byte(`{"apiVersion":"traefik.io/v1alpha1","kind":"IngressRoute","metadata":{"name":"api","namespace":"app","uid":"parent-uid","resourceVersion":"10","labels":{"app":"keep"}},"spec":{"entryPoints":["websecure","grpc"],"routes":[{"match":"Host(`+"`api.example.test`"+`)","kind":"Rule","services":[{"name":"backend","port":8080}]}],"tls":{"secretName":"keep"}}}`), &u.Object); err != nil {
		t.Fatal(err)
	}
	u.SetAnnotations(annotations)
	return u
}

func client(objects ...runtime.Object) *fake.FakeDynamicClient {
	resources := map[schema.GroupVersionResource]string{ingressGVR: "IngressList", gslbGVR: "GslbList"}
	resources[legacyGslbGVR] = "GslbList"
	for _, group := range []string{"traefik.io", "traefik.containo.us"} {
		resources[schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "ingressroutes"}] = "IngressRouteList"
		resources[schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "middlewares"}] = "MiddlewareList"
		resources[schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "tlsoptions"}] = "TLSOptionList"
		objects = append(objects, &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": group + "/v1alpha1", "kind": "TLSOption", "metadata": map[string]interface{}{"namespace": "app", "name": "client-auth", "uid": "tls-option-uid", "resourceVersion": "1"}, "spec": map[string]interface{}{"clientAuth": map[string]interface{}{"clientAuthType": "RequireAndVerifyClientCert", "secretNames": []interface{}{"client-ca"}}}}})
	}
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), resources, objects...)
	c.PrependReactor("create", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
		u := a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
		if u.GetUID() == "" {
			u.SetUID(types.UID("child-" + u.GetName() + u.GetKind()))
		}
		u.SetResourceVersion("1")
		return false, nil, nil
	})
	return c
}

func writes(c *fake.FakeDynamicClient) []ktesting.Action {
	var out []ktesting.Action
	for _, a := range c.Actions() {
		if !slices.Contains([]string{"get", "list", "watch"}, a.GetVerb()) {
			out = append(out, a)
		}
	}
	return out
}

// client-go's dynamic fake drops DeleteOptions before recording an Action.
// Capture at the interface boundary to verify real-client CAS options as well.
type deleteRecorder struct {
	dynamic.Interface
	options *[]metav1.DeleteOptions
}
type deleteNamespaceable struct {
	dynamic.NamespaceableResourceInterface
	options *[]metav1.DeleteOptions
}
type deleteResource struct {
	dynamic.ResourceInterface
	options *[]metav1.DeleteOptions
}

func (c deleteRecorder) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return deleteNamespaceable{c.Interface.Resource(gvr), c.options}
}
func (c deleteNamespaceable) Namespace(ns string) dynamic.ResourceInterface {
	return deleteResource{c.NamespaceableResourceInterface.Namespace(ns), c.options}
}
func (c deleteResource) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	*c.options = append(*c.options, opts)
	return c.ResourceInterface.Delete(ctx, name, opts, subresources...)
}

func mustSet(t *testing.T, u *unstructured.Unstructured, v interface{}, path ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(u.Object, v, path...); err != nil {
		t.Fatal(err)
	}
}

func TestLiteralHostBooleanProof(t *testing.T) {
	for _, match := range []string{
		"Host(`api.example.test`)",
		"Host(`api.example.test`) && (PathPrefix(`/api`) || PathPrefix(`/swagger`))",
		"(Path(`/`) || Method(`GET`)) && Host(\"api.example.test\")",
		"Host(`api.example.test`) && !Path(`/private`)",
	} {
		t.Run(match, func(t *testing.T) {
			host, err := literalHost(match)
			if err != nil || host != "api.example.test" {
				t.Fatalf("host=%q err=%v", host, err)
			}
		})
	}
	for _, match := range []string{
		"Host(`api.example.test`) || PathPrefix(`/api`)",
		"Host(`api.example.test`) && Path(`/api`) || Path(`/bypass`)",
		"!Host(`api.example.test`)", "HostRegexp(`.*`)",
		"Host(`a.example.test`) || Host(`b.example.test`)",
		"Host(`a.example.test`, `b.example.test`)", "Host(variable)", "Host(`UPPER.example.test`)",
	} {
		t.Run(match, func(t *testing.T) {
			if _, err := literalHost(match); err == nil {
				t.Fatal("accepted unsafe match")
			}
		})
	}
}

func TestAnnotationSpelling(t *testing.T) {
	a := map[string]string{"gslb_strategy": "failover", Prefix + "strategy": "failover", "dnsttl": "30", "unrelated": "keep"}
	setAnnotation(a, "strategy", "roundRobin")
	setAnnotation(a, "dnsttl", "60")
	setAnnotation(a, "local", "true")
	if a["gslb_strategy"] != "roundRobin" || a[Prefix+"strategy"] != "roundRobin" || a["dnsttl"] != "60" || a[Prefix+"dnsttl"] != "" || a[Prefix+"local"] != "true" {
		t.Fatalf("unexpected spelling migration: %v", a)
	}
	setAnnotation(a, "strategy", "")
	if _, ok := a["gslb_strategy"]; ok || a[Prefix+"strategy"] != "" || a["unrelated"] != "keep" {
		t.Fatalf("bad explicit clear: %v", a)
	}
}

func TestCandidatesAndRouteSelection(t *testing.T) {
	for _, api := range []string{"traefik.io/v1alpha1", "traefik.containo.us/v1alpha1"} {
		u := route(t, map[string]string{"gslb": "true", "gslb_weight_enable": "true", "gslb_weights": "east=80,west=20", "gslb_strategy": "roundRobin"})
		u.SetAPIVersion(api)
		c := Candidates(*u, config())
		if !c.Eligible || !c.Enabled || c.Policy.Weights["east"] != 80 || c.Hostnames[0] != "api.example.test" {
			t.Fatalf("candidate: %+v", c)
		}
	}
	u := route(t, nil)
	routes, _, _ := unstructured.NestedSlice(u.Object, "spec", "routes")
	first := map[string]interface{}{"match": "Host(`block.example.test`)", "middlewares": []interface{}{map[string]interface{}{"name": "deny"}}, "services": []interface{}{map[string]interface{}{"kind": "TraefikService", "name": "invalid"}}}
	u.Object["spec"].(map[string]interface{})["routes"] = []interface{}{first, routes[0]}
	cfg := config()
	cfg.BlockingMiddleware = "deny"
	if c := Candidate(*u, cfg); !c.Eligible || c.Hostnames[0] != "api.example.test" {
		t.Fatalf("legacy route selection: %+v", c)
	}
	u.SetAnnotations(map[string]string{RouteIndex: "0"})
	if Candidate(*u, cfg).Eligible {
		t.Fatal("accepted first TraefikService")
	}
	u.SetAnnotations(map[string]string{RouteIndex: "2"})
	if Candidate(*u, cfg).Eligible {
		t.Fatal("accepted index 2")
	}
	u = route(t, nil)
	svc := u.Object["spec"].(map[string]interface{})["routes"].([]interface{})[0].(map[string]interface{})["services"].([]interface{})[0].(map[string]interface{})
	for _, port := range []interface{}{"8080", nil, int64(0), int64(65536)} {
		svc["port"] = port
		if Candidate(*u, config()).Eligible {
			t.Fatalf("accepted port %v", port)
		}
	}
	delete(svc, "port")
	if Candidate(*u, config()).Eligible {
		t.Fatal("accepted missing port")
	}
}

func admit(t *testing.T, handler http.Handler, u, old *unstructured.Unstructured, dry bool) (*admissionResponse, *unstructured.Unstructured) {
	t.Helper()
	gvr, _ := routeGVR(u.GetAPIVersion())
	body, _ := json.Marshal(u.Object)
	req := &admissionRequest{UID: "request-uid", Kind: u.GroupVersionKind(), Resource: gvr, Namespace: u.GetNamespace(), Name: u.GetName(), Operation: "CREATE", Object: body, DryRun: &dry}
	if old != nil {
		req.Operation = "UPDATE"
		req.OldObject, _ = json.Marshal(old.Object)
	}
	wire, _ := json.Marshal(admissionReview{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview", Request: req})
	r := httptest.NewRequest("POST", "/mutate", strings.NewReader(string(wire)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var review admissionReview
	if err := json.Unmarshal(w.Body.Bytes(), &review); err != nil || review.Response == nil {
		t.Fatalf("invalid response: %s", w.Body.String())
	}
	if review.Response.UID != req.UID {
		t.Fatal("response lost request UID")
	}
	result := u.DeepCopy()
	if len(review.Response.Patch) > 0 {
		patch, err := jsonpatch.DecodePatch(review.Response.Patch)
		if err != nil {
			t.Fatal(err)
		}
		patched, err := patch.Apply(body)
		if err != nil {
			t.Fatalf("invalid JSON patch %s: %v", review.Response.Patch, err)
		}
		if err := json.Unmarshal(patched, &result.Object); err != nil {
			t.Fatal(err)
		}
	}
	return review.Response, result
}

func TestAdmissionMTLSDryRunIsPureAndPreservesOld(t *testing.T) {
	old := route(t, map[string]string{"unrelated": "keep", "gslb": "true"})
	u := old.DeepCopy()
	a := u.GetAnnotations()
	a["gslb_mtls"] = "true"
	u.SetAnnotations(a)
	c := client(old)
	handler := NewAdmissionHandler(config())
	real, changed := admit(t, handler, u, old, false)
	dry, _ := admit(t, handler, u, old, true)
	if !real.Allowed || !reflect.DeepEqual(real, dry) || len(c.Actions()) != 0 {
		t.Fatalf("non-pure admission: %+v / %+v / %v", real, dry, c.Actions())
	}
	if got := changed.GetAnnotations()[OriginalEntrypoints]; got != `["websecure","grpc"]` {
		t.Fatalf("snapshot %s", got)
	}
	if changed.GetAnnotations()["unrelated"] != "keep" {
		t.Fatal("lost unrelated annotation")
	}
	if secret, _, _ := unstructured.NestedString(changed.Object, "spec", "tls", "secretName"); secret != "keep" {
		t.Fatal("modified unrelated server certificate")
	}
	option, _, _ := unstructured.NestedMap(changed.Object, "spec", "tls", "options")
	if !reflect.DeepEqual(option, map[string]interface{}{"name": "client-auth", "namespace": "app"}) {
		t.Fatal("parent does not enforce selected mTLS policy")
	}
	for _, deleteSnapshot := range []bool{false, true} {
		off := changed.DeepCopy()
		a := off.GetAnnotations()
		a["gslb_mtls"] = "false"
		if deleteSnapshot {
			delete(a, OriginalEntrypoints)
		}
		off.SetAnnotations(a)
		response, restored := admit(t, handler, off, changed, false)
		entries, _, _ := unstructured.NestedStringSlice(restored.Object, "spec", "entryPoints")
		if !response.Allowed || !reflect.DeepEqual(entries, []string{"websecure", "grpc"}) || restored.GetAnnotations()[OriginalEntrypoints] != "" {
			t.Fatalf("restore failed: %+v %v", response, restored.Object)
		}
	}
	tampered := changed.DeepCopy()
	a = tampered.GetAnnotations()
	a[OriginalEntrypoints] = `["evil"]`
	tampered.SetAnnotations(a)
	if response, _ := admit(t, handler, tampered, changed, false); response.Allowed {
		t.Fatal("accepted changed snapshot")
	}
	wrongUID := u.DeepCopy()
	wrongUID.SetUID("other")
	if response, _ := admit(t, handler, wrongUID, old, true); response.Allowed {
		t.Fatal("accepted changed UID")
	}
}

func TestAdmissionLegacyAndAbsentEntrypoints(t *testing.T) {
	for _, empty := range []bool{false, true} {
		u := route(t, map[string]string{"mtls": "true"})
		if empty {
			_ = unstructured.SetNestedStringSlice(u.Object, []string{}, "spec", "entryPoints")
		} else {
			unstructured.RemoveNestedField(u.Object, "spec", "entryPoints")
		}
		response, on := admit(t, NewAdmissionHandler(config()), u, nil, false)
		if !response.Allowed {
			t.Fatal(response.Result)
		}
		off := on.DeepCopy()
		a := off.GetAnnotations()
		a["mtls"] = "false"
		off.SetAnnotations(a)
		response, restored := admit(t, NewAdmissionHandler(config()), off, on, false)
		_, found, _ := unstructured.NestedSlice(restored.Object, "spec", "entryPoints")
		if !response.Allowed || found != empty {
			t.Fatalf("absent vs empty lost: empty=%v response=%+v object=%v", empty, response, restored.Object)
		}
	}
	old := route(t, map[string]string{"mtls": "true"})
	_ = unstructured.SetNestedStringSlice(old.Object, []string{"mtls"}, "spec", "entryPoints")
	off := old.DeepCopy()
	off.SetAnnotations(map[string]string{"mtls": "false"})
	if response, _ := admit(t, NewAdmissionHandler(config()), off, old, false); response.Allowed {
		t.Fatal("guessed legacy ordinary entrypoint")
	}
	off.SetAnnotations(map[string]string{"mtls": "false", "entrypoint": "grpc"})
	response, restored := admit(t, NewAdmissionHandler(config()), off, old, false)
	entries, _, _ := unstructured.NestedStringSlice(restored.Object, "spec", "entryPoints")
	if !response.Allowed || !reflect.DeepEqual(entries, []string{"grpc"}) {
		t.Fatal("explicit legacy recovery failed")
	}
}

func TestAdmissionHTTPGuards(t *testing.T) {
	h := NewAdmissionHandler(Config{})
	for _, tc := range []struct {
		method, body, content string
		code                  int
	}{
		{"GET", "", "application/json", 405}, {"POST", "{}", "text/plain", 415},
		{"POST", "{}", "application/json", 400}, {"POST", strings.Repeat(" ", MaxAdmissionBytes+1), "application/json", 413},
		{"POST", `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{}}`, "application/json", 400},
	} {
		r := httptest.NewRequest(tc.method, "/mutate", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.content)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("got %d want %d", w.Code, tc.code)
		}
	}
}

func TestProviderPinnedParentOnlyAndNoop(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true", "gslb_strategy": "roundRobin", "other": "keep"})
	c := client(u)
	p := NewProvider(c, config())
	req := v1.MutationRequest{Provider: "traefik", Operation: "update", Resource: reference(u)}
	prepared, err := p.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Current.Object, prepared.Desired.Object) || len(prepared.Fields) == 0 {
		t.Fatal("no-op preparation is invalid")
	}
	if slices.Contains(prepared.Fields, "/metadata/annotations/other") {
		t.Fatal("unrelated annotation exposed")
	}
	req.Policy = &v1.Policy{Strategy: "failover", PrimaryGeoTag: "east", DNSTTLSeconds: 60}
	req.Traefik = &v1.TraefikOptions{MTLS: true, CommonNames: []string{"client.example.test"}}
	prepared, err = p.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes(c)) != 0 || prepared.Desired.GetAnnotations()["gslb_strategy"] != "failover" || prepared.Desired.GetAnnotations()[Prefix+"strategy"] != "" || prepared.Desired.GetAnnotations()["other"] != "keep" {
		t.Fatal("provider wrote or renamed legacy annotations")
	}
	if prepared.Desired.GetAnnotations()[OriginalEntrypoints] == "" {
		t.Fatal("provider did not normalize mTLS")
	}
	validator := p.(interface {
		ValidateAdmission(mutation.Prepared, *unstructured.Unstructured) error
	})
	if err := validator.ValidateAdmission(prepared, prepared.Desired); err != nil {
		t.Fatal(err)
	}
	unexpected := prepared.Desired.DeepCopy()
	mustSet(t, unexpected, "changed", "spec", "tls", "secretName")
	if validator.ValidateAdmission(prepared, unexpected) == nil {
		t.Fatal("unexpected admission change accepted")
	}
	req.Resource.UID = "recreated"
	if _, err := p.Prepare(context.Background(), req); err == nil {
		t.Fatal("UID was not pinned")
	}
	req.Resource = reference(u)
	req.Resource.ResourceVersion = "old"
	if _, err := p.Prepare(context.Background(), req); err == nil {
		t.Fatal("RV was not pinned")
	}
	if len(writes(c)) != 0 {
		t.Fatal("Prepare wrote to Kubernetes")
	}
}

func TestDeactivateLegacyMTLSWithExplicitEntrypoint(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true", "mtls": "true"})
	_ = unstructured.SetNestedStringSlice(u.Object, []string{"mtls"}, "spec", "entryPoints")
	c := client(u)
	p := NewProvider(c, config())
	req := v1.MutationRequest{Provider: "traefik", Operation: "deactivate", Resource: reference(u)}
	if _, err := p.Prepare(context.Background(), req); err == nil {
		t.Fatal("guessed legacy ordinary entrypoints")
	}
	req.Traefik = &v1.TraefikOptions{Entrypoint: "grpc"}
	prepared, err := p.Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, _ := unstructured.NestedStringSlice(prepared.Desired.Object, "spec", "entryPoints")
	if !reflect.DeepEqual(entries, []string{"grpc"}) || len(writes(c)) != 0 {
		t.Fatal("legacy deactivate failed")
	}
}

func TestChildOwnershipIsNotJustANameOrLabel(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true"})
	for _, alter := range []func(*unstructured.Unstructured){
		func(c *unstructured.Unstructured) { c.SetLabels(nil) },
		func(c *unstructured.Unstructured) {
			refs := c.GetOwnerReferences()
			refs[0].UID = "foreign"
			c.SetOwnerReferences(refs)
		},
		func(c *unstructured.Unstructured) {
			refs := c.GetOwnerReferences()
			flag := false
			refs[0].Controller = &flag
			c.SetOwnerReferences(refs)
		},
		func(c *unstructured.Unstructured) {
			refs := c.GetOwnerReferences()
			refs = append(refs, metav1.OwnerReference{UID: "independent"})
			c.SetOwnerReferences(refs)
		},
	} {
		child := newChild(u, "k8gb.io/v1beta1", "Gslb", childName(u, false))
		alter(child)
		if owned(child, u) {
			t.Fatal("accepted ambiguous owner")
		}
		c := client(u, child)
		r := Reconciler{Client: c, Config: config()}
		if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err == nil || len(writes(c)) != 0 {
			t.Fatal("ambiguous ownership was not refused before writes")
		}
	}
}

func mwc() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "MutatingWebhookConfiguration", "metadata": map[string]interface{}{"name": "adapter", "uid": "hook-uid", "resourceVersion": "3"}, "webhooks": []interface{}{map[string]interface{}{"name": "adapter.example.test", "sideEffects": "None", "failurePolicy": "Fail", "admissionReviewVersions": []interface{}{"v1"}, "namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{Prefix + "traefik-webhook": "true"}}, "rules": []interface{}{map[string]interface{}{"apiGroups": []interface{}{"traefik.io", "traefik.containo.us"}, "apiVersions": []interface{}{"v1alpha1"}, "resources": []interface{}{"ingressroutes"}, "operations": []interface{}{"CREATE", "UPDATE"}, "scope": "Namespaced"}}}}}}
}

func TestWebhookReadinessAndNamespaceOptIn(t *testing.T) {
	u := route(t, nil)
	hook := mwc()
	ns := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": "app", "uid": "namespace-uid", "resourceVersion": "4"}}}
	for _, enabled := range []bool{false, true} {
		if enabled {
			ns.SetLabels(map[string]string{Prefix + "traefik-webhook": "true"})
		}
		c := client(u, hook, ns)
		cfg := config()
		cfg.WebhookConfig = "adapter"
		p := NewProvider(c, cfg)
		prepared, err := p.Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "enable", Resource: reference(u)})
		if enabled && (err != nil || len(prepared.Dependencies) != 2) {
			t.Fatalf("readiness: %+v %v", prepared, err)
		}
		if !enabled && err == nil {
			t.Fatal("namespace without opt-in was accepted")
		}
		if len(writes(c)) != 0 {
			t.Fatal("readiness wrote")
		}
	}
	for _, field := range []string{"failurePolicy", "sideEffects"} {
		bad := hook.DeepCopy()
		bad.Object["webhooks"].([]interface{})[0].(map[string]interface{})[field] = "Ignore"
		c := client(u, bad, ns)
		cfg := config()
		cfg.WebhookConfig = "adapter"
		if _, err := NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "enable", Resource: reference(u)}); err == nil {
			t.Fatalf("accepted %s", field)
		}
	}
}

func TestConfigurationFingerprint(t *testing.T) {
	fingerprint := func(c Config) string {
		return NewProvider(nil, c).(interface{ ConfigurationFingerprint() string }).ConfigurationFingerprint()
	}
	if fingerprint(Config{}) != fingerprint(defaults(Config{})) {
		t.Fatal("defaults not normalized")
	}
	base := config()
	reordered := base
	reordered.AllowedGeoTags = []string{"west", "east"}
	if fingerprint(base) != fingerprint(reordered) {
		t.Fatal("geo set order changed fingerprint")
	}
	for _, change := range []func(*Config){func(c *Config) { c.Domain = "example.test" }, func(c *Config) { c.CNPlugin = "other" }, func(c *Config) { c.OrdinaryEntrypoints = []string{"https"} }, func(c *Config) { c.MTLSEntrypoint = "client-auth" }, func(c *Config) { c.WebhookConfig = "other" }} {
		changed := base
		change(&changed)
		if fingerprint(base) == fingerprint(changed) {
			t.Fatal("render config not bound")
		}
	}
}

func TestReconcilerPreservesManualStateAndCleansOnlyOwned(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true", "local": "true"})
	cfg := config()
	cfg.LocalDomain = "local.test"
	c := client(u)
	var deleteOptions []metav1.DeleteOptions
	r := Reconciler{Client: deleteRecorder{c, &deleteOptions}, Config: cfg}
	ctx := context.Background()
	if err := r.Reconcile(ctx, "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	name := childName(u, false)
	ing, err := c.Resource(ingressGVR).Namespace("app").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !owned(ing, u) || *ing.GetOwnerReferences()[0].BlockOwnerDeletion {
		t.Fatal("unsafe ownership")
	}
	firstIngressService(ing)["name"] = "backendqaz"
	a := ing.GetAnnotations()
	a[Prefix+"manualDisable"] = "true"
	a["custom.example/keep"] = "yes"
	ing.SetAnnotations(a)
	_, _ = c.Resource(ingressGVR).Namespace("app").Update(ctx, ing, metav1.UpdateOptions{})
	g, _ := c.Resource(gslbGVR).Namespace("app").Get(ctx, name, metav1.GetOptions{})
	g.SetAnnotations(map[string]string{"k8gb.io/override-ip": "192.0.2.4", Prefix + "operational-ip": "192.0.2.5"})
	mustSet(t, g, "preserve", "spec", "operatorExtra")
	mustSet(t, g, int64(120), "spec", "strategy", "splitBrainThresholdSeconds")
	mustSet(t, g, map[string]interface{}{"hosts": "api.example.test"}, "status")
	_, _ = c.Resource(gslbGVR).Namespace("app").Update(ctx, g, metav1.UpdateOptions{})
	u.SetResourceVersion("11")
	routes := u.Object["spec"].(map[string]interface{})["routes"].([]interface{})
	routes[0].(map[string]interface{})["services"].([]interface{})[0].(map[string]interface{})["name"] = "new-backend"
	u.SetAnnotations(map[string]string{"gslb": "true", "local": "true", "gslb_strategy": "failover", "geotag": "east"})
	gvr, _ := routeGVR(u.GetAPIVersion())
	_, _ = c.Resource(gvr).Namespace("app").Update(ctx, u, metav1.UpdateOptions{})
	c.ClearActions()
	if err := r.Reconcile(ctx, "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	ing, _ = c.Resource(ingressGVR).Namespace("app").Get(ctx, name, metav1.GetOptions{})
	if text(firstIngressService(ing), "name") != "new-backendqaz" || ing.GetAnnotations()["custom.example/keep"] != "yes" || ing.GetAnnotations()[Prefix+"manualDisable"] != "true" {
		t.Fatalf("manual state lost: %v", ing.Object)
	}
	g, _ = c.Resource(gslbGVR).Namespace("app").Get(ctx, name, metav1.GetOptions{})
	if g.GetAnnotations()["k8gb.io/override-ip"] != "192.0.2.4" || g.GetAnnotations()[Prefix+"operational-ip"] != "192.0.2.5" || text(g.Object["spec"].(map[string]interface{}), "operatorExtra") != "preserve" {
		t.Fatal("operational overrides lost")
	}
	if ttl, _, _ := unstructured.NestedInt64(g.Object, "spec", "strategy", "splitBrainThresholdSeconds"); ttl != 120 {
		t.Fatal("strategy extension lost")
	}
	if got, _, _ := unstructured.NestedString(g.Object, "status", "hosts"); got != "api.example.test" {
		t.Fatal("status overwritten")
	}
	for _, action := range writes(c) {
		if action.GetResource().Resource == "dnsendpoints" || action.GetResource().Resource == "ingressroutes" {
			t.Fatal("wrote outside child set")
		}
	}
	c.ClearActions()
	if err := r.Reconcile(ctx, "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	if len(writes(c)) != 0 {
		t.Fatal("reconcile is not idempotent")
	}
	// An independent resource at an optional child name must never be deleted.
	localName := childName(u, true)
	local, _ := c.Resource(ingressGVR).Namespace("app").Get(ctx, localName, metav1.GetOptions{})
	local.SetOwnerReferences(nil)
	local.SetLabels(nil)
	_, _ = c.Resource(ingressGVR).Namespace("app").Update(ctx, local, metav1.UpdateOptions{})
	u.SetAnnotations(map[string]string{"gslb": "false"})
	u.SetResourceVersion("12")
	_, _ = c.Resource(gvr).Namespace("app").Update(ctx, u, metav1.UpdateOptions{})
	c.ClearActions()
	if err := r.Reconcile(ctx, "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resource(ingressGVR).Namespace("app").Get(ctx, localName, metav1.GetOptions{}); err != nil {
		t.Fatal("deleted unowned child")
	}
	if _, err := c.Resource(gslbGVR).Namespace("app").Get(ctx, name, metav1.GetOptions{}); err == nil {
		t.Fatal("owned Gslb was not cleaned")
	}
	for _, a := range writes(c) {
		if a.GetVerb() != "delete" {
			t.Fatalf("false did not only delete: %s", a.GetVerb())
		}
	}
	if len(deleteOptions) == 0 {
		t.Fatal("no deletes captured")
	}
	for _, opts := range deleteOptions {
		pre := opts.Preconditions
		if pre == nil || pre.UID == nil || pre.ResourceVersion == nil {
			t.Fatal("missing delete preconditions")
		}
	}
}

func TestReconcilerResourceAndHostnameCollisions(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true"})
	independent := newChild(u, "k8gb.io/v1beta1", "Gslb", childName(u, false))
	independent.SetOwnerReferences(nil)
	independent.SetLabels(nil)
	c := client(u, independent)
	r := Reconciler{Client: c, Config: config()}
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected collision, got %v", err)
	}
	if len(writes(c)) != 0 {
		t.Fatal("wrote before preflight collision")
	}
	other := u.DeepCopy()
	other.SetName("other")
	other.SetUID("other-uid")
	c = client(u, other)
	r.Client = c
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err == nil {
		t.Fatal("accepted duplicate full hostname")
	}
	if len(writes(c)) != 0 {
		t.Fatal("wrote colliding host")
	}
	other.Object["spec"].(map[string]interface{})["routes"].([]interface{})[0].(map[string]interface{})["match"] = "Host(`api.other.test`)"
	c = client(u, other)
	r.Client = c
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal("first DNS labels must not collide:", err)
	}
	if childName(u, false) == childName(other, false) {
		t.Fatal("child names collide")
	}
}

func TestReconcilerFreshParentAndBoundedReads(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true"})
	c := client(u)
	gets := 0
	c.PrependReactor("get", "ingressroutes", func(a ktesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets > 1 {
			changed := u.DeepCopy()
			changed.SetResourceVersion("changed")
			return true, changed, nil
		}
		return false, nil, nil
	})
	r := Reconciler{Client: c, Config: config()}
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err == nil {
		t.Fatal("did not re-read parent")
	}
	if len(writes(c)) != 0 {
		t.Fatal("wrote after parent change")
	}
	c = client(u)
	lists := 0
	c.PrependReactor("list", "ingressroutes", func(a ktesting.Action) (bool, runtime.Object, error) {
		lists++
		return true, &unstructured.UnstructuredList{Object: map[string]interface{}{"metadata": map[string]interface{}{"continue": "again"}}}, nil
	})
	r.Client = c
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err == nil || lists != maxListPages {
		t.Fatalf("unbounded lists %d err=%v", lists, err)
	}
	if len(writes(c)) != 0 {
		t.Fatal("wrote after truncated read")
	}
}

func TestCNPluginAndLocalLifecycle(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true", "mtls": "true", "mtls_cn_check": "client-a, client-b", "local": "true"})
	cfg := config()
	cfg.LocalDomain = "local.test"
	bad := cfg
	bad.CNPlugin = ""
	if Candidate(*u, bad).Eligible {
		t.Fatal("CN without plugin accepted")
	}
	c := client(u)
	r := Reconciler{Client: c, Config: cfg}
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil {
		t.Fatal(err)
	}
	mwgvr, _ := routeGVR(u.GetAPIVersion())
	mwgvr.Resource = "middlewares"
	mw, err := c.Resource(mwgvr).Namespace("app").Get(context.Background(), childName(u, false), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plugin, _, _ := unstructured.NestedMap(mw.Object, "spec", "plugin")
	if _, ok := plugin[cfg.CNPlugin]; !ok || len(plugin) != 1 {
		t.Fatal("forced CN plugin")
	}
	if _, err := c.Resource(ingressGVR).Namespace("app").Get(context.Background(), childName(u, true), metav1.GetOptions{}); err == nil {
		t.Fatal("local mTLS child created")
	}
	// Missing parent is a GC concern, not an instruction for broad child deletes.
	gvr, _ := routeGVR(u.GetAPIVersion())
	_ = c.Resource(gvr).Namespace("app").Delete(context.Background(), u.GetName(), metav1.DeleteOptions{})
	c.ClearActions()
	if err := r.Reconcile(context.Background(), "app", "api", u.GetAPIVersion()); err != nil || len(writes(c)) != 0 {
		t.Fatalf("parent-delete cleanup wrote: %v", err)
	}
}

func TestSweepCleansOwnedChildrenAfterAnnotationsRemoved(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true"})
	c := client(u)
	cfg := config()
	cfg.AllowedNamespaces = []string{"app"}
	r := Reconciler{Client: c, Config: cfg}
	ctx := context.Background()
	if err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	u.SetAnnotations(nil)
	u.SetResourceVersion("11")
	gvr, _ := routeGVR(u.GetAPIVersion())
	_, _ = c.Resource(gvr).Namespace("app").Update(ctx, u, metav1.UpdateOptions{})
	c.ClearActions()
	if err := r.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(writes(c)) != 2 {
		t.Fatalf("expected owned ingress/Gslb deletes, got %v", writes(c))
	}
}

func TestConfigAndPolicyValidation(t *testing.T) {
	for _, cfg := range []Config{{Domain: "*.example.test"}, {CNPlugin: "forced/plugin"}, {OrdinaryEntrypoints: []string{"mtls"}}, {AllowedNamespaces: []string{"app", "app"}}, {AllowedGeoTags: []string{"east", "east"}}} {
		if ValidateConfig(cfg) == nil {
			t.Fatalf("accepted invalid config %+v", cfg)
		}
	}
	u := route(t, map[string]string{"gslb": "true", "gslb_strategy": "failover", "geotag": "unknown"})
	if Candidate(*u, config()).Eligible {
		t.Fatal("disallowed geo accepted")
	}
	cfg := config()
	cfg.Domain = "other.test"
	if !Candidate(*route(t, nil), cfg).Eligible {
		t.Fatal("creation catalog rejected a local host before explicit global hostname selection")
	}
	if Candidate(*route(t, map[string]string{"gslb": "true"}), cfg).Eligible {
		t.Fatal("enabled route silently inferred a global hostname")
	}
	for _, op := range []string{"create", "delete", "disable"} {
		_, err := NewProvider(client(u), config()).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: op, Resource: reference(u)})
		var typed *mutation.Error
		if !errors.As(err, &typed) {
			t.Fatal(fmt.Sprintf("unsupported %s: %v", op, err))
		}
	}
}
