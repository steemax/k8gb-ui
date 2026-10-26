package traefik

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var ingressGVR = schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"}
var gslbGVR = schema.GroupVersionResource{Group: "k8gb.io", Version: "v1beta1", Resource: "gslbs"}

type Reconciler struct {
	Client dynamic.Interface
	Config Config
}
type child struct {
	gvr              schema.GroupVersionResource
	name             string
	desired, current *unstructured.Unstructured
}

// Reconcile reads a committed parent; it is never called from admission. Missing
// or deleting parents are left to Kubernetes owner-reference garbage collection.
func (r Reconciler) Reconcile(ctx context.Context, namespace, name, apiVersion string) error {
	c := defaults(r.Config)
	if err := ValidateConfig(c); err != nil {
		return err
	}
	if r.Client == nil {
		return fmt.Errorf("Kubernetes client is required")
	}
	if !namespaceAllowed(namespace, c) {
		return fmt.Errorf("namespace is not allowed")
	}
	gvr, err := routeGVR(apiVersion)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	parent, err := r.Client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if parent.GetDeletionTimestamp() != nil {
		return nil
	}
	if parent.GetUID() == "" || parent.GetResourceVersion() == "" {
		return fmt.Errorf("committed parent UID and resourceVersion required")
	}
	var s routeSettings
	if _, err := siteExcluded(parent.GetAnnotations()); err != nil {
		return err
	}
	enabled, err := flag(parent.GetAnnotations(), "gslb")
	if err != nil {
		return err
	}
	if enabled {
		s, err = settings(parent, c)
		if err != nil {
			return err
		}
	}
	if s.enabled {
		if _, err := validateMTLS(ctx, r.Client, parent, c); err != nil {
			return err
		}
		if err := r.uniqueHosts(ctx, parent, s, c); err != nil {
			return err
		}
	}
	children := buildChildren(parent, s, c)
	if s.excluded {
		if err := checkExcludedServiceAbsent(ctx, r.Client, parent); err != nil {
			return err
		}
	}
	// Preflight the whole bounded set before any writes, so a native independent
	// Gslb cannot be silently adopted after an Ingress was already created.
	for i := range children {
		ch := &children[i]
		ch.current, err = r.Client.Resource(ch.gvr).Namespace(namespace).Get(ctx, ch.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			ch.current = nil
			continue
		}
		if err != nil {
			return err
		}
		if ch.desired != nil && !owned(ch.current, parent) {
			return fmt.Errorf("resource collision: %s %s is not owned by parent UID %s", ch.gvr.Resource, ch.name, parent.GetUID())
		}
	}
	for _, ch := range children {
		if ch.desired == nil && (ch.current == nil || !owned(ch.current, parent)) {
			continue
		}
		var updated *unstructured.Unstructured
		if ch.desired != nil && ch.current != nil {
			updated = mergeChild(ch.current, ch.desired)
			if reflect.DeepEqual(updated.Object, ch.current.Object) {
				continue
			}
		}
		// No cross-resource transaction exists in Kubernetes. Rechecking the parent
		// before each CAS write bounds the race; later sweeps converge after changes.
		fresh, err := r.Client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if fresh.GetUID() != parent.GetUID() || fresh.GetResourceVersion() != parent.GetResourceVersion() || fresh.GetDeletionTimestamp() != nil {
			return fmt.Errorf("parent changed during reconciliation; retry next sweep")
		}
		if s.excluded {
			// Absence cannot be pinned by UID/RV; recheck before each side effect.
			if err := checkExcludedServiceAbsent(ctx, r.Client, parent); err != nil {
				return err
			}
		}
		api := r.Client.Resource(ch.gvr).Namespace(namespace)
		switch {
		case ch.desired == nil:
			uid, rv := ch.current.GetUID(), ch.current.GetResourceVersion()
			err = api.Delete(ctx, ch.name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}})
		case ch.current == nil:
			_, err = api.Create(ctx, ch.desired, metav1.CreateOptions{})
		default:
			_, err = api.Update(ctx, updated, metav1.UpdateOptions{})
		}
		if err != nil {
			return fmt.Errorf("reconcile %s %s: %w", ch.gvr.Resource, ch.name, err)
		}
	}
	return nil
}

