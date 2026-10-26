package console

import (
	"context"
	"time"

	v1 "k8gb-ui/api/v1"
)

type demoAgent struct{ cluster string }

func Demo(listen string) (Config, map[string]Agent) {
	c := Config{Listen: listen, Auth: AuthConfig{Mode: "local"}}
	agents := map[string]Agent{}
	for _, id := range []string{"east", "west"} {
		c.Clusters = append(c.Clusters, ClusterConfig{ID: id, Name: map[string]string{"east": "East cluster", "west": "West cluster"}[id], URL: "http://127.0.0.1:8081", Zones: []string{"apps.example.test"}, GeoTags: []string{id}})
		agents[id] = demoAgent{cluster: id}
	}
	return c, agents
}

func (d demoAgent) Namespaces(context.Context) ([]string, error) {
	return []string{"demo", "sandbox"}, nil
}

func (d demoAgent) Snapshot(_ context.Context, namespace string) (v1.Snapshot, error) {
	result := v1.Snapshot{APIVersion: v1.Version, ClusterID: d.cluster, Namespace: namespace, CollectedAt: time.Now().UTC(), Capabilities: v1.Capabilities{ReadOnly: true, Providers: []string{"native"}}, ObjectsComplete: true, Objects: []v1.Object{}, Warnings: []string{}}
	if namespace != "demo" {
		return result, nil
	}
	for _, item := range []struct {
		name, strategy string
		ttl            int64
		primary        string
	}{
		{"catalog", "roundRobin", 10, ""}, {"checkout", "failover", 10, "east"}, {"payments", "weightedRoundRobin", 30, ""}, {"identity", "geoip", 30, ""}, {"reports", "roundRobin", 10, ""},
	} {
		if item.name == "reports" && d.cluster == "west" {
			continue
		}
		object := v1.Object{Resource: v1.ResourceReference{APIVersion: "k8gb.io/v1beta1", Kind: "Gslb", Namespace: namespace, Name: item.name, UID: d.cluster + "-" + item.name, ResourceVersion: "1"}, Target: &v1.ResourceReference{APIVersion: "networking.k8s.io/v1", Kind: "Ingress", Namespace: namespace, Name: item.name}, Hostname: item.name + ".apps.example.test", GeoTag: d.cluster, Policy: v1.Policy{Strategy: item.strategy, DNSTTLSeconds: item.ttl, PrimaryGeoTag: item.primary}, Health: "healthy", DNSObserved: true, DNSTargets: []string{"192.0.2.10", "198.51.100.20"}, Backends: []v1.Backend{{Namespace: namespace, Service: item.name, Port: "8080", Health: "healthy", EndpointDataAvailable: true, ReadyEndpoints: 2, TotalEndpoints: 2}}, Warnings: []string{}}
		if item.strategy == "weightedRoundRobin" {
			object.Policy.Weights = map[string]int64{"east": 1, "west": 5}
		}
		if item.name == "catalog" && d.cluster == "west" {
			object.DNSTargets = []string{"198.51.100.20"}
		}
		if item.name == "checkout" {
			object.DNSTargets = []string{"192.0.2.10"}
		}
		result.Objects = append(result.Objects, object)
	}
	return result, nil
}
