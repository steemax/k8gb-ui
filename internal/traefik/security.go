package traefik

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

var sanPattern = regexp.MustCompile(`^[A-Za-z0-9*.-]+$`)

func MTLSEntrypoints(cfg Config) []string {
	c := defaults(cfg)
	if c.MTLSEntrypoints != nil {
		return append([]string{}, c.MTLSEntrypoints...)
	}
	if c.MTLSTLSOption != "" {
		return []string{c.MTLSEntrypoint}
	}
	return []string{}
}

func tlsNamespaces(namespace string, c Config) []string {
	if len(c.TLSOptionNamespaces) > 0 {
		return c.TLSOptionNamespaces
	}
	return []string{namespace}
}

func tlsOptionReference(value, namespace string, c Config) (v1.TLSOptionReference, error) {
	var ref v1.TLSOptionReference
	if strings.HasSuffix(value, "@kubernetescrd") {
		name := strings.TrimSuffix(value, "@kubernetescrd")
		for _, ns := range tlsNamespaces(namespace, c) {
			if strings.HasPrefix(name, ns+"-") {
				if ref.Name != "" {
					return ref, fmt.Errorf("ambiguous legacy TLSOption reference; use namespace/name")
				}
				ref = v1.TLSOptionReference{Namespace: ns, Name: strings.TrimPrefix(name, ns+"-")}
			}
		}
	} else {
		ref.Namespace, ref.Name, _ = strings.Cut(value, "/")
	}
	if len(validation.IsDNS1123Label(ref.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(ref.Name)) != 0 || !slices.Contains(tlsNamespaces(namespace, c), ref.Namespace) {
		return ref, fmt.Errorf("TLSOption must be namespace/name in an approved namespace; file-provider and ambiguous references are not supported")
	}
	return ref, nil
}

func tlsOptionGVR(apiVersion string) schema.GroupVersionResource {
	group := strings.TrimSuffix(apiVersion, "/v1alpha1")
	return schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "tlsoptions"}
}

func inspectTLSOption(u *unstructured.Unstructured) v1.TraefikTLSOption {
	result := v1.TraefikTLSOption{Resource: v1.ResourceReference{APIVersion: u.GetAPIVersion(), Kind: "TLSOption", Namespace: u.GetNamespace(), Name: u.GetName(), UID: string(u.GetUID()), ResourceVersion: u.GetResourceVersion()}, CASecretNames: []string{}}
	result.ClientAuthType, _, _ = unstructured.NestedString(u.Object, "spec", "clientAuth", "clientAuthType")
	names, _, err := unstructured.NestedStringSlice(u.Object, "spec", "clientAuth", "secretNames")
	result.CASecretNames = append(result.CASecretNames, names...)
	switch {
	case u.GetDeletionTimestamp() != nil:
		result.Reason = "TLSOption is being deleted"
	case result.ClientAuthType != "RequireAndVerifyClientCert":
		result.Reason = "Requires RequireAndVerifyClientCert"
	case err != nil || len(names) == 0 || len(names) > 32:
		result.Reason = "Requires CA Secret references"
	case u.GetUID() == "" || u.GetResourceVersion() == "":
		result.Reason = "TLSOption identity is not available"
	default:
		seen := map[string]bool{}
		for _, name := range names {
			if len(validation.IsDNS1123Subdomain(name)) != 0 || seen[name] {
				result.Reason = "Invalid or repeated CA Secret reference"
				return result
			}
			seen[name] = true
		}
		result.Eligible = true
	}
	return result
}

// SecurityCatalog reads policies and CA Secret NAMES only. EntryPoints and plugin
// alias are operator-approved installation settings, not runtime health claims.
func SecurityCatalog(ctx context.Context, client dynamic.Interface, namespace string, cfg Config) v1.TraefikSecurityCatalog {
	c := defaults(cfg)
	result := v1.TraefikSecurityCatalog{Entrypoints: MTLSEntrypoints(c), OrdinaryEntrypoints: append([]string{}, c.OrdinaryEntrypoints...), TLSOptions: []v1.TraefikTLSOption{}, PluginAlias: c.CNPlugin, SANFilterConfigured: c.CNPlugin != "", Warnings: []string{}}
	if len(result.Entrypoints) == 0 {
		result.Warnings = append(result.Warnings, "mTLS entrypoint allowlist is not configured")
		return result
	}
	if client == nil {
		result.Warnings = append(result.Warnings, "TLSOption discovery unavailable")
		return result
	}
	for _, group := range []string{"traefik.io", "traefik.containo.us"} {
		for _, ns := range tlsNamespaces(namespace, c) {
			gvr := schema.GroupVersionResource{Group: group, Version: "v1alpha1", Resource: "tlsoptions"}
			next := ""
			for page := 0; page < 16; page++ {
				items, err := client.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 256, Continue: next})
				if err != nil {
					if !apierrors.IsNotFound(err) {
						result.Warnings = append(result.Warnings, fmt.Sprintf("TLSOption discovery unavailable: %s in %s", group, ns))
					}
					break
				}
				for i := range items.Items {
					result.TLSOptions = append(result.TLSOptions, inspectTLSOption(&items.Items[i]))
				}
				next = items.GetContinue()
				if next == "" {
					break
				}
				if page == 15 {
					result.Warnings = append(result.Warnings, "TLSOption catalog limit reached")
				}
			}
		}
	}
	sort.Slice(result.TLSOptions, func(i, j int) bool {
		a, b := result.TLSOptions[i].Resource, result.TLSOptions[j].Resource
		return a.Namespace+"/"+a.Name+"/"+a.APIVersion < b.Namespace+"/"+b.Name+"/"+b.APIVersion
	})
	if c.CNPlugin != "" {
		result.Warnings = append(result.Warnings, "certauthz alias is configured; Traefik plugin runtime loading is not verified")
	}
	return result
}

func validateMTLS(ctx context.Context, client dynamic.Interface, route *unstructured.Unstructured, c Config) (*v1.ResourceReference, error) {
	s, err := settings(route, c)
	if err != nil {
		return nil, err
	}
	if !s.options.MTLS {
		return nil, nil
	}
	if s.options.TLSOption == nil || !slices.Contains(MTLSEntrypoints(c), s.options.MTLSEntrypoint) {
		return nil, fmt.Errorf("mTLS requires an approved entrypoint and TLSOption")
	}
	if client == nil {
		return nil, fmt.Errorf("TLSOption validation requires Kubernetes read access")
	}
	ref := s.options.TLSOption
	u, err := client.Resource(tlsOptionGVR(route.GetAPIVersion())).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("selected TLSOption %s/%s is unavailable", ref.Namespace, ref.Name)
	}
	if u.GetAPIVersion() != route.GetAPIVersion() || u.GetKind() != "TLSOption" || u.GetNamespace() != ref.Namespace || u.GetName() != ref.Name {
		return nil, fmt.Errorf("TLSOption response identity does not match the selected policy")
	}
	policy := inspectTLSOption(u)
	if !policy.Eligible {
		return nil, fmt.Errorf("selected TLSOption %s/%s: %s", ref.Namespace, ref.Name, policy.Reason)
	}
	return &policy.Resource, nil
}
