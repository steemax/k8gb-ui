package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type provider struct {
	client dynamic.Interface
	config Config
}

func NewProvider(client dynamic.Interface, cfg Config) mutation.Provider {
	return &provider{client: client, config: defaults(cfg)}
}
func (*provider) Operations() []string {
	return []string{"enable", "update", "deactivate", "excludeSite", "includeSite"}
}

// Shared TLS policies may live outside application namespaces, but can only be
// read as pinned dependencies in explicitly approved policy namespaces.
func (p *provider) AllowsReadDependency(ref v1.ResourceReference) bool {
	return ref.Kind == "TLSOption" && (ref.APIVersion == "traefik.io/v1alpha1" || ref.APIVersion == "traefik.containo.us/v1alpha1") && slices.Contains(p.config.TLSOptionNamespaces, ref.Namespace)
}

// ConfigurationFingerprint is consumed through the mutation engine's optional
// interface. Set-valued lists are canonicalized; entrypoint order is meaningful.
func (p *provider) ConfigurationFingerprint() string {
	c := defaults(p.config)
	c.AllowedGeoTags = append([]string(nil), c.AllowedGeoTags...)
	c.AllowedNamespaces = append([]string(nil), c.AllowedNamespaces...)
	sort.Strings(c.AllowedGeoTags)
	sort.Strings(c.AllowedNamespaces)
	c.MTLSEntrypoints = slices.Clone(c.MTLSEntrypoints)
	c.TLSOptionNamespaces = slices.Clone(c.TLSOptionNamespaces)
	sort.Strings(c.MTLSEntrypoints)
	sort.Strings(c.TLSOptionNamespaces)
	encoded, _ := json.Marshal(c)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

// ValidateAdmission rejects unexpected edits by other mutating webhooks. Only
// API-server bookkeeping may differ from the already normalized desired parent.
func (p *provider) ValidateAdmission(prepared mutation.Prepared, admitted *unstructured.Unstructured) error {
	if admitted == nil || prepared.Desired == nil {
		return mutation.Failure(409, "unexpected_mutation", "missing admitted or desired parent")
	}
	expected, actual := prepared.Desired.DeepCopy(), admitted.DeepCopy()
	for _, key := range []string{"resourceVersion", "managedFields", "generation", "creationTimestamp", "selfLink"} {
		unstructured.RemoveNestedField(expected.Object, "metadata", key)
		unstructured.RemoveNestedField(actual.Object, "metadata", key)
	}
	if !reflect.DeepEqual(expected.Object, actual.Object) {
		return mutation.Failure(409, "unexpected_mutation", "admission changed fields outside the normalized parent plan; replan")
	}
	enabled, _ := flag(expected.GetAnnotations(), "gslb")
	excluded := expected.GetAnnotations()[SiteExcludedAnnotation] == "true"
	if enabled && excluded {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := checkExcludedServiceAbsent(ctx, p.client, expected); err != nil {
			return err
		}
	}
	return nil
}

func (p *provider) Prepare(ctx context.Context, request v1.MutationRequest) (mutation.Prepared, error) {
	var prepared mutation.Prepared
	if err := ValidateConfig(p.config); err != nil {
		return prepared, mutation.Failure(422, "invalid_config", err.Error())
	}
	if !slices.Contains(p.Operations(), request.Operation) {
		return prepared, mutation.Unsupported("traefik", request.Operation)
	}
	siteOperation := request.Operation == "excludeSite" || request.Operation == "includeSite"
	if request.Traffic != nil || siteOperation && (request.Policy != nil || request.Traefik != nil) {
		return prepared, mutation.Failure(422, "invalid_options", "site operations accept no policy, traefik or traffic options; other Traefik operations accept no traffic options")
	}
	if request.Provider != "traefik" || request.Target != nil {
		return prepared, mutation.Failure(422, "invalid_target", "traefik writes target only the pinned parent IngressRoute")
	}
	ref := request.Resource
	gvr, err := routeGVR(ref.APIVersion)
	if err != nil || ref.Kind != "IngressRoute" || ref.Name == "" || !namespaceAllowed(ref.Namespace, p.config) || ref.UID == "" || ref.ResourceVersion == "" {
		return prepared, mutation.Failure(422, "invalid_resource", "a supported namespaced IngressRoute pinned by UID and resourceVersion is required")
	}
	if p.client == nil {
		return prepared, mutation.Failure(503, "kubernetes_unavailable", "Kubernetes client is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	current, err := p.client.Resource(gvr).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return prepared, err
	}
	if string(current.GetUID()) != ref.UID || current.GetResourceVersion() != ref.ResourceVersion {
		return prepared, mutation.Failure(409, "resource_changed", "IngressRoute UID or resourceVersion changed; replan")
	}
	if current.GetDeletionTimestamp() != nil {
		return prepared, mutation.Failure(409, "resource_deleting", "IngressRoute is being deleted")
	}
	if _, err := siteExcluded(current.GetAnnotations()); err != nil {
		return prepared, invalid(err)
	}
	desired := current.DeepCopy()
	a := desired.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	if siteOperation {
		s, err := settings(current, p.config)
		if err != nil {
			return prepared, invalid(err)
		}
		if !s.enabled {
			return prepared, mutation.Failure(409, "not_enabled", "enable the IngressRoute before changing site traffic")
		}
		deps, err := p.siteTrafficDependencies(ctx, current, s.target)
		if err != nil {
			return prepared, err
		}
		prepared.Dependencies = append(prepared.Dependencies, deps...)
		if request.Operation == "excludeSite" {
			if err := checkExcludedServiceAbsent(ctx, p.client, current); err != nil {
				return prepared, err
			}
			a[SiteExcludedAnnotation] = "true"
		} else {
			delete(a, SiteExcludedAnnotation)
		}
	} else if request.Operation == "deactivate" {
		setAnnotation(a, "gslb", "false")
		setAnnotation(a, "mtls", "false")
		setAnnotation(a, "mtls_cn_check", "")
		if request.Traefik != nil && request.Traefik.Entrypoint != "" {
			setAnnotation(a, "entrypoint", request.Traefik.Entrypoint)
		}
	} else {
		enabled, err := flag(a, "gslb")
		if err != nil {
			return prepared, invalid(err)
		}
		if request.Operation == "update" && !enabled {
			return prepared, mutation.Failure(409, "not_enabled", "enable the IngressRoute before updating it")
		}
		setAnnotation(a, "gslb", "true")
		if request.Policy != nil {
			if err := validatePolicy(*request.Policy, p.config); err != nil {
				return prepared, invalid(err)
			}
			policy := request.Policy
			setAnnotation(a, "strategy", policy.Strategy)
			setAnnotation(a, "geotag", policy.PrimaryGeoTag)
			ttl := policy.DNSTTLSeconds
			if ttl == 0 {
				ttl = 60
			}
			setAnnotation(a, "dnsttl", strconv.FormatInt(ttl, 10))
			setAnnotation(a, "weight_enable", strconv.FormatBool(len(policy.Weights) > 0))
			encoded, _ := json.Marshal(policy.Weights)
			setAnnotation(a, "weights", "")
			if len(policy.Weights) > 0 {
				setAnnotation(a, "weights", string(encoded))
			}
		}
		if o := request.Traefik; o != nil {
			setAnnotation(a, "hostname", o.Hostname)
			setAnnotation(a, "mtls", strconv.FormatBool(o.MTLS))
			setAnnotation(a, "local", strconv.FormatBool(o.Local))
			setAnnotation(a, "mtls_cn_check", strings.Join(o.CommonNames, ","))
			setAnnotation(a, "entrypoint", o.Entrypoint)
			setAnnotation(a, "mtls_entrypoint", o.MTLSEntrypoint)
			setAnnotation(a, "mtls_tlsoption", "")
			if o.TLSOption != nil {
				setAnnotation(a, "mtls_tlsoption", o.TLSOption.Namespace+"/"+o.TLSOption.Name)
			}
		}
	}
	desired.SetAnnotations(a)
	desiredSettings, err := settings(desired, p.config)
	if err != nil {
		return prepared, invalid(err)
	}
	if desiredSettings.enabled && !siteOperation {
		dep, err := validateMTLS(ctx, p.client, desired, p.config)
		if err != nil {
			return prepared, invalid(err)
		}
		if dep != nil {
			prepared.Dependencies = append(prepared.Dependencies, *dep)
		}
		if err := (Reconciler{Client: p.client, Config: p.config}).uniqueHosts(ctx, desired, desiredSettings, p.config); err != nil {
			return prepared, err
		}
	}
	if !siteOperation {
		if err := normalizeParent(desired, current, p.config); err != nil {
			return prepared, invalid(err)
		}
	}
	if p.config.WebhookConfig != "" {
		deps, err := p.checkWebhook(ctx, desired, gvr)
		if err != nil {
			return prepared, mutation.Failure(503, "webhook_not_ready", err.Error())
		}
		prepared.Dependencies = append(prepared.Dependencies, deps...)
	}
	prepared.GVR, prepared.Current, prepared.Desired = gvr, current, desired
	if siteOperation {
		prepared.Fields = []string{"/metadata/annotations/" + pointerEscape(SiteExcludedAnnotation)}
		prepared.Warnings = []string{"Site traffic changes reconcile asynchronously on the owned global Ingress and Gslb only; local children and live parent routing are unchanged. DNS propagation and remote traffic withdrawal are not confirmed."}
		return prepared, nil
	}
	// This is a stable ownership allowlist, not a diff; no-op plans are valid.
	keys := map[string]bool{OriginalEntrypoints: true, OriginalTLSOptions: true}
	for key, spellings := range aliases {
		keys[Prefix+key] = true
		for _, spelling := range spellings {
			keys[spelling] = true
		}
	}
	for k := range keys {
		prepared.Fields = append(prepared.Fields, "/metadata/annotations/"+pointerEscape(k))
	}
	prepared.Fields = append(prepared.Fields, "/spec/entryPoints", "/spec/tls")
	sort.Strings(prepared.Fields)
	prepared.Warnings = []string{"Child resources are reconciled asynchronously after the parent commits; webhook registration is not a live endpoint health check."}
	return prepared, nil
}

func invalid(err error) error { return mutation.Failure(422, "invalid_traefik_policy", err.Error()) }
func pointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

func containsAny(values []string, choices ...string) bool {
	for _, choice := range choices {
		if slices.Contains(values, choice) {
			return true
		}
	}
	return false
}

// checkWebhook verifies coverage of UPDATE (the provider's operation) and CREATE.
// Namespace selectors are evaluated with a fresh Namespace read, never guessed.
func (p *provider) checkWebhook(ctx context.Context, route *unstructured.Unstructured, gvr schema.GroupVersionResource) ([]v1.ResourceReference, error) {
	mwcGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	u, err := p.client.Resource(mwcGVR).Get(ctx, p.config.WebhookConfig, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read named mutating webhook: %w", err)
	}
	deps := []v1.ResourceReference{reference(u)}
	hooks, _, _ := unstructured.NestedSlice(u.Object, "webhooks")
	if len(hooks) > 128 {
		return nil, fmt.Errorf("too many webhook entries")
	}
	var ns *unstructured.Unstructured
	for _, item := range hooks {
		hook, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		versions, _, _ := unstructured.NestedStringSlice(hook, "admissionReviewVersions")
		conditions, _, _ := unstructured.NestedSlice(hook, "matchConditions")
		if text(hook, "sideEffects") != "None" || text(hook, "failurePolicy") != "Fail" || !slices.Contains(versions, "v1") || len(conditions) != 0 {
			continue
		}
		matches := true
		for _, which := range []string{"namespaceSelector", "objectSelector"} {
			selectorMap, found, err := unstructured.NestedMap(hook, which)
			if err != nil {
				matches = false
				break
			}
			if !found || len(selectorMap) == 0 {
				continue
			}
			var selector metav1.LabelSelector
			if runtime.DefaultUnstructuredConverter.FromUnstructured(selectorMap, &selector) != nil {
				matches = false
				break
			}
			parsed, err := metav1.LabelSelectorAsSelector(&selector)
			if err != nil {
				matches = false
				break
			}
			actual := route.GetLabels()
			if which == "namespaceSelector" {
				if ns == nil {
					ns, err = p.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Get(ctx, route.GetNamespace(), metav1.GetOptions{})
					if err != nil {
						return nil, fmt.Errorf("check namespace opt-in: %w", err)
					}
					deps = append(deps, reference(ns))
				}
				actual = ns.GetLabels()
			}
			if !parsed.Matches(labels.Set(actual)) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		rules, _, _ := unstructured.NestedSlice(hook, "rules")
		covered := map[string]bool{}
		for _, item := range rules {
			rule, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			groups, _, _ := unstructured.NestedStringSlice(rule, "apiGroups")
			versions, _, _ := unstructured.NestedStringSlice(rule, "apiVersions")
			resources, _, _ := unstructured.NestedStringSlice(rule, "resources")
			ops, _, _ := unstructured.NestedStringSlice(rule, "operations")
			if text(rule, "scope") == "Cluster" || !containsAny(groups, "*", gvr.Group) || !containsAny(versions, "*", gvr.Version) || !containsAny(resources, "*", "*/*", "ingressroutes") {
				continue
			}
			for _, op := range []string{"CREATE", "UPDATE"} {
				if containsAny(ops, "*", op) {
					covered[op] = true
				}
			}
		}
		if covered["CREATE"] && covered["UPDATE"] {
			return deps, nil
		}
	}
	return nil, fmt.Errorf("named webhook must cover CREATE/UPDATE of this IngressRoute with sideEffects None, failurePolicy Fail, v1 reviews and matching namespace/object selectors")
}
