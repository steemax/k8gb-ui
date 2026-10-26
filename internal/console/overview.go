package console

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "k8gb-ui/api/v1"
)

func zoneFor(host string, zones []string) string {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	best := ""
	for _, zone := range zones {
		if (host == zone || strings.HasSuffix(host, "."+zone)) && len(zone) > len(best) {
			best = zone
		}
	}
	return best
}

// Aggregate compares only snapshots the caller was authorized to collect. DNS
// addresses themselves are site-specific; only record counts are compared.
func Aggregate(namespace string, sites []v1.Site, snapshots []v1.Snapshot) v1.Overview {
	result := v1.Overview{APIVersion: v1.Version, Namespace: namespace, CollectedAt: time.Now().UTC(), Sites: sites, Groups: []v1.Group{}, Capabilities: v1.Capabilities{ReadOnly: true, Providers: []string{"native"}}}
	groups := map[string]*v1.Group{}
	siteByID := map[string]v1.Site{}
	for _, site := range sites {
		siteByID[site.Cluster.ID] = site
	}
	for _, snapshot := range snapshots {
		site, known := siteByID[snapshot.ClusterID]
		if !known {
			continue
		}
		for _, object := range snapshot.Objects {
			host := strings.TrimSuffix(strings.ToLower(object.Hostname), ".")
			zone := zoneFor(host, site.Cluster.Zones)
			key := namespace + "\x00" + zone + "\x00" + host
			if host == "" {
				key += "\x00" + snapshot.ClusterID + "\x00" + object.Resource.APIVersion + "/" + object.Resource.Name
			}
			group := groups[key]
			if group == nil {
				sum := sha256.Sum256([]byte(key))
				group = &v1.Group{ID: hex.EncodeToString(sum[:12]), Namespace: namespace, Hostname: host, Zone: zone, Instances: []v1.Instance{}, Signals: []v1.Signal{}, State: "healthy"}
				groups[key] = group
			}
			group.Instances = append(group.Instances, v1.Instance{ClusterID: snapshot.ClusterID, Object: object})
		}
	}
	for _, group := range groups {
		add := func(code, severity, message string) {
			group.Signals = append(group.Signals, v1.Signal{Code: code, Severity: severity, Message: message})
		}
		observed := map[string]int{}
		allowedTags := map[string]bool{}
		for _, site := range sites {
			if contains(site.Cluster.Zones, group.Zone) {
				for _, tag := range site.Cluster.GeoTags {
					allowedTags[tag] = true
				}
			}
		}
		policies := map[string][]string{}
		ttls := map[int64][]string{}
		counts := map[int][]string{}
		for _, instance := range group.Instances {
			observed[instance.ClusterID]++
			site := siteByID[instance.ClusterID]
			if time.Since(site.CollectedAt) > 2*time.Minute {
				add("stale_snapshot", "unknown", instance.ClusterID+": snapshot is older than two minutes")
			}
			if len(site.Cluster.GeoTags) > 0 && instance.GeoTag != "" && !contains(site.Cluster.GeoTags, instance.GeoTag) {
				add("geotag_mismatch", "warning", fmt.Sprintf("%s reports unexpected GeoTag %s", instance.ClusterID, instance.GeoTag))
			}
			if instance.Policy.Strategy == "failover" && (instance.Policy.PrimaryGeoTag == "" || len(allowedTags) > 0 && !allowedTags[instance.Policy.PrimaryGeoTag]) {
				add("invalid_primary", "warning", fmt.Sprintf("%s: primary %q is not a known GeoTag of this zone", instance.ClusterID, instance.Policy.PrimaryGeoTag))
			}
			policyKey := instance.Policy.Strategy + "\x00" + instance.Policy.PrimaryGeoTag
			weightTags := []string{}
			for tag := range instance.Policy.Weights {
				weightTags = append(weightTags, tag)
			}
			sort.Strings(weightTags)
			for _, tag := range weightTags {
				policyKey += "\x00" + tag + "=" + strconv.FormatInt(instance.Policy.Weights[tag], 10)
			}
			policies[policyKey] = append(policies[policyKey], instance.ClusterID)
			ttls[instance.Policy.DNSTTLSeconds] = append(ttls[instance.Policy.DNSTTLSeconds], instance.ClusterID)
			if instance.DNSObserved && !instance.Traffic.Excluded {
				counts[len(instance.DNSTargets)] = append(counts[len(instance.DNSTargets)], instance.ClusterID)
			} else if !instance.DNSObserved && !instance.Traffic.Excluded {
				add("dns_unknown", "unknown", instance.ClusterID+": DNS records were not observed")
			}
			if instance.Traffic.Excluded {
				add("site_excluded", "info", instance.ClusterID+": site is manually excluded; DNS convergence is asynchronous")
			}
			if instance.Traffic.OverrideConflict {
				add("ip_override_conflict", "warning", instance.ClusterID+": IP override snapshot conflicts with live annotations; automatic reset is blocked")
			} else if len(instance.Traffic.IPs) > 0 || len(instance.Traffic.Hostnames) > 0 {
				add("ip_override", "info", instance.ClusterID+": explicit exposed-address override is configured")
			}
			switch strings.ToLower(instance.Health) {
			case "healthy":
			case "excluded":
				if !instance.Traffic.Excluded {
					add("health_unknown", "unknown", instance.ClusterID+": exclusion was not verified from owned routing resources")
				}
			case "unhealthy", "notfound", "unavailable":
				add("backend_health", "warning", instance.ClusterID+": application health is "+instance.Health)
			default:
				add("health_unknown", "unknown", instance.ClusterID+": application health is unknown")
			}
			for _, message := range instance.Warnings {
				add("resource_warning", "unknown", instance.ClusterID+": "+message)
			}
		}
		if group.Zone == "" {
			add("zone_unknown", "unknown", "Hostname does not match a configured DNS zone; expected cluster participation is unknown")
		}
		for _, site := range sites {
			if group.Zone == "" || !contains(site.Cluster.Zones, group.Zone) {
				continue
			}
			if !site.Available {
				add("source_unavailable", "unknown", site.Cluster.ID+": cluster data unavailable")
				continue
			}
			if !site.ObjectsComplete {
				add("source_partial", "unknown", site.Cluster.ID+": Gslb inventory is incomplete; missing resources cannot be confirmed")
				continue
			}
			if observed[site.Cluster.ID] == 0 {
				add("missing_object", "warning", site.Cluster.ID+": no Gslb observed for this hostname in a participating cluster")
			}
			if observed[site.Cluster.ID] > 1 {
				add("ambiguous_object", "warning", site.Cluster.ID+": multiple Gslb resources manage this hostname")
			}
		}
		if len(policies) > 1 {
			add("policy_mismatch", "warning", "Load balancing policy differs between clusters")
		}
		if len(ttls) > 1 {
			add("ttl_mismatch", "warning", "DNS TTL differs between clusters")
		}
		if len(counts) > 1 {
			parts := []string{}
			keys := []int{}
			for count := range counts {
				keys = append(keys, count)
			}
			sort.Ints(keys)
			for _, count := range keys {
				names := counts[count]
				sort.Strings(names)
				parts = append(parts, strings.Join(names, ", ")+"="+strconv.Itoa(count))
			}
			add("dns_target_count", "info", "Different DNS target counts: "+strings.Join(parts, "; "))
		}
		for _, signal := range group.Signals {
			if signal.Severity == "warning" {
				group.State = "warning"
				break
			}
			if signal.Severity == "unknown" {
				group.State = "unknown"
			} else if group.State == "healthy" {
				group.State = "info"
			}
		}
		sort.SliceStable(group.Instances, func(i, j int) bool {
			a, b := group.Instances[i], group.Instances[j]
			if a.ClusterID != b.ClusterID {
				return a.ClusterID < b.ClusterID
			}
			return a.Resource.Name < b.Resource.Name
		})
		result.Groups = append(result.Groups, *group)
	}
	sort.Slice(result.Groups, func(i, j int) bool {
		a, b := result.Groups[i], result.Groups[j]
		if a.Hostname != b.Hostname {
			return a.Hostname < b.Hostname
		}
		return a.ID < b.ID
	})
	return result
}
