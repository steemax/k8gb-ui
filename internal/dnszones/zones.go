// Package dnszones reads only explicitly located controller configuration, never Secrets.
package dnszones

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
)

type Config struct{ Namespace, Deployment, Container string }
type Reader struct {
	Client dynamic.Interface
	Config Config
}

var Deployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

func (c Config) Validate() error {
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(c.Deployment)) != 0 || c.Container != "" && len(validation.IsDNS1123Label(c.Container)) != 0 {
		return fmt.Errorf("invalid k8gb controller locator")
	}
	return nil
}
func ValidName(s string) bool {
	return strings.Contains(s, ".") && s == strings.ToLower(s) && len(validation.IsDNS1123Subdomain(s)) == 0
}
func Contains(zone, host string) bool {
	return ValidName(zone) && ValidName(host) && strings.HasSuffix(host, "."+zone)
}

func Parse(raw string) ([]string, error) {
	if len(raw) > 32768 {
		return nil, fmt.Errorf("DNS_ZONES is too large")
	}
	zones := map[string]bool{}
	for _, entry := range strings.Split(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), ";")), ";") {
		parts := strings.Split(entry, ":")
		if len(parts) != 2 && len(parts) != 3 {
			return nil, fmt.Errorf("invalid DNS_ZONES entry")
		}
		parent, zone := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if !Contains(parent, zone) {
			return nil, fmt.Errorf("DNS_ZONES requires valid parent and child zone names")
		}
		if len(parts) == 3 {
			ttl, e := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 32)
			if e != nil || ttl < 0 {
				return nil, fmt.Errorf("invalid DNS_ZONES negative TTL")
			}
		}
		zones[zone] = true
		if len(zones) > 128 {
			return nil, fmt.Errorf("too many DNS zones")
		}
	}
	result := make([]string, 0, len(zones))
	for z := range zones {
		result = append(result, z)
	}
	sort.Strings(result)
	return result, nil
}

func (r *Reader) Matches(ref v1.ResourceReference) bool {
	return r != nil && ref.APIVersion == "apps/v1" && ref.Kind == "Deployment" && ref.Namespace == r.Config.Namespace && ref.Name == r.Config.Deployment
}

func (r *Reader) Read(ctx context.Context) (result v1.DNSZoneCatalog, ref v1.ResourceReference, err error) {
	result.Zones = []string{}
	if r == nil {
		return result, ref, fmt.Errorf("DNS zone discovery is not configured")
	}
	result.Source = "Deployment " + r.Config.Namespace + "/" + r.Config.Deployment + " / DNS_ZONES"
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	if err = r.Config.Validate(); err != nil {
		return
	}
	if r.Client == nil {
		err = fmt.Errorf("Kubernetes client unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var obj *unstructured.Unstructured
	obj, err = r.Client.Resource(Deployments).Namespace(r.Config.Namespace).Get(ctx, r.Config.Deployment, metav1.GetOptions{})
	if err != nil {
		err = fmt.Errorf("read controller Deployment: %w", err)
		return
	}
	if obj.GetDeletionTimestamp() != nil {
		err = fmt.Errorf("controller Deployment is deleting")
		return
	}
	ref = v1.ResourceReference{APIVersion: "apps/v1", Kind: "Deployment", Namespace: r.Config.Namespace, Name: r.Config.Deployment, UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion()}
	containers, _, e := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if e != nil {
		err = e
		return
	}
	matches := 0
	for _, raw := range containers {
		c, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(c, "name")
		if r.Config.Container != "" && name != r.Config.Container {
			continue
		}
		env, _, _ := unstructured.NestedSlice(c, "env")
		values := map[string]string{}
		seen := map[string]bool{}
		unresolved := false
		for _, raw := range env {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			key, _, _ := unstructured.NestedString(item, "name")
			if key != "DNS_ZONES" && key != "DNS_ZONE" && key != "EDGE_DNS_ZONE" {
				continue
			}
			if seen[key] {
				err = fmt.Errorf("duplicate DNS zone environment variable")
				return
			}
			seen[key] = true
			if _, ok := item["valueFrom"]; ok {
				unresolved = true
			}
			values[key], _, _ = unstructured.NestedString(item, "value")
		}
		if !seen["DNS_ZONES"] && !seen["DNS_ZONE"] {
			continue
		}
		matches++
		if matches > 1 {
			err = fmt.Errorf("multiple containers declare DNS zones; configure --k8gb-container")
			return
		}
		if unresolved {
			err = fmt.Errorf("DNS zone valueFrom is not resolved; Secrets and ConfigMaps are not read")
			return
		}
		value := values["DNS_ZONES"]
		if value == "" {
			value = values["EDGE_DNS_ZONE"] + ":" + values["DNS_ZONE"]
		}
		result.Zones, err = Parse(value)
		if err != nil {
			return
		}
	}
	if matches != 1 {
		err = fmt.Errorf("no literal DNS_ZONES or DNS_ZONE/EDGE_DNS_ZONE found in controller")
		return
	}
	result.Available = true
	return
}
