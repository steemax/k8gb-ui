# GSLB mutations and optional Traefik adapter

## Opt-in, administrator-only

For shared Traefik entrypoint/TLSOption selection and optional certificate DNS
SAN filtering, see [Traefik mTLS](traefik-mtls.md). The selector never reads Secrets.

Existing configurations remain read-only. Enable all layers explicitly:

1. Agent write RBAC (`deploy/agent-write-rbac.yaml`, or namespace-scoped bindings).
2. Agent `--enable-writes`, a dedicated `--write-token-file`, a separate random
   `--plan-key-file` (at least 32 bytes), and explicit `--geotags`.
3. Console `writesEnabled: true`, per-cluster `writeTokenFile`, and OIDC/builtin access
   bindings with `role: admin`. Existing bindings default to viewer.

Read tokens cannot use mutation endpoints. Client certificates are independent
of tokens; TLS verification remains mandatory. Do not expose an agent's
plaintext HTTP listener outside a trusted TLS ingress/proxy boundary.

Local development additionally needs `auth.localAdmin: true`; `auth.mode: local`
still refuses any non-loopback bind. The illustrative `--demo` stays read-only.

## API

- Console: POST `/api/v1/mutations/plan` accepts `{targets:[{clusterId,request}]}`.
- Console: POST `/api/v1/mutations/apply` accepts the returned target plans and an
  explicit `allowPartial` boolean. JSON requests require same Origin, same-origin
  credentials and `X-K8gb-Intent: mutation`. Browser-supplied actors are replaced
  with the authenticated OIDC subject or `builtin:<username>` identity.
- Agent: the same endpoints operate on one `MutationRequest`/`MutationPlan`, using
  the dedicated write credential, not a claimed identity header.
- GET `/api/v1/catalog?namespace=...` (agent `/api/v1/candidates`) returns current
  pinned routing references and capabilities, with per-cluster failures.
- GET `/api/v1/audit?namespace=...` returns recent, namespace-filtered agent logs.

Requests use exact API version, kind, namespace and name. Updates require UID
and resourceVersion; native creation also pins its Ingress/HTTPRoute target.
Each plan includes Kubernetes dry-run results, the diff, actor/reason, dependency
versions, cluster identity, a five-minute expiry and an HMAC signature. Apply
verifies the receipt, re-reads dependencies, repeats dry-run and uses CAS patches.
Canonical `k8gb.io/v1beta1` Gslb dependencies also carry a server-derived signed
`configurationDigest`. Only controller-owned `status` and volatile metadata
(RV, generation, timestamps and managedFields) are excluded from that hash;
spec, annotations, labels, owner references, finalizers and UID still guard
against configuration drift or replacement. Apply may advance the Gslb RV for
status-only churn after review, but its single write still atomically tests
the freshly observed UID/RV. Other dependencies retain exact RV matching.
There is no automatic retry or multi-cluster transaction. A timeout can mean
`outcomeUnknown`; inspect live resources and audit before preparing another plan.

## Native provider (no webhook)

The read model reports `management: native | traefik | external`. Native objects
show **Direct Gslb** and **Edit GSLB policy**; verified adapter ownership shows
**Webhook / Traefik** and **Edit source IngressRoute**. Labels alone cannot claim
adapter ownership: the route owner UID must match its adapter owner label.
Unknown/other owners are not advertised as direct-edit targets. Group summaries
may contain more than one mode; per-site write targets remain explicit. This
classification does not replace authenticated API authorization or provider checks.

Before previewing existing Gslb edits, UI refreshes references from the current
namespace snapshot. It retains form intent and refuses changed identity,
ownership, visible policy or operational state. Only RV/status churn may advance
the reference. This is not an automatic apply retry or a new signed plan after
confirmation; the reviewed plan remains immutable.

