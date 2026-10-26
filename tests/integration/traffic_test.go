//go:build integration

package integration_test

import (
	"reflect"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8gb-ui/internal/mutation"
	"k8gb-ui/internal/traefik"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTrafficOverrideRealKind(t *testing.T) {
	f := newFixture(t)
	for _, group := range []string{"k8gb.io", "k8gb.absa.oss"} {
		t.Run(group, func(t *testing.T) {
			m := f.manager(t, nil, traefik.Config{}, nil)
			r := f.createRequest(t, group, "traffic-"+suffix(t))
			p, err := m.Plan(f.ctx, r)
			check(t, err)
			_, err = m.Apply(f.ctx, p)
			check(t, err)
			gvr := gslbs
			gvr.Group = group
			original := f.get(t, gvr, r.Resource.Name)
			original.SetAnnotations(map[string]string{mutation.ExposedIPsAnnotation: "192.0.2.42", mutation.ExposedHostnamesAnnotation: "original.example.test", "example.test/keep": "yes"})
			original, err = f.api(gvr).Update(f.ctx, original, metav1.UpdateOptions{})
			check(t, err)
			r = v1.MutationRequest{Provider: "traffic", Operation: "overrideIPs", Resource: ref(original), Traffic: &v1.TrafficOptions{IPs: []string{"198.51.100.9", "198.51.100.10"}}, Actor: "local-admin", Reason: "Planned override acceptance"}
			p, err = m.Plan(f.ctx, r)
			check(t, err)
			f.unchanged(t, gvr, original)
			result, err := m.Apply(f.ctx, p)
			check(t, err)
			if result.Status != "applied" {
				t.Fatalf("result=%+v", result)
			}
			changed := f.get(t, gvr, original.GetName())
			status := mutation.InspectTraffic(changed.GetAnnotations())
			if !status.OverrideManaged || status.OverrideConflict || len(status.Hostnames) != 0 {
				t.Fatalf("status=%+v", status)
			}
			if !reflect.DeepEqual(original.Object["spec"], changed.Object["spec"]) {
				t.Fatal("override touched policy or routing")
			}
			settings := v1.MutationRequest{Provider: "native", Operation: "update", Resource: ref(changed), Policy: &v1.Policy{Strategy: "failover", PrimaryGeoTag: "west", DNSTTLSeconds: 45}, Actor: r.Actor, Reason: "Settings must preserve override"}
			applied(t, f, m, settings)
			afterSettings := f.get(t, gvr, changed.GetName())
			if !reflect.DeepEqual(changed.GetAnnotations(), afterSettings.GetAnnotations()) {
				t.Fatal("settings changed operational snapshot")
			}
			changed = afterSettings
			// A new process can safely reset using only the resource-local snapshot.
			m = f.manager(t, nil, traefik.Config{}, nil)
			r.Resource, r.Operation, r.Traffic = ref(changed), "resetIPs", nil
			p, err = m.Plan(f.ctx, r)
			check(t, err)
			_, err = m.Apply(f.ctx, p)
			check(t, err)
			restored := f.get(t, gvr, original.GetName())
			if !reflect.DeepEqual(original.GetAnnotations(), restored.GetAnnotations()) {
				t.Fatalf("restore=%v", restored.GetAnnotations())
			}
			r.Resource, r.Operation, r.Traffic = ref(restored), "overrideIPs", &v1.TrafficOptions{IPs: []string{"203.0.113.9"}}
			p, err = m.Plan(f.ctx, r)
			check(t, err)
			a := restored.GetAnnotations()
			a[mutation.ExposedIPsAnnotation] = "192.0.2.77"
			restored.SetAnnotations(a)
			restored, err = f.api(gvr).Update(f.ctx, restored, metav1.UpdateOptions{})
			check(t, err)
			_, err = m.Apply(f.ctx, p)
			expectFailure(t, err, 409, "stale_resource", "plan_drift")
			f.unchanged(t, gvr, restored)
		})
	}
}
