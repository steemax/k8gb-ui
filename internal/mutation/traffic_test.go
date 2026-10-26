package mutation

import (
	"context"
	"reflect"
	"strings"
	"testing"

	v1 "k8gb-ui/api/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func newTrafficHarness(t *testing.T, annotations map[string]string) (*Manager, *mutationAPI, v1.MutationRequest) {
	t.Helper()
	m, api, _ := newHarness(t)
	m.opts.Providers["traffic"] = NewTrafficProvider(m.client)
	ref := objectReference(fixtureObject())
	api.objects[resourcePath(ref)].SetAnnotations(annotations)
	return m, api, v1.MutationRequest{Provider: "traffic", Operation: "overrideIPs", Resource: ref, Traffic: &v1.TrafficOptions{IPs: []string{"192.0.2.2", "192.0.2.1"}}, Actor: "admin", Reason: "Planned maintenance"}
}

func TestTrafficOverrideLifecycleRestoresOriginalIncludingEmpty(t *testing.T) {
	for _, initial := range []map[string]string{nil, {ExposedIPsAnnotation: ""}, {ExposedHostnamesAnnotation: ""}, {ExposedIPsAnnotation: "192.0.2.9", ExposedHostnamesAnnotation: "old.example.test", "example.test/keep": "yes"}} {
		m, api, req := newTrafficHarness(t, initial)
		before := api.objects[resourcePath(req.Resource)].DeepCopy()
		var events []v1.AuditEvent
		m.opts.Audit = func(e v1.AuditEvent) { events = append(events, e) }
		apply := func() *unstructured.Unstructured {
			t.Helper()
			plan, err := m.Plan(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if result, err := m.Apply(context.Background(), plan); err != nil || result.Status != "applied" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			return api.objects[resourcePath(req.Resource)].DeepCopy()
		}
		plan, err := m.Plan(context.Background(), req)
		if err != nil || api.writes != 0 || !reflect.DeepEqual(before.Object, api.objects[resourcePath(req.Resource)].Object) {
			t.Fatalf("plan had side effects: %+v %v", plan, err)
		}
		current := apply()
		status := InspectTraffic(current.GetAnnotations())
		if !status.OverrideManaged || status.OverrideConflict || len(status.Hostnames) != 0 || !reflect.DeepEqual(status.IPs, []string{"192.0.2.1", "192.0.2.2"}) {
			t.Fatalf("status=%+v", status)
		}
		if !reflect.DeepEqual(before.Object["spec"], current.Object["spec"]) || !reflect.DeepEqual(before.Object["status"], current.Object["status"]) {
			t.Fatal("override changed policy/routing/status")
		}
		// Updating an active override must not replace the original snapshot.
		req.Resource, req.Traffic.IPs = objectReference(current), []string{"198.51.100.5"}
		current = apply()
		// A fresh manager (e.g. after restart) restores from Kubernetes alone.
		fresh, err := NewManager(m.client, m.opts)
		if err != nil {
			t.Fatal(err)
		}
		m = fresh
		req.Resource, req.Operation, req.Traffic = objectReference(current), "resetIPs", nil
		current = apply()
		if len(current.GetAnnotations()) != len(initial) || len(initial) != 0 && !reflect.DeepEqual(current.GetAnnotations(), initial) {
			t.Fatalf("original annotations not restored: want=%v got=%v", initial, current.GetAnnotations())
		}
		if len(events) < 12 || events[len(events)-1].Operation != "resetIPs" || events[len(events)-1].Actor != "admin" {
			t.Fatalf("audit=%+v", events)
		}
	}
}

func TestTrafficOverrideConflictAndUnmanagedReset(t *testing.T) {
	for _, conflict := range []string{"ips", "hostnames", "snapshot"} {
		t.Run(conflict, func(t *testing.T) {
			m, api, req := newTrafficHarness(t, nil)
			plan, err := m.Plan(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.Apply(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			current := api.objects[resourcePath(req.Resource)]
			a := current.GetAnnotations()
			switch conflict {
			case "ips":
				a[ExposedIPsAnnotation] = "203.0.113.9"
			case "hostnames":
				a[ExposedHostnamesAnnotation] = "external.example.test"
			case "snapshot":
				a[IPOverrideStateAnnotation] = "{}"
			}
			current.SetAnnotations(a)
			req.Resource = objectReference(current)
			for _, op := range []string{"overrideIPs", "resetIPs"} {
				req.Operation = op
				if op == "resetIPs" {
					req.Traffic = nil
				}
				_, err = m.Plan(context.Background(), req)
				requireFailure(t, err, "override_conflict")
			}
			if !InspectTraffic(a).OverrideConflict {
				t.Fatal("conflict not surfaced")
			}
		})
	}
	m, _, req := newTrafficHarness(t, map[string]string{ExposedIPsAnnotation: "192.0.2.9"})
	req.Operation, req.Traffic = "resetIPs", nil
	_, err := m.Plan(context.Background(), req)
	requireFailure(t, err, "unmanaged_override")
}

func TestTrafficOverrideValidationAndPinnedPlan(t *testing.T) {
	for _, ips := range [][]string{nil, {""}, {"localhost"}, {"127.0.0.1"}, {"0.0.0.0"}, {"169.254.1.2"}, {"224.0.0.1"}, {"255.255.255.255"}, {"2001:db8::1"}, {"::ffff:192.0.2.1"}, {"192.0.2.1/32"}, {"192.0.2.1:80"}, {"192.0.2.1", "192.0.2.1"}, strings.Split(strings.Repeat("192.0.2.1,", 16)+"192.0.2.2", ",")} {
		if _, err := ValidateOverrideIPs(ips); err == nil {
			t.Fatalf("accepted invalid IPs %v", ips)
		}
	}
	m, api, req := newTrafficHarness(t, nil)
	for _, bad := range []func(*v1.MutationRequest){func(r *v1.MutationRequest) { r.Reason = " " }, func(r *v1.MutationRequest) { r.Resource.UID = "" }, func(r *v1.MutationRequest) { r.Resource.Namespace = "foreign" }, func(r *v1.MutationRequest) { r.Policy = &v1.Policy{Strategy: "roundRobin"} }, func(r *v1.MutationRequest) { r.Traffic = nil }, func(r *v1.MutationRequest) {
		r.Resource.Kind = "Ingress"
		r.Resource.APIVersion = "networking.k8s.io/v1"
	}, func(r *v1.MutationRequest) { r.Operation = "excludeSite" }} {
		r := req
		bad(&r)
		if _, err := m.Plan(context.Background(), r); err == nil {
			t.Fatal("invalid traffic request accepted")
		}
	}
	plan, err := m.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	current := api.objects[resourcePath(req.Resource)]
	current.SetResourceVersion("77")
	current.SetLabels(map[string]string{"external-change": "must-replan"})
	if _, err = m.Apply(context.Background(), plan); err == nil {
		t.Fatal("stale plan applied")
	}
	if api.writes != 0 {
		t.Fatal("stale request wrote")
	}
}

func TestTrafficReviewedPlanSurvivesControllerStatusUpdates(t *testing.T) {
	m, api, req := newTrafficHarness(t, nil)
	plan, err := m.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	current := api.objects[resourcePath(req.Resource)]
	current.SetResourceVersion("controller-status")
	current.Object["status"] = map[string]interface{}{"hosts": "web.example.test"}
	result, err := m.Apply(context.Background(), plan)
	if err != nil || result.Status != "applied" || api.writes != 1 {
		t.Fatalf("status update blocked IP override: %+v %v", result, err)
	}
}

func TestTrafficUnexpectedAdmissionAndNoop(t *testing.T) {
	m, api, req := newTrafficHarness(t, nil)
	api.admit = func(o *unstructured.Unstructured, _ bool) {
		a := o.GetAnnotations()
		a[ExposedIPsAnnotation] = "192.0.2.99"
		o.SetAnnotations(a)
	}
	_, err := m.Plan(context.Background(), req)
	requireFailure(t, err, "unexpected_mutation")
	api.admit = nil
	plan, err := m.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	req.Resource = objectReference(api.objects[resourcePath(req.Resource)])
	plan, err = m.Plan(context.Background(), req)
	if err != nil || !plan.Noop {
		t.Fatalf("same override must be noop %+v %v", plan, err)
	}
	req.Provider = "native"
	req.Operation = "update"
	req.Policy = &v1.Policy{Strategy: "roundRobin"}
	if _, err = m.Plan(context.Background(), req); err == nil {
		t.Fatal("native policy provider accepted traffic options")
	}
}

func TestTrafficResetAllowsServerToOmitEmptyAnnotationMap(t *testing.T) {
	m, api, req := newTrafficHarness(t, nil)
	plan, err := m.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Apply(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	req.Operation, req.Resource, req.Traffic = "resetIPs", objectReference(api.objects[resourcePath(req.Resource)]), nil
	api.admit = func(o *unstructured.Unstructured, _ bool) {
		if len(o.GetAnnotations()) == 0 {
			o.SetAnnotations(nil)
		}
	}
	plan, err = m.Plan(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := m.Apply(context.Background(), plan); err != nil || result.Status != "applied" {
		t.Fatalf("reset=%+v %v", result, err)
	}
}