func owned(child, parent *unstructured.Unstructured) bool {
	if child.GetNamespace() != parent.GetNamespace() || child.GetLabels()[ManagedLabel] != manager || child.GetLabels()[OwnerLabel] != string(parent.GetUID()) {
		return false
	}
	refs := child.GetOwnerReferences()
	// Reject additional owners, including independent non-controller owners.
	return len(refs) == 1 && refs[0].UID == parent.GetUID() && refs[0].APIVersion == parent.GetAPIVersion() && refs[0].Kind == "IngressRoute" && refs[0].Name == parent.GetName() && refs[0].Controller != nil && *refs[0].Controller
}

func childName(parent *unstructured.Unstructured, local bool) string {
	hash := sha256.Sum256([]byte(parent.GetAPIVersion() + "/" + parent.GetNamespace() + "/" + parent.GetName() + "/" + string(parent.GetUID())))
	stem := parent.GetName()
	if len(stem) > 30 {
		stem = stem[:30]
	}
	stem = strings.TrimRight(stem, ".-")
	prefix := "gslb-"
	if local {
		prefix = "local-gslb-"
	}
	return prefix + stem + "-" + hex.EncodeToString(hash[:8])
}

func newChild(parent *unstructured.Unstructured, apiVersion, kind, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]interface{}{"name": name, "namespace": parent.GetNamespace()}, "spec": map[string]interface{}{}}}
	u.SetLabels(map[string]string{ManagedLabel: manager, OwnerLabel: string(parent.GetUID())})
	controller, block := true, false
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: parent.GetAPIVersion(), Kind: "IngressRoute", Name: parent.GetName(), UID: parent.GetUID(), Controller: &controller, BlockOwnerDeletion: &block}})
	return u
}

func buildChildren(parent *unstructured.Unstructured, s routeSettings, c Config) []child {
	name, localName := childName(parent, false), childName(parent, true)
	mwGVR, _ := routeGVR(parent.GetAPIVersion())
	mwGVR.Resource = "middlewares"
	children := []child{{gvr: mwGVR, name: name}, {gvr: ingressGVR, name: name}, {gvr: gslbGVR, name: name}, {gvr: ingressGVR, name: localName}, {gvr: gslbGVR, name: localName}}
	if !s.enabled {
		return children
	}
	if len(s.options.CommonNames) > 0 {
		mw := newChild(parent, parent.GetAPIVersion(), "Middleware", name)
		_ = unstructured.SetNestedMap(mw.Object, map[string]interface{}{c.CNPlugin: map[string]interface{}{"domains": stringValues(s.options.CommonNames)}}, "spec", "plugin")
		children[0].desired = mw
	}
	children[1].desired = makeIngress(parent, name, s.target.host, s, c, false)
	children[2].desired = makeGslb(parent, name, s)
	if s.options.Local && !s.options.MTLS {
		children[3].desired = makeIngress(parent, localName, s.target.localHost, s, c, true)
		children[4].desired = makeGslb(parent, localName, s)
	}
	return children
}

func stringValues(values []string) []interface{} {
	out := make([]interface{}, len(values))
	for i, s := range values {
		out[i] = s
	}
	return out
}

const routerPrefix = "traefik.ingress.kubernetes.io/router."

var managedRouterAnnotations = []string{routerPrefix + "entrypoints", routerPrefix + "tls", routerPrefix + "tls.options", routerPrefix + "middlewares"}

func makeIngress(parent *unstructured.Unstructured, name, host string, s routeSettings, c Config, local bool) *unstructured.Unstructured {
	u := newChild(parent, "networking.k8s.io/v1", "Ingress", name)
	entries := c.OrdinaryEntrypoints
	if s.options.Entrypoint != "" {
		entries = strings.Split(s.options.Entrypoint, ",")
	}
	a := map[string]string{routerPrefix + "entrypoints": strings.Join(entries, ",")}
	if s.options.MTLS && !local {
		a[routerPrefix+"entrypoints"] = s.options.MTLSEntrypoint
		a[routerPrefix+"tls"] = "true"
		a[routerPrefix+"tls.options"] = s.options.TLSOption.Namespace + "-" + s.options.TLSOption.Name + "@kubernetescrd"
		if len(s.options.CommonNames) > 0 {
			a[routerPrefix+"middlewares"] = parent.GetNamespace() + "-" + name + "@kubernetescrd"
		}
	}
	service := s.target.service
	if s.excluded && !local {
		a[SiteExcludedAnnotation] = "true"
		service = ExcludedServiceName(string(parent.GetUID()))
	}
	u.SetAnnotations(a)
	spec := map[string]interface{}{"rules": []interface{}{map[string]interface{}{"host": host, "http": map[string]interface{}{"paths": []interface{}{map[string]interface{}{"path": "/", "pathType": "Prefix", "backend": map[string]interface{}{"service": map[string]interface{}{"name": service, "port": map[string]interface{}{"number": s.target.port}}}}}}}}}
	if c.IngressClass != "" {
		spec["ingressClassName"] = c.IngressClass
	}
	if s.options.MTLS && !local {
		spec["tls"] = []interface{}{map[string]interface{}{"hosts": []interface{}{host}}}
	}
	u.Object["spec"] = spec
	return u
}

