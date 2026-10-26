package traefik

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestMultipleGlobalDomains(t *testing.T) {
	cfg := config()
	cfg.Domain = "apps.example.test"
	cfg.Domains = []string{"shared.example.test"}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"web.apps.example.test", "web.shared.example.test"} {
		parent := route(t, map[string]string{"gslb": "true", "gslb_hostname": host})
		s, err := settings(parent, defaults(cfg))
		if err != nil || s.target.host != host {
			t.Fatalf("%s: %v", host, err)
		}
		children := buildChildren(parent, s, defaults(cfg))
		found := false
		for _, ch := range children {
			if ch.desired != nil && ch.desired.GetKind() == "Ingress" && ch.name[:5] != "local" {
				rules, _, _ := unstructured.NestedSlice(ch.desired.Object, "spec", "rules")
				if len(rules) > 0 {
					rule, _ := rules[0].(map[string]interface{})
					found = rule["host"] == host
				}
			}
		}
		if !found {
			t.Fatalf("generated Ingress lost selected hostname: %s", host)
		}
	}
	parent := route(t, map[string]string{"gslb": "true", "gslb_hostname": "web.evil.test"})
	if _, err := settings(parent, defaults(cfg)); err == nil {
		t.Fatal("out-of-allowlist host")
	}
}