`create`: create a Gslb referencing a same-namespace Ingress or HTTPRoute.
`update`: change only the selected policy fields: strategy, TTL, primary and
weights. Local routing references, other strategy fields, operational metadata
and status are preserved. Weighted form mode maps to native roundRobin + weight.
Canonical and explicitly requested legacy Gslb APIs are supported.

No native delete/deactivate operation is advertised.
Deleting a resource in an authoritative DNS cluster is not a safe drain primitive.
Generated Gslbs must be edited through their owning route provider.

Creation from either provider lists explicit target clusters and pinned resources.

### DNS Zone Selection

New native Gslb and Traefik-enable requests carry `dnsZone`. The agent reads the
named k8gb controller Deployment on catalog, plan and apply. Configure
`--k8gb-namespace`, `--k8gb-deployment` and, if ambiguous, `--k8gb-container`.
`deploy/agent-dns-zones-rbac.yaml` grants only GET on that named Deployment;
application namespace permissions are not broadened. No Pods, Secrets or arbitrary
ConfigMaps are read. Literal `DNS_ZONES=parent:child:negativeTTL;parent:child2:negativeTTL`
produces `child` and `child2`. Negative TTL is not the service's DNS TTL.
Legacy literal `EDGE_DNS_ZONE` / `DNS_ZONE` is supported. References, shell-style
expansion and ambiguous containers fail closed; deploy literal controller env
values or extend discovery before using such configurations. Dynamic-only
`ZoneDelegation` inventories are not yet discovered by this reader.

The UI offers the intersection of discovered zones across selected clusters,
also narrowed by the Traefik adapter allowlist. Missing/unreadable discovery or
no common zone blocks creation. The server validates the chosen zone and every
declared hostname, and pins the controller Deployment UID/resourceVersion in the
signed plan. A changed controller requires a fresh preview. Source identity is
chosen by agent configuration, never by the browser.

Native creation references the existing Ingress/HTTPRoute without changing its
hostnames. Selecting a zone filters native routing targets; all target hosts must
be below that zone. To change a native target's DNS name, edit that routing
resource separately. Traefik creation combines an explicit DNS name and selected
zone into `k8gb-ui.io/hostname`; the reconciler uses exactly that hostname for the
generated native Ingress. No first-label inference from the local route is used
server-side. Local-child creation is not exposed by the standalone UI; existing
local annotations remain supported when editing legacy routes.

For a multi-zone adapter, configure matching `--traefik-domains` (agent) and
`--domains` (webhook) CSV allowlists. The original single `--traefik-domain` /
`--domain` remains compatible and adds one allowed suffix. Without an adapter
allowlist the agent still validates creation against controller-discovered zones;
the admission server itself does not query Kubernetes for DNS zones.
Matching eligible resources are selected together; review and change that selection
before previewing. Native targets must declare identical DNS hostnames. Each site
has an independent signed plan and apply result, not an atomic multi-cluster
transaction. The autoplay DNS diagram follows the unsaved policy and selected
sites; it is an illustration, not a live DNS measurement.

Native creation rejects targets already referenced by canonical or legacy Gslbs.
Traefik enable/update also rejects a hostname published by an independent Gslb in
the namespace. Labels do not grant adoption rights. These checks are not a
cross-resource reservation; avoid concurrent independent creators. See
[management handover](management-handover.md) for the proposed explicit migration
protocol. Automatic native-to-webhook migration is not implemented.

## Operational traffic API

Traffic operations use the same administrator-only signed plan/apply endpoints,
required reason, namespace authorization, exact UID/resourceVersion pins, dry-run,
audit and explicit partial-batch consent as settings. There is no automatic retry,
hidden namespace-wide selection, COD-to-IP mapping or direct DNSEndpoint write.

`provider: traffic`, `operation: overrideIPs` targets an existing Gslb (canonical
or legacy; native or adapter-generated). Supply `traffic: {ips: ["192.0.2.10"]}`.
Only 1..16 unique unicast IPv4 addresses are accepted: upstream application DNS
publishes A records. Hostnames, CIDRs, IPv6, ports, loopback and link-local targets
are rejected. Health checks still apply; this does not force an unhealthy site
into DNS or create a proxy/tunnel to another cluster.

