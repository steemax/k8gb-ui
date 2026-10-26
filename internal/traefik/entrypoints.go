package traefik

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func normalizeParent(route, old *unstructured.Unstructured, c Config) error {
	c = defaults(c)
	if err := normalizeEntrypoints(route, old, c); err != nil {
		return err
	}
	return normalizeTLSOptions(route, old, c)
}

func normalizeEntrypoints(route, old *unstructured.Unstructured, c Config) error {
	a := route.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	mtls, err := flag(a, "mtls")
	if err != nil {
		return err
	}
	oldMTLS := false
	oldSaved, oldHas := "", false
	if old != nil {
		oldMTLS, err = flag(old.GetAnnotations(), "mtls")
		if err != nil {
			return err
		}
		oldSaved, oldHas = old.GetAnnotations()[OriginalEntrypoints]
	}
	saved, has := a[OriginalEntrypoints]
	if oldHas {
		if has && saved != oldSaved {
			return fmt.Errorf("%s is managed and cannot be changed", OriginalEntrypoints)
		}
		saved, has = oldSaved, true
	}
	entries, present, err := unstructured.NestedStringSlice(route.Object, "spec", "entryPoints")
	if err != nil {
		return fmt.Errorf("spec.entryPoints must be a string array")
	}
	var original []string
	if has {
		if len(saved) > 4096 || json.Unmarshal([]byte(saved), &original) != nil {
			return fmt.Errorf("invalid original-entrypoints snapshot")
		}
		if err := validEntrypoints(original, c); err != nil {
			return err
		}
	}
	selected, _ := annotation(a, "mtls_entrypoint")
	if selected == "" {
		selected = c.MTLSEntrypoint
	}
	previous := c.MTLSEntrypoint
	if old != nil {
		if name, _ := annotation(old.GetAnnotations(), "mtls_entrypoint"); name != "" {
			previous = name
		}
	}
	ordinary := func(names []string) []string {
		var out []string
		if names != nil {
			out = []string{}
		}
		for _, name := range names {
			if !oldMTLS && c.MTLSEntrypoints != nil || name != previous {
				out = append(out, name)
			}
		}
		return out
	}
	if mtls {
		if !containsAny(MTLSEntrypoints(c), selected) {
			return fmt.Errorf("mTLS entrypoint is not in the approved installation allowlist")
		}
		if !has {
			before := entries
			if len(entries) > 0 && len(ordinary(entries)) == 0 && old != nil && !oldMTLS {
				before, _, err = unstructured.NestedStringSlice(old.Object, "spec", "entryPoints")
				if err != nil {
					return err
				}
			}
			original = ordinary(before)
			if err := validEntrypoints(original, c); err != nil {
				return err
			}
			// An existing mTLS-only route has no recoverable ordinary list.
			if len(before) == 0 || len(original) > 0 {
				encoded, _ := json.Marshal(original)
				if len(encoded) > 4096 {
					return fmt.Errorf("entrypoint snapshot too large")
				}
				saved, has = string(encoded), true
			}
		}
		if has {
			a[OriginalEntrypoints] = saved
			route.SetAnnotations(a)
		}
		return unstructured.SetNestedStringSlice(route.Object, []string{selected}, "spec", "entryPoints")
	}
	legacyOnly := len(entries) == 1 && entries[0] == previous
	if !has && !oldMTLS && !legacyOnly {
		return nil
	}
	// An explicit ordinary list is also the recovery path for legacy mTLS-only routes.
	if len(entries) > 0 && !(oldMTLS && len(entries) == 1 && entries[0] == previous) && validEntrypoints(entries, c) == nil {
		original = entries
	} else if !has {
		explicit, err := annotation(a, "entrypoint")
		if err != nil {
			return err
		}
		if explicit == "" {
			return fmt.Errorf("cannot disable legacy mTLS without saved entrypoints; supply ordinary spec.entryPoints or entrypoint explicitly")
		}
		original = strings.Split(explicit, ",")
		if err := validEntrypoints(original, c); err != nil {
			return err
		}
	}
	if original == nil {
		if present {
			unstructured.RemoveNestedField(route.Object, "spec", "entryPoints")
		}
	} else if !reflect.DeepEqual(entries, original) {
		if err := unstructured.SetNestedStringSlice(route.Object, original, "spec", "entryPoints"); err != nil {
			return err
		}
	}
	delete(a, OriginalEntrypoints)
	if len(a) > 0 {
		route.SetAnnotations(a)
	} else {
		unstructured.RemoveNestedField(route.Object, "metadata", "annotations")
	}
	return nil
}
