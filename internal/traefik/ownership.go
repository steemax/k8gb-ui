package traefik

import (
	"context"
	"fmt"
	"strings"

	"k8gb-ui/internal/mutation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var legacyGslbGVR = schema.GroupVersionResource{Group: "k8gb.absa.oss", Version: "v1beta1", Resource: "gslbs"}

// Existing standalone or externally owned Gslbs must be handed over explicitly,
// never duplicated by enabling an IngressRoute with the same DNS hostname.
func (r Reconciler) uniqueGslbHosts(ctx context.Context, parent *unstructured.Unstructured, hosts map[string]bool) error {
	for _, gvr := range []schema.GroupVersionResource{gslbGVR, legacyGslbGVR} {
		items, err := r.listRoutes(ctx, parent.GetNamespace(), gvr)
		if err != nil {
			return fmt.Errorf("verify Gslb hostname ownership: %w", err)
		}
		for i := range items {
			gslb := &items[i]
			if owned(gslb, parent) {
				continue
			}
			names, err := r.gslbHostnames(ctx, gslb)
			if err != nil {
				return fmt.Errorf("verify Gslb %s/%s hostnames: %w", gslb.GetNamespace(), gslb.GetName(), err)
			}
			for _, name := range names {
				if hosts[strings.TrimSuffix(strings.ToLower(name), ".")] {
					return mutation.Failure(409, "hostname_owned", fmt.Sprintf("hostname %s is already managed by Gslb %s/%s; an explicit management handover is required", name, gslb.GetNamespace(), gslb.GetName()))
				}
			}
		}
	}
	return nil
}

func (r Reconciler) gslbHostnames(ctx context.Context, gslb *unstructured.Unstructured) ([]string, error) {
	var hosts []string
	addRules := func(u *unstructured.Unstructured, path ...string) {
		rules, _, _ := unstructured.NestedSlice(u.Object, path...)
		for _, raw := range rules {
			if rule, ok := raw.(map[string]interface{}); ok && text(rule, "host") != "" {
				hosts = append(hosts, text(rule, "host"))
			}
		}
	}
	addRules(gslb, "spec", "ingress", "rules")
	status, _, _ := unstructured.NestedFieldNoCopy(gslb.Object, "status", "hosts")
	switch value := status.(type) {
	case string:
		hosts = append(hosts, strings.FieldsFunc(value, func(c rune) bool { return c == ',' || c == ' ' })...)
	case []interface{}:
		for _, raw := range value {
			if name, ok := raw.(string); ok {
				hosts = append(hosts, name)
			}
		}
	}
	ref, found, err := unstructured.NestedMap(gslb.Object, "spec", "resourceRef")
	if err != nil || !found {
		return hosts, err
	}
	namespace := text(ref, "namespace")
	if namespace == "" {
		namespace = gslb.GetNamespace()
	}
	if namespace != gslb.GetNamespace() {
		return nil, fmt.Errorf("cross-namespace routing references are not supported")
	}
	var gvr schema.GroupVersionResource
	switch text(ref, "apiVersion") + "/" + text(ref, "kind") {
	case "networking.k8s.io/v1/Ingress":
		gvr = ingressGVR
	case "gateway.networking.k8s.io/v1/HTTPRoute":
		gvr = schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes"}
	default:
		return nil, fmt.Errorf("unsupported routing reference; cannot prove hostname ownership")
	}
	target, err := r.Client.Resource(gvr).Namespace(namespace).Get(ctx, text(ref, "name"), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if gvr == ingressGVR {
		addRules(target, "spec", "rules")
	} else {
		names, _, err := unstructured.NestedStringSlice(target.Object, "spec", "hostnames")
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, names...)
	}
	return hosts, nil
}
