package mutation

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/netip"
	"reflect"
	"sort"
	"strings"

	v1 "k8gb-ui/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

const (
	ExposedIPsAnnotation       = "k8gb.io/exposed-ip-addresses"
	ExposedHostnamesAnnotation = "k8gb.io/exposed-hostnames"
	IPOverrideStateAnnotation  = "k8gb-ui.io/ip-override-state"
	SiteExcludedAnnotation     = "k8gb-ui.io/site-excluded"
)

// The snapshot lives with the affected Gslb, so restarting Console/agent cannot
// lose the original values. Pointers distinguish an absent annotation from "".
type ipSnapshot struct {
	Version   int     `json:"version"`
	IPs       *string `json:"ips"`
	Hostnames *string `json:"hostnames"`
	Applied   string  `json:"applied"`
}

type trafficProvider struct{ client dynamic.Interface }

func NewTrafficProvider(client dynamic.Interface) Provider { return &trafficProvider{client: client} }
func (*trafficProvider) Operations() []string              { return []string{"overrideIPs", "resetIPs"} }
func (*trafficProvider) ConfigurationFingerprint() string  { return "ip-snapshot-v1-ipv4" }

func ValidateOverrideIPs(ips []string) ([]string, error) {
	if len(ips) == 0 || len(ips) > 16 {
		return nil, Failure(422, "invalid_ips", "provide between 1 and 16 explicit IPv4 addresses")
	}
	result := make([]string, 0, len(ips))
	seen := map[string]bool{}
	for _, raw := range ips {
		ip, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip == netip.MustParseAddr("255.255.255.255") {
			return nil, Failure(422, "invalid_ips", "addresses must be IPv4 unicast (no hostnames, ports, CIDRs, loopback or link-local); upstream application DNS publishes A records")
		}
		value := ip.String()
		if seen[value] {
			return nil, Failure(422, "invalid_ips", "duplicate override IP address")
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func readIPSnapshot(a map[string]string) (*ipSnapshot, error) {
	raw, found := a[IPOverrideStateAnnotation]
	if !found {
		return nil, nil
	}
	if len(raw) == 0 || len(raw) > 16384 {
		return nil, Failure(409, "override_conflict", "invalid IP override snapshot; inspect the live annotations")
	}
	var state ipSnapshot
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return nil, Failure(409, "override_conflict", "invalid IP override snapshot; inspect the live annotations")
	}
	if decoder.Decode(new(interface{})) != io.EOF || state.Version != 1 || state.IPs != nil && len(*state.IPs) > 4096 || state.Hostnames != nil && len(*state.Hostnames) > 4096 {
		return nil, Failure(409, "override_conflict", "unsupported or oversized IP override snapshot")
	}
	ips, err := ValidateOverrideIPs(strings.Split(state.Applied, ","))
	if err != nil || strings.Join(ips, ",") != state.Applied {
		return nil, Failure(409, "override_conflict", "invalid installed IP override snapshot")
	}
	if current, ok := a[ExposedIPsAnnotation]; !ok || current != state.Applied {
		return nil, Failure(409, "override_conflict", "IP override changed outside this operation; refusing to overwrite or restore")
	}
	if _, ok := a[ExposedHostnamesAnnotation]; ok {
		return nil, Failure(409, "override_conflict", "exposed-hostnames was changed outside this operation; refusing to overwrite or restore")
	}
	return &state, nil
}

func annotationValue(a map[string]string, key string) *string {
	value, found := a[key]
	if !found {
		return nil
	}
	return &value
}

func restoreAnnotation(a map[string]string, key string, value *string) {
	if value == nil {
		delete(a, key)
	} else {
		a[key] = *value
	}
}

func (p *trafficProvider) Prepare(ctx context.Context, req v1.MutationRequest) (Prepared, error) {
	var prepared Prepared
	if req.Operation != "overrideIPs" && req.Operation != "resetIPs" {
		return prepared, Unsupported("traffic", req.Operation)
	}
	gvr, err := resourceGVR(req.Resource)
	if err != nil {
		return prepared, err
	}
	if req.Provider != "traffic" || gvr.Resource != "gslbs" || req.Resource.UID == "" || req.Resource.ResourceVersion == "" || req.Target != nil || req.Policy != nil || req.Traefik != nil || p.client == nil {
		return prepared, Failure(422, "invalid_traffic_request", "traffic operations require only a pinned Gslb resource and optional IP options")
	}
	if req.Operation == "resetIPs" && req.Traffic != nil || req.Operation == "overrideIPs" && req.Traffic == nil {
		return prepared, Failure(422, "invalid_traffic_request", "overrideIPs requires traffic.ips; resetIPs does not accept options")
	}
	current, err := p.client.Resource(gvr).Namespace(req.Resource.Namespace).Get(ctx, req.Resource.Name, metav1.GetOptions{})
	if err != nil {
		return prepared, err
	}
	if !matchesReference(current, req.Resource, true) || current.GetDeletionTimestamp() != nil {
		return prepared, Failure(409, "stale_resource", "Gslb identity/version changed or it is deleting")
	}
	desired := current.DeepCopy()
	a := desired.GetAnnotations()
	if a == nil {
		a = map[string]string{}
	}
	state, err := readIPSnapshot(a)
	if err != nil {
		return prepared, err
	}
	if req.Operation == "resetIPs" {
		if state != nil {
			restoreAnnotation(a, ExposedIPsAnnotation, state.IPs)
			restoreAnnotation(a, ExposedHostnamesAnnotation, state.Hostnames)
			delete(a, IPOverrideStateAnnotation)
		} else if _, ips := a[ExposedIPsAnnotation]; ips {
			return prepared, Failure(409, "unmanaged_override", "no managed snapshot exists; refusing to remove an externally configured IP override")
		}
	} else {
		ips, err := ValidateOverrideIPs(req.Traffic.IPs)
		if err != nil {
			return prepared, err
		}
		if state == nil {
			state = &ipSnapshot{Version: 1, IPs: annotationValue(a, ExposedIPsAnnotation), Hostnames: annotationValue(a, ExposedHostnamesAnnotation)}
		}
		if state.IPs != nil && len(*state.IPs) > 4096 || state.Hostnames != nil && len(*state.Hostnames) > 4096 {
			return prepared, Failure(422, "snapshot_too_large", "existing override annotation is too large to snapshot safely")
		}
		state.Applied = strings.Join(ips, ",")
		raw, _ := json.Marshal(state)
		a[IPOverrideStateAnnotation], a[ExposedIPsAnnotation] = string(raw), state.Applied
		// Upstream gives exposed-hostnames higher precedence than exposed IPs.
		delete(a, ExposedHostnamesAnnotation)
	}
	if len(a) != 0 || len(current.GetAnnotations()) != 0 {
		desired.SetAnnotations(a)
	}
	prepared = Prepared{GVR: gvr, Current: current, Desired: desired,
		Fields:   []string{annotationPath(ExposedIPsAnnotation), annotationPath(ExposedHostnamesAnnotation), annotationPath(IPOverrideStateAnnotation)},
		Warnings: []string{"This operation affects every hostname of this Gslb, not just the displayed hostname. Only this resource in this cluster is changed; other Gslbs sharing DNS names are not modified automatically.", "The k8gb controller must reconcile and DNS caches must expire. Health checks still apply; a successful annotation write is not confirmation of DNS convergence."},
	}
	return prepared, nil
}

func annotationPath(key string) string {
	return "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func (*trafficProvider) ValidateAdmission(prepared Prepared, admitted *unstructured.Unstructured) error {
	// The API server omits metadata.annotations after removing its final key.
	// The engine already checks every field outside this narrow allowlist.
	for _, path := range prepared.Fields {
		wanted, wantedFound := jsonValue(prepared.Desired, path)
		got, gotFound := jsonValue(admitted, path)
		if wantedFound != gotFound || !reflect.DeepEqual(wanted, got) {
			return Failure(409, "unexpected_mutation", "admission changed the reviewed traffic operation")
		}
	}
	return nil
}

func InspectTraffic(annotations map[string]string) v1.TrafficStatus {
	status := v1.TrafficStatus{Excluded: annotations[SiteExcludedAnnotation] == "true", IPs: []string{}, Hostnames: []string{}}
	if raw, ok := annotations[ExposedIPsAnnotation]; ok {
		status.IPs = strings.Split(raw, ",")
	}
	if raw, ok := annotations[ExposedHostnamesAnnotation]; ok {
		status.Hostnames = strings.Split(raw, ",")
	}
	_, status.OverrideManaged = annotations[IPOverrideStateAnnotation]
	_, err := readIPSnapshot(annotations)
	status.OverrideConflict = err != nil
	return status
}
