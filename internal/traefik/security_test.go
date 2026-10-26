package traefik

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

func secureConfig() Config {
	return defaults(Config{MTLSEntrypoints: []string{"secure-client", "websecure"}, OrdinaryEntrypoints: []string{"websecure", "grpc"}, AllowedGeoTags: []string{"east", "west"}})
}

func secureAnnotations(entry string) map[string]string {
	return map[string]string{"gslb": "true", Prefix + "mtls": "true", Prefix + "mtls_entrypoint": entry, Prefix + "mtls_tlsoption": "app/client-auth"}
}

func TestSelectableMTLSAndSharedEntrypointRoundTrip(t *testing.T) {
	for _, entry := range []string{"secure-client", "websecure"} {
		t.Run(entry, func(t *testing.T) {
			old := route(t, map[string]string{"gslb": "true"})
			mustSet(t, old, map[string]interface{}{"name": "ordinary", "namespace": "app"}, "spec", "tls", "options")
			before := old.DeepCopy()
			on := old.DeepCopy()
			on.SetAnnotations(secureAnnotations(entry))
			client := client(old)
			handler := NewValidatedAdmissionHandler(client, secureConfig())
			real, changed := admit(t, handler, on, old, false)
			dry, _ := admit(t, handler, on, old, true)
			if !real.Allowed || !reflect.DeepEqual(real, dry) || len(writes(client)) != 0 {
				t.Fatalf("non-read-only or rejected admission: %+v", real)
			}
			entries, _, _ := unstructured.NestedStringSlice(changed.Object, "spec", "entryPoints")
			options, _, _ := unstructured.NestedMap(changed.Object, "spec", "tls", "options")
			if !reflect.DeepEqual(entries, []string{entry}) || !reflect.DeepEqual(options, map[string]interface{}{"namespace": "app", "name": "client-auth"}) {
				t.Fatal("selected entrypoint/TLSOption not applied")
			}
			if secret, _, _ := unstructured.NestedString(changed.Object, "spec", "tls", "secretName"); secret != "keep" {
				t.Fatal("server certificate overwritten")
			}
			if changed.GetAnnotations()[OriginalTLSOptions] == "" {
				t.Fatal("original TLS options not saved")
			}
			off := changed.DeepCopy()
			a := off.GetAnnotations()
			a[Prefix+"mtls"] = "false"
			off.SetAnnotations(a)
			response, restored := admit(t, handler, off, changed, false)
			if !response.Allowed || !reflect.DeepEqual(before.Object["spec"], restored.Object["spec"]) || restored.GetAnnotations()[OriginalTLSOptions] != "" || restored.GetAnnotations()[OriginalEntrypoints] != "" {
				t.Fatalf("ordinary routing/TLS not restored: %+v", response)
			}
			for _, action := range client.Actions() {
				if action.GetResource().Resource == "secrets" {
					t.Fatal("Secret contents were read")
				}
			}
		})
	}
}

func TestMTLSStrictPolicyChecksOnProviderAdmissionAndReconciler(t *testing.T) {
	for _, mode := range []string{"NoClientCert", "RequestClientCert", "RequireAnyClientCert", "VerifyClientCertIfGiven", "no-ca", "missing", "forbidden", "unapproved-entry", "unapproved-namespace"} {
		t.Run(mode, func(t *testing.T) {
			u := route(t, secureAnnotations("secure-client"))
			c := client(u)
			cfg := secureConfig()
			gvr := tlsOptionGVR(u.GetAPIVersion())
			policy, _ := c.Resource(gvr).Namespace("app").Get(context.Background(), "client-auth", metav1.GetOptions{})
			switch mode {
			case "missing":
				_ = c.Resource(gvr).Namespace("app").Delete(context.Background(), "client-auth", metav1.DeleteOptions{})
			case "forbidden":
				c.PrependReactor("get", "tlsoptions", func(a ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(a.GetResource().GroupResource(), "client-auth", nil)
				})
			case "no-ca":
				unstructured.RemoveNestedField(policy.Object, "spec", "clientAuth", "secretNames")
				_, _ = c.Resource(gvr).Namespace("app").Update(context.Background(), policy, metav1.UpdateOptions{})
			case "unapproved-entry":
				a := u.GetAnnotations()
				a[Prefix+"mtls_entrypoint"] = "unknown"
				u.SetAnnotations(a)
				parentGVR, _ := routeGVR(u.GetAPIVersion())
				_, _ = c.Resource(parentGVR).Namespace("app").Update(context.Background(), u, metav1.UpdateOptions{})
			case "unapproved-namespace":
				cfg.TLSOptionNamespaces = []string{"tls-system"}
			default:
				mustSet(t, policy, mode, "spec", "clientAuth", "clientAuthType")
				_, _ = c.Resource(gvr).Namespace("app").Update(context.Background(), policy, metav1.UpdateOptions{})
			}
			c.ClearActions()
			if _, err := NewProvider(c, cfg).Prepare(context.Background(), v1.MutationRequest{Provider: "traefik", Operation: "update", Resource: reference(u)}); err == nil {
				t.Fatal("unsafe provider request accepted")
			}
			if response, _ := admit(t, NewValidatedAdmissionHandler(c, cfg), u, nil, true); response.Allowed {
				t.Fatal("unsafe admission accepted")
			}
			if err := (Reconciler{Client: c, Config: cfg}).Reconcile(context.Background(), "app", u.GetName(), u.GetAPIVersion()); err == nil {
				t.Fatal("unsafe policy reconciled")
			}
			if len(writes(c)) != 0 {
				t.Fatal("invalid policy caused writes")
			}
		})
	}
}