The standard `k8gb.io/exposed-ip-addresses` annotation is updated. Because upstream
gives `k8gb.io/exposed-hostnames` higher precedence, it is removed for the duration
of an override. Both exact original values, including absence versus empty text,
are saved in `k8gb-ui.io/ip-override-state` on that same Gslb. Subsequent overrides
retain the first snapshot. No ConfigMap, agent memory or desired-state database
is needed to reset after restarting any replica.

`provider: traffic`, `operation: resetIPs` takes only the pinned Gslb and reason;
omit `traffic`, `policy`, `traefik` and `target`. It restores the snapshot only
if current installed override annotations still match it. External edits or a
malformed snapshot block both overwrite and reset. Unmanaged overrides are never
removed by guessing their original value. Deleting/recreating the Gslb loses its
snapshot and intentionally invalidates old plans.

An IP operation affects **all hosts of one Gslb**. Other Gslbs or clusters sharing
DNS names are not implicitly changed. Review the complete explicit target set;
Console accepts up to 32 independent target plans, not a cross-resource transaction.
An annotation update is not proof that controller reconciliation, authoritative
DNS publication, external DNS propagation or resolver cache expiry has completed.

`provider: traefik`, `operation: excludeSite` / `includeSite` takes the pinned,
enabled parent IngressRoute and reason, without policy or options. This is not
`deactivate`: Gslb and Ingress resources remain. The operation changes only the
parent's `k8gb-ui.io/site-excluded` annotation, with owned generated **global**
Ingress/Gslb dependencies also pinned. Admission still writes no children.

The adapter reconciler replaces only the owned global Ingress's synthetic backend
with a deterministic absent Service name derived from the parent UID. It creates
no such Service and refuses collisions. The live parent IngressRoute routing and
local GSLB siblings are untouched. Inclusion removes the marker and uses the
current desired backend, not a stale saved backend. Settings/backend updates
preserve exclusion and managed IP override snapshots until explicitly reversed.

This is **not graceful connection draining**: clients caching an old DNS answer
may fail at the excluded global Ingress until DNS caches expire. The read model
labels the intentional missing synthetic backend `Excluded`, not `NotFound`;
conflicting markers or Service collisions remain visible as unknown/warnings.
Catalog `siteTrafficEligible` independently checks the supported owned global
pair; ordinary catalog eligibility does not authorize exclusion.

### Native exclusion: local routing also stops

This deliberately disruptive operation is a separate opt-in:
`--enable-native-site-traffic` requires `--enable-writes`. Install
`deploy/agent-native-site-rbac.yaml` in each allowed namespace in addition to the
existing roles. Default read/write deployment permissions remain unchanged.

`provider: native`, `operation: excludeSite` / `includeSite` targets the pinned
ordinary Ingress in `resource`, with the observed standalone Gslb in `target`.
The initial protocol supports only a named, same-namespace Ingress resourceRef;
inline Gslbs, selectors, HTTPRoutes, shared targets and owned/generated resources
are rejected. No policy or traffic options accompany this request.

Exclude substitutes an absent synthetic Service for every HTTP-path backend of
that Ingress. **Local routing for all its hosts and paths stops too**, including
requests that bypass GSLB. The UI requires a separate local-impact confirmation
in addition to review and exclusion consent. This is not graceful draining:
cached DNS answers may send clients to a failing route until they expire.

The original rules and marker are snapshotted on the same Ingress, bound to its
UID and the Gslb UID, with a 48 KiB size limit. Include restores only that exact
managed exclusion; external backend changes, resource replacement or a synthetic
Service collision block automatic restoration. No Service or DNSEndpoint is
created or changed, and Gslb policy/IP overrides are preserved. The collector
reports a validated intentional backend as `Excluded`, not `NotFound`.