func makeGslb(parent *unstructured.Unstructured, name string, s routeSettings) *unstructured.Unstructured {
	u := newChild(parent, "k8gb.io/v1beta1", "Gslb", name)
	if s.excluded && name == childName(parent, false) {
		u.SetAnnotations(map[string]string{SiteExcludedAnnotation: "true"})
	}
	strategy := map[string]interface{}{"type": s.policy.Strategy, "dnsTtlSeconds": s.policy.DNSTTLSeconds}
	if s.policy.PrimaryGeoTag != "" {
		strategy["primaryGeoTag"] = s.policy.PrimaryGeoTag
	}
	if len(s.policy.Weights) > 0 {
		weights := map[string]interface{}{}
		for geo, weight := range s.policy.Weights {
			weights[geo] = weight
		}
		strategy["weight"] = weights
	}
	u.Object["spec"] = map[string]interface{}{"resourceRef": map[string]interface{}{"apiVersion": "networking.k8s.io/v1", "kind": "Ingress", "name": name}, "strategy": strategy}
	return u
}

func mergeChild(current, desired *unstructured.Unstructured) *unstructured.Unstructured {
	u := current.DeepCopy()
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range desired.GetLabels() {
		labels[k] = v
	}
	u.SetLabels(labels)
	if desired.GetKind() == "Gslb" || desired.GetKind() == "Ingress" {
		a := u.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		delete(a, SiteExcludedAnnotation)
		if desired.GetAnnotations()[SiteExcludedAnnotation] == "true" {
			a[SiteExcludedAnnotation] = "true"
		}
		if len(a) > 0 {
			u.SetAnnotations(a)
		} else {
			unstructured.RemoveNestedField(u.Object, "metadata", "annotations")
		}
	}
	var fields [][]string
	if desired.GetKind() == "Gslb" {
		// Preserve operational IP overrides, legacy k8gb.io annotations, status,
		// ingress metadata and strategy extensions such as split-brain thresholds.
		fields = [][]string{{"spec", "resourceRef"}, {"spec", "strategy", "type"}, {"spec", "strategy", "primaryGeoTag"}, {"spec", "strategy", "dnsTtlSeconds"}, {"spec", "strategy", "weight"}}
	} else if desired.GetKind() == "Ingress" {
		// Router fields and the exclusion marker are managed; other keys survive.
		a := u.GetAnnotations()
		if a == nil {
			a = map[string]string{}
		}
		for _, k := range managedRouterAnnotations {
			delete(a, k)
		}
		for k, v := range desired.GetAnnotations() {
			a[k] = v
		}
		u.SetAnnotations(a)
		fields = [][]string{{"spec", "rules"}, {"spec", "ingressClassName"}, {"spec", "tls"}}
	} else {
		fields = [][]string{{"spec", "plugin"}}
	}
	for _, path := range fields {
		value, found, _ := unstructured.NestedFieldCopy(desired.Object, path...)
		if found {
			_ = unstructured.SetNestedField(u.Object, value, path...)
		} else {
			unstructured.RemoveNestedField(u.Object, path...)
		}
	}
	if desired.GetKind() == "Ingress" {
		// Legacy manual Disable changes the first backend, not the hostname.
		if oldService := firstIngressService(current); oldService != nil && current.GetAnnotations()[SiteExcludedAnnotation] != "true" && desired.GetAnnotations()[SiteExcludedAnnotation] != "true" && strings.HasSuffix(text(oldService, "name"), "qaz") {
			if service := firstIngressService(u); service != nil && !strings.HasSuffix(text(service, "name"), "qaz") {
				service["name"] = text(service, "name") + "qaz"
			}
		}
	}
	return u
}