func TestSecurityCatalogAndPlanDependencies(t *testing.T) {
	u := route(t, map[string]string{"gslb": "true"})
	c := client(u)
	cfg := secureConfig()
	cat := SecurityCatalog(context.Background(), c, "app", cfg)
	if len(cat.Entrypoints) != 2 || len(cat.TLSOptions) != 2 || !cat.TLSOptions[0].Eligible || cat.SANFilterConfigured {
		t.Fatalf("catalog: %+v", cat)
	}
	request := v1.MutationRequest{Provider: "traefik", Operation: "update", Resource: reference(u), Traefik: &v1.TraefikOptions{MTLS: true, MTLSEntrypoint: "websecure", TLSOption: &v1.TLSOptionReference{Namespace: "app", Name: "client-auth"}}}
	prepared, err := NewProvider(c, cfg).Prepare(context.Background(), request)
	if err != nil || len(prepared.Dependencies) != 1 || prepared.Dependencies[0].Kind != "TLSOption" || prepared.Dependencies[0].UID == "" || prepared.Dependencies[0].ResourceVersion == "" {
		t.Fatalf("policy not pinned in plan: %v %+v", err, prepared.Dependencies)
	}
	request.Traefik.CommonNames = []string{"client.example.test"}
	if _, err := NewProvider(c, cfg).Prepare(context.Background(), request); err == nil {
		t.Fatal("DNS SAN allowed without certauthz alias")
	}
	cfg.CNPlugin = "certauthz"
	request.Traefik.CommonNames = []string{"bad/[pattern]"}
	if _, err := NewProvider(c, cfg).Prepare(context.Background(), request); err == nil {
		t.Fatal("unsupported certauthz pattern accepted")
	}
	for _, action := range c.Actions() {
		if action.GetResource().Resource == "secrets" {
			t.Fatal("Secret contents read")
		}
	}
	if len(writes(c)) != 0 {
		t.Fatal("discovery or planning performed writes")
	}
}

func TestOriginalTLSOptionsAbsentAndTamperGuards(t *testing.T) {
	for _, present := range []bool{false, true} {
		u := route(t, secureAnnotations("websecure"))
		if present {
			mustSet(t, u, map[string]interface{}{}, "spec", "tls")
		} else {
			unstructured.RemoveNestedField(u.Object, "spec", "tls")
		}
		before := u.DeepCopy()
		if err := normalizeParent(u, nil, secureConfig()); err != nil {
			t.Fatal(err)
		}
		on := u.DeepCopy()
		a := u.GetAnnotations()
		a[Prefix+"mtls"] = "false"
		u.SetAnnotations(a)
		if err := normalizeParent(u, on, secureConfig()); err != nil {
			t.Fatal(err)
		}
		_, actual, _ := unstructured.NestedMap(u.Object, "spec", "tls")
		if actual != present || !reflect.DeepEqual(u.Object["spec"], before.Object["spec"]) {
			t.Fatal("absent vs empty TLS state not preserved")
		}
		tampered := on.DeepCopy()
		a = tampered.GetAnnotations()
		a[OriginalTLSOptions] = `{"tlsPresent":true,"unrecoverable":true}`
		tampered.SetAnnotations(a)
		if err := normalizeParent(tampered, on, secureConfig()); err == nil {
			t.Fatal("snapshot tampering accepted")
		}
	}
}

func TestSecurityReferenceNamespaceAndFileProviderGuards(t *testing.T) {
	cfg := secureConfig()
	for _, value := range []string{"default@file", "other/client-auth", "app/", "/client-auth", "app/bad,name"} {
		if _, err := tlsOptionReference(value, "app", cfg); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	cfg.TLSOptionNamespaces = []string{"tls-system"}
	ref, err := tlsOptionReference("tls-system/client-auth", "app", cfg)
	if err != nil || ref.Namespace != "tls-system" {
		t.Fatal(err)
	}
	p := NewProvider(client(), cfg).(*provider)
	if !p.AllowsReadDependency(v1.ResourceReference{APIVersion: "traefik.io/v1alpha1", Kind: "TLSOption", Namespace: "tls-system"}) || p.AllowsReadDependency(v1.ResourceReference{APIVersion: "v1", Kind: "Secret", Namespace: "tls-system"}) {
		t.Fatal("read dependency scope is too broad")
	}
	catalog := SecurityCatalog(context.Background(), client(), "app", Config{})
	if len(catalog.Entrypoints) != 0 || !strings.Contains(strings.Join(catalog.Warnings, " "), "allowlist") {
		t.Fatal("invented mTLS capability")
	}
}