A DNS-only companion Ingress is not created: hostname-uniqueness admission may
reject it. A nonexistent resourceRef is not an alternative: tested k8gb v1.0.0
leaves the old DNS data when reconciliation fails. Weight zero is not a universal
exclude primitive for every strategy. Legacy `qaz` exclusions are preserved but
not automatically adopted by this new protocol.

## Optional Traefik provider/webhook

Build `./cmd/webhook` or the Dockerfile with `COMPONENT=webhook`. Nothing installs
or registers it automatically. The native agent neither needs nor checks it.

The adapter supports `enable`, `update` and `deactivate` on an existing
IngressRoute. Enable `--traefik-adapter` on the agent and configure the same
domain, GeoTags, entrypoints and optional TLS/CN settings on agent and webhook.
`--traefik-webhook-config` names the fail-closed registration checked during plans.

Admission is side-effect-free (`sideEffects: None`) and only validates/normalizes
the parent. `--reconcile` runs a separate periodic loop over the explicit
`--allowed-namespaces` list after parents commit. The loop creates Gslb/native
Ingress children with controller ownerReferences; it refuses collisions rather
than adopting independent resources. Deactivation removes only owned children.
Readiness confirms admission availability, not application/DNS convergence.

No corporate domain, IP, default primary, CA or plugin is assumed. Use an explicit
full GSLB hostname annotation; configured domains bound allowed hostnames rather
than discarding DNS labels. Historical `gslb_*` keys remain readable; new keys use
`k8gb-ui.io/`. See adapter tests for accepted route and annotation formats.
The selected route must resolve to a Kubernetes Service with a numeric port;
TraefikService graphs are not flattened. Route index is 0/1 via annotation or an
explicitly configured blocking middleware; no middleware name is assumed.

mTLS requires an existing strict Traefik TLSOption reference. DNS SAN checks additionally
require separately installed/configured certauthz; stock Traefik is not claimed
to enforce this DNS SAN filter itself. Turning off legacy mTLS without an ordinary
entrypoint snapshot requires explicit ordinary entrypoints; 443 is not guessed.

The reconciler preserves operational annotations/status and existing legacy
manual backend exclusion (`qaz`) while changing settings; it does not overwrite
DNSEndpoint targets. This is preservation compatibility, not a new drain API.
Existing externally managed derived objects are not silently adopted or renamed. Migrate
them separately to avoid competing old/new controllers.

## Audit and deployment

Plan/apply events are structured JSON on stdout. A per-agent in-memory history
keeps at most 700 KiB, with no fixed operation-count limit. It is a convenience
buffer: restarts clear it and replicas do not share it. Optional
`--audit-configmap=k8gb-ui-audit` instead reads/writes a shared namespace-local
700 KiB ConfigMap; provision the separate `agent-audit-configmap.yaml` first.
stdout remains independent, and application writes and audit persistence are not
an atomic transaction. No desired-state database is used.
Every Console requests authorized cluster-local histories on demand, in parallel,
with stable event IDs/order and explicit partial-source status. See
[federated audit and replica boundaries](audit-topology.md). A load-balanced
multi-replica agent should enable the shared ConfigMap mode; more Console copies
do not synchronize independently buffered memory. Journals are not replicated
between clusters; source outages remain explicit partial results.

The optional deployment templates need your own images, a TLS Secret and CA
bundle. Opt application namespaces in using `k8gb-ui.io/traefik-enabled: 'true'`
only after readiness. Repeat the reconciler RoleBinding per permitted namespace.
There is no automatic admission/cluster policy installation on Console startup.
The reconciler Role now additionally needs `get` on Services to reject synthetic
exclusion-name collisions; it does not need permission to create/edit Services.
Upgrade the agent, Console and optional webhook/reconciler together before
advertising the new operations. No additional domain/IP registry or token type
is introduced; the existing separate write credential still applies.