func firstIngressService(u *unstructured.Unstructured) map[string]interface{} {
	spec, ok := u.Object["spec"].(map[string]interface{})
	if !ok {
		return nil
	}
	rules, ok := spec["rules"].([]interface{})
	if !ok || len(rules) == 0 {
		return nil
	}
	rule, ok := rules[0].(map[string]interface{})
	if !ok {
		return nil
	}
	http, ok := rule["http"].(map[string]interface{})
	if !ok {
		return nil
	}
	paths, ok := http["paths"].([]interface{})
	if !ok || len(paths) == 0 {
		return nil
	}
	path, ok := paths[0].(map[string]interface{})
	if !ok {
		return nil
	}
	backend, ok := path["backend"].(map[string]interface{})
	if !ok {
		return nil
	}
	service, _ := backend["service"].(map[string]interface{})
	return service
}

const listPageSize int64 = 200
const maxListPages = 10

func (r Reconciler) listRoutes(ctx context.Context, ns string, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	var items []unstructured.Unstructured
	next := ""
	timeout := int64(5)
	for page := 0; page < maxListPages; page++ {
		list, err := r.Client.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{Limit: listPageSize, Continue: next, TimeoutSeconds: &timeout})
		if apierrors.IsNotFound(err) {
			return items, nil
		}
		if err != nil {
			return nil, err
		}
		if len(items)+len(list.Items) > int(listPageSize)*maxListPages {
			return nil, fmt.Errorf("IngressRoute read limit exceeded")
		}
		items = append(items, list.Items...)
		next = list.GetContinue()
		if next == "" {
			return items, nil
		}
	}
	return nil, fmt.Errorf("IngressRoute pagination limit exceeded")
}

func (r Reconciler) uniqueHosts(ctx context.Context, parent *unstructured.Unstructured, s routeSettings, c Config) error {
	hosts := map[string]bool{s.target.host: true}
	if s.options.Local && !s.options.MTLS {
		hosts[s.target.localHost] = true
	}
	for _, api := range []string{"traefik.io/v1alpha1", "traefik.containo.us/v1alpha1"} {
		gvr, _ := routeGVR(api)
		items, err := r.listRoutes(ctx, parent.GetNamespace(), gvr)
		if err != nil {
			return err
		}
		for i := range items {
			u := &items[i]
			if u.GetUID() == parent.GetUID() || u.GetDeletionTimestamp() != nil {
				continue
			}
			other, err := settings(u, c)
			if err != nil || !other.enabled {
				continue
			}
			if hosts[other.target.host] || (other.options.Local && !other.options.MTLS && hosts[other.target.localHost]) {
				return fmt.Errorf("hostname collision with IngressRoute %s/%s", u.GetNamespace(), u.GetName())
			}
		}
	}
	return r.uniqueGslbHosts(ctx, parent, hosts)
}

// Sweep performs a bounded pass in explicitly allowed namespaces. No cluster-wide
// list, watch, database, child discovery or broad name-based cleanup is used.
func (r Reconciler) Sweep(ctx context.Context) error {
	if err := ValidateConfig(r.Config); err != nil {
		return err
	}
	if r.Client == nil || len(r.Config.AllowedNamespaces) == 0 {
		return fmt.Errorf("sweeps require client and explicit allowed namespaces")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var first error
	for _, ns := range r.Config.AllowedNamespaces {
		for _, api := range []string{"traefik.io/v1alpha1", "traefik.containo.us/v1alpha1"} {
			gvr, _ := routeGVR(api)
			readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			items, err := r.listRoutes(readCtx, ns, gvr)
			cancel()
			if err != nil {
				if first == nil {
					first = err
				}
				continue
			}
			for i := range items {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if _, err := annotation(items[i].GetAnnotations(), "gslb"); err != nil {
					if first == nil {
						first = err
					}
					continue
				}
				// Also reconcile absent flags: removing annotations must not orphan
				// owned children. Unrelated routes only incur bounded identity reads.
				if err := r.Reconcile(ctx, ns, items[i].GetName(), api); err != nil && first == nil {
					first = fmt.Errorf("%s/%s: %w", ns, items[i].GetName(), err)
				}
			}
		}
	}
	return first
}
