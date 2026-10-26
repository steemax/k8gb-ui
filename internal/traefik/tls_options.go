package traefik

import (
	"encoding/json"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type tlsOptionsSnapshot struct {
	TLSPresent    bool                   `json:"tlsPresent"`
	Options       map[string]interface{} `json:"options,omitempty"`
	Unrecoverable bool                   `json:"unrecoverable,omitempty"`
}

func selectedTLSOptions(route *unstructured.Unstructured, c Config) (map[string]interface{}, error) {
	value, err := annotation(route.GetAnnotations(), "mtls_tlsoption")
	if err != nil {
		return nil, err
	}
	if value == "" {
		value = c.MTLSTLSOption
	}
	ref, err := tlsOptionReference(value, route.GetNamespace(), c)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"name": ref.Name, "namespace": ref.Namespace}, nil
}

// Only TLS options are managed; server certificates, resolvers and domains stay
// untouched. The snapshot also distinguishes an absent tls block from tls: {}.
func normalizeTLSOptions(route, old *unstructured.Unstructured, c Config) error {
	a := route.GetAnnotations()
	mtls, err := flag(a, "mtls")
	if err != nil {
		return err
	}
	oldMTLS := false
	saved, has := a[OriginalTLSOptions]
	if old != nil {
		oldMTLS, _ = flag(old.GetAnnotations(), "mtls")
		if previous, found := old.GetAnnotations()[OriginalTLSOptions]; found {
			if has && saved != previous {
				return fmt.Errorf("%s is managed and cannot be changed", OriginalTLSOptions)
			}
			saved, has = previous, true
		}
	}
	var snapshot tlsOptionsSnapshot
	if has && (len(saved) > 4096 || json.Unmarshal([]byte(saved), &snapshot) != nil) {
		return fmt.Errorf("invalid original TLS options snapshot")
	}
	if mtls {
		selected, err := selectedTLSOptions(route, c)
		if err != nil {
			return err
		}
		if !has {
			_, snapshot.TLSPresent, err = unstructured.NestedMap(route.Object, "spec", "tls")
			if err != nil {
				return fmt.Errorf("spec.tls must be an object")
			}
			snapshot.Options, _, err = unstructured.NestedMap(route.Object, "spec", "tls", "options")
			if err != nil {
				return fmt.Errorf("spec.tls.options must be an object")
			}
			if oldMTLS && snapshot.Options != nil {
				previous, _ := selectedTLSOptions(old, c)
				if reflect.DeepEqual(previous, snapshot.Options) {
					snapshot.Unrecoverable = true
					snapshot.Options = nil
				}
			}
			raw, _ := json.Marshal(snapshot)
			if len(raw) > 4096 {
				return fmt.Errorf("TLS options snapshot too large")
			}
			saved, has = string(raw), true
		}
		if a == nil {
			a = map[string]string{}
		}
		a[OriginalTLSOptions] = saved
		route.SetAnnotations(a)
		return unstructured.SetNestedMap(route.Object, selected, "spec", "tls", "options")
	}
	if !has {
		return nil
	}
	if snapshot.Unrecoverable {
		if old == nil {
			return fmt.Errorf("legacy TLS recovery requires the previous IngressRoute")
		}
		current, _, _ := unstructured.NestedMap(route.Object, "spec", "tls", "options")
		previous, _, _ := unstructured.NestedMap(old.Object, "spec", "tls", "options")
		if current == nil || reflect.DeepEqual(current, previous) {
			return fmt.Errorf("cannot disable legacy mTLS without original TLS options; supply ordinary spec.tls.options explicitly")
		}
	} else if snapshot.Options != nil {
		if err := unstructured.SetNestedMap(route.Object, snapshot.Options, "spec", "tls", "options"); err != nil {
			return err
		}
	} else {
		unstructured.RemoveNestedField(route.Object, "spec", "tls", "options")
		remaining, _, _ := unstructured.NestedMap(route.Object, "spec", "tls")
		if !snapshot.TLSPresent && len(remaining) == 0 {
			unstructured.RemoveNestedField(route.Object, "spec", "tls")
		}
	}
	delete(a, OriginalTLSOptions)
	if len(a) == 0 {
		unstructured.RemoveNestedField(route.Object, "metadata", "annotations")
	} else {
		route.SetAnnotations(a)
	}
	return nil
}
