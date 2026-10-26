//go:build integration

package integration_test

import (
	"reflect"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/traefik"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestNativeRealKind(t *testing.T) {
	f := newFixture(t)
	for _, group := range []string{"k8gb.io", "k8gb.absa.oss"} {
		t.Run(group, func(t *testing.T) {
			gvr := gslbs
			gvr.Group = group
			var events []v1.AuditEvent
			m := f.manager(t, nil, traefik.Config{}, func(event v1.AuditEvent) { events = append(events, event) })
			request := f.createRequest(t, group, "native-"+suffix(t))
			target := f.get(t, ingresses, request.Target.Name)
			plan, err := m.Plan(f.ctx, request)
			check(t, err)
			if plan.Noop || plan.Token == "" || plan.Digest == "" || len(plan.Changes) == 0 || len(plan.Dependencies) != 1 {
				t.Fatalf("invalid create plan: %+v", plan)
			}
			f.absent(t, gvr, request.Resource.Name)
			f.unchanged(t, ingresses, target)
			result, err := m.Apply(f.ctx, plan)
			check(t, err)
			if result.Status != "applied" {
				t.Fatalf("create status: %+v", result)
			}
			created := f.get(t, gvr, request.Resource.Name)
			nested(t, created, "roundRobin", "spec", "strategy", "type")
			nested(t, created, request.Target.Name, "spec", "resourceRef", "name")
			created.SetAnnotations(map[string]string{"k8gb.io/exposed-ip-addresses": "192.0.2.42", "k8gb.io/exposed-hostnames": "override.example.test", "integration.example.test/keep": "true"})
			check(t, unstructured.SetNestedField(created.Object, int64(120), "spec", "strategy", "splitBrainThresholdSeconds"))
			created, err = f.api(gvr).Update(f.ctx, created, metav1.UpdateOptions{})
			check(t, err)
			request.Operation, request.Resource, request.Target = "update", ref(created), nil
			request.Policy = &v1.Policy{Strategy: "failover", PrimaryGeoTag: "east", DNSTTLSeconds: 90}
			plan, err = m.Plan(f.ctx, request)
			check(t, err)
			f.unchanged(t, gvr, created)
			result, err = m.Apply(f.ctx, plan)
			check(t, err)
			if result.Status != "applied" {
				t.Fatalf("update status: %+v", result)
			}
			updated := f.get(t, gvr, created.GetName())
			nested(t, updated, "failover", "spec", "strategy", "type")
			nested(t, updated, "east", "spec", "strategy", "primaryGeoTag")
			nested(t, updated, int64(90), "spec", "strategy", "dnsTtlSeconds")
			nested(t, updated, int64(120), "spec", "strategy", "splitBrainThresholdSeconds")
			if updated.GetUID() != created.GetUID() || !reflect.DeepEqual(updated.GetAnnotations(), created.GetAnnotations()) {
				t.Fatal("update changed identity or unrelated annotations")
			}
			statuses := map[string]bool{}
			for _, event := range events {
				statuses[event.Status] = true
				if event.Actor != request.Actor || event.Reason != request.Reason || event.ClusterID != "east" {
					t.Fatalf("incorrect audit attribution: %+v", event)
				}
			}
			for _, status := range []string{"plan_started", "planned", "apply_started", "applied"} {
				if !statuses[status] {
					t.Fatalf("missing audit status %s", status)
				}
			}
			t.Run("noop_does_not_write", func(t *testing.T) {
				r := request
				r.Resource = ref(updated)
				p, err := m.Plan(f.ctx, r)
				check(t, err)
				if !p.Noop {
					t.Fatal("unchanged policy must produce a noop plan")
				}
				result, err := m.Apply(f.ctx, p)
				check(t, err)
				if result.Status != "noop" {
					t.Fatalf("unexpected noop result: %+v", result)
				}
				f.unchanged(t, gvr, updated)
			})

			t.Run("required_reason", func(t *testing.T) {
				r := request
				r.Resource = ref(updated)
				r.Reason = "   "
				_, err := m.Plan(f.ctx, r)
				expectFailure(t, err, 422, "invalid_request")
				f.unchanged(t, gvr, updated)
			})
			t.Run("forbidden_namespace", func(t *testing.T) {
				restricted := f.manager(t, []string{"k8gb-ui-e2e-not-allowed"}, traefik.Config{}, nil)
				r := request
				r.Resource = ref(updated)
				_, err := restricted.Plan(f.ctx, r)
				expectFailure(t, err, 403, "namespace_forbidden")
				f.unchanged(t, gvr, updated)
			})
			for _, field := range []string{"uid", "resourceVersion"} {
				t.Run("stale_"+field, func(t *testing.T) {
					r := request
					r.Resource = ref(updated)
					if field == "uid" {
						r.Resource.UID = "00000000-0000-0000-0000-000000000000"
					} else {
						r.Resource.ResourceVersion = created.GetResourceVersion()
					}
					_, err := m.Plan(f.ctx, r)
					expectFailure(t, err, 409, "stale_resource")
					f.unchanged(t, gvr, updated)
				})
			}
			t.Run("tampered_token", func(t *testing.T) {
				r := request
				r.Resource = ref(updated)
				r.Policy = &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 80}
				p, err := m.Plan(f.ctx, r)
				check(t, err)
				p.Token = strings.Repeat("x", len(p.Token))
				_, err = m.Apply(f.ctx, p)
				expectFailure(t, err, 403, "invalid_signature")
				f.unchanged(t, gvr, updated)
			})
			t.Run("drift_after_plan", func(t *testing.T) {
				r := request
				r.Resource = ref(updated)
				r.Policy = &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 80}
				p, err := m.Plan(f.ctx, r)
				check(t, err)
				live := updated.DeepCopy()
				a := live.GetAnnotations()
				a["integration.example.test/drift"] = "changed"
				live.SetAnnotations(a)
				live, err = f.api(gvr).Update(f.ctx, live, metav1.UpdateOptions{})
				check(t, err)
				_, err = m.Apply(f.ctx, p)
				expectFailure(t, err, 409, "stale_resource", "plan_drift")
				f.unchanged(t, gvr, live)
			})
			t.Run("target_drift", func(t *testing.T) {
				r := f.createRequest(t, group, "dependency-"+suffix(t))
				p, err := m.Plan(f.ctx, r)
				check(t, err)
				live := f.get(t, ingresses, r.Target.Name)
				live.SetAnnotations(map[string]string{"integration.example.test/drift": "changed"})
				_, err = f.api(ingresses).Update(f.ctx, live, metav1.UpdateOptions{})
				check(t, err)
				_, err = m.Apply(f.ctx, p)
				expectFailure(t, err, 409, "stale_dependency", "plan_drift")
				f.absent(t, gvr, r.Resource.Name)
			})
			t.Run("replaced_uid_after_plan", func(t *testing.T) {
				before := f.get(t, gvr, updated.GetName())
				r := request
				r.Resource = ref(before)
				r.Policy = &v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 80}
				p, err := m.Plan(f.ctx, r)
				check(t, err)
				uid := before.GetUID()
				check(t, f.api(gvr).Delete(f.ctx, before.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}))
				replacement := object(before.GetAPIVersion(), before.GetKind(), f.ns, before.GetName(), before.Object["spec"].(map[string]interface{}))
				replacement, err = f.api(gvr).Create(f.ctx, replacement, metav1.CreateOptions{})
				check(t, err)
				if replacement.GetUID() == before.GetUID() {
					t.Fatal("Kubernetes did not allocate a replacement UID")
				}
				_, err = m.Apply(f.ctx, p)
				expectFailure(t, err, 409, "stale_resource", "plan_drift")
				f.unchanged(t, gvr, replacement)
			})
		})
	}
}
