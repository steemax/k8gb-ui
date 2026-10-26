package console

import (
	"testing"
	"time"

	v1 "k8gb-ui/api/v1"
)

func TestOperationalTrafficSignalsDoNotHideMissingCollectionData(t *testing.T) {
	sites := []v1.Site{}
	snapshots := []v1.Snapshot{}
	for _, id := range []string{"east", "west"} {
		sites = append(sites, v1.Site{Cluster: v1.Cluster{ID: id, GeoTags: []string{id}, Zones: []string{"apps.example.test"}}, Available: true, ObjectsComplete: true, CollectedAt: time.Now()})
		o := v1.Object{Resource: v1.ResourceReference{APIVersion: "k8gb.io/v1beta1", Kind: "Gslb", Namespace: "app", Name: "web"}, Hostname: "web.apps.example.test", Health: "Healthy", DNSObserved: true, DNSTargets: []string{"192.0.2.1"}, Policy: v1.Policy{Strategy: "roundRobin", DNSTTLSeconds: 30}}
		if id == "west" {
			o.Health = "Excluded"
			o.Traffic.Excluded = true
			o.DNSObserved = false
			o.DNSTargets = nil
		}
		snapshots = append(snapshots, v1.Snapshot{ClusterID: id, Namespace: "app", Objects: []v1.Object{o}})
	}
	result := Aggregate("app", sites, snapshots)
	if len(result.Groups) != 1 || result.Groups[0].State != "info" {
		t.Fatalf("expected informational exclusion: %+v", result.Groups)
	}
	for _, s := range result.Groups[0].Signals {
		if s.Code == "dns_unknown" || s.Code == "health_unknown" || s.Code == "dns_target_count" {
			t.Fatalf("false exclusion problem: %+v", s)
		}
	}
	snapshots[1].Objects[0].Warnings = []string{"DNSEndpoint API unavailable"}
	if got := Aggregate("app", sites, snapshots).Groups[0].State; got != "unknown" {
		t.Fatalf("collection outage was hidden: %s", got)
	}
	snapshots[1].Objects[0].Warnings = nil
	snapshots[1].Objects[0].Traffic.OverrideConflict = true
	if got := Aggregate("app", sites, snapshots).Groups[0].State; got != "warning" {
		t.Fatalf("IP conflict not surfaced: %s", got)
	}
}
