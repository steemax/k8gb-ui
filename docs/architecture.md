# Architecture

```text
Browser -> Console (OIDC or builtin + access bindings) -> authenticated per-cluster agent
                                                  -> Kubernetes read API
```

Console has no kubeconfig. The agent uses an explicit kubeconfig for local
development, or its own ServiceAccount in a cluster. Neither component is in
the application's data or DNS serving path. Configuration remains in Kubernetes;
only cluster registry and access policy live in Console configuration.

Console placement is independent of the management provider: one Console can
reach many agents, or distributed Consoles can share a public GSLB hostname and
query the same agents. Native Gslb editing and optional IngressRoute automation
work with either topology. See [deployment options and management workflows](deployment-options.md)
for diagrams, shared-session requirements and UI publication bootstrapping.

## Identity and grouping

- Cluster ID, display name and GeoTags are separate fields.
- A zone's participating clusters are explicitly configured. No cluster names,
  domain suffixes or IPs are synthesized from corporate naming conventions.
- Groups use namespace + matched zone + full hostname, not resource name.
  A multi-host Gslb generates a separate read-model entry for each hostname.
- A resource reference retains API version, kind, namespace, name, UID and RV.
- Resource-name ambiguity is surfaced, not silently resolved across clusters.

The registry currently replaces automatic ZoneDelegation discovery. Configure
only intended participants per zone. Longest matching DNS suffix selects the
zone. A hostname outside configured zones is unknown, not a missing deployment.

## Diagnostics

Missing objects in reachable participating clusters are warnings. Unreachable
agents, unavailable optional APIs and stale snapshots are unknown. Different
DNS target counts are information; different target IPs alone are not drift.
Backend endpoint readiness is separate from k8gb's application health.

There is no hidden fallback to a previous live snapshot at the server. The
browser can retain its last displayed snapshot after a refresh error, explicitly
marking it stale. Failed/partial collections must never look like healthy data.

## Transport and authentication

Read APIs are GET-only. Mutations use separate authenticated POST plan/apply
routes and remain disabled by default; Console additionally enforces administrator
bindings, same-origin JSON requests and per-target namespace access.
OIDC uses authorization code flow, PKCE, nonce and state. Signed session cookies
are Secure, HttpOnly and SameSite=Lax; no access/refresh tokens are sent to JS.
Application bindings constrain both cluster fan-out and namespace selection.
The agent independently enforces its deployment namespace allowlist.
Built-in password authentication uses the same access bindings without any
external identity provider. Passwords are salted Argon2id hashes in a mounted
users Secret/file; every request revalidates account state and current groups.
There is no automatic OIDC fallback. See [authentication](authentication.md).

Console verifies each agent's API version, cluster ID and requested namespace,
limits response size/time and refuses redirects. Server TLS verification is
mandatory; a custom CA and client certificate are independently configurable.
The bearer credential's scope is the entire configured agent scope: do not give
untrusted callers direct agent access or trust a claimed actor header.

## Management providers

`native` edits Gslb directly using validated plan/apply. `traefik` is an
optional adapter preserving the annotation-driven workflow. Its admission checks,
original-entrypoint metadata, temporary exclusion and IP override semantics must
not become assumptions of the native contract. The optional provider is registered
only when explicitly configured; native operation needs no webhook registration.

Deleting a Gslb while a cluster remains authoritative for a zone is not a generic
safe exclusion operation. Define and acceptance-test that operation separately.
Multi-cluster writes report partial/unknown results, not transactions. The
`traffic` provider writes standard per-Gslb exposed-IP annotations and stores
reset snapshots on the affected resource. It neither owns DNSEndpoints nor
changes native application backends. Temporary site exclusion has two separate
contracts: the Traefik provider reconciles only its owned global Ingress; the
separately enabled native site protocol snapshots and substitutes backends on an
ordinary named-reference Ingress, with explicit local-routing-loss consent.
Native restoration requires an exact managed snapshot and unchanged bound UIDs.
Neither path creates duplicate-host companion Ingresses or writes DNSEndpoints.

Audit uses structured stdout plus a 700 KiB in-memory convenience buffer by
default. Optional namespace-local ConfigMap history shares the 700 KiB retained
window across agent replicas, with CAS writes and fresh reads. Consoles aggregate
each cluster's independent journal on demand, without cross-cluster replication.
No mandatory log store or database is introduced. See audit-topology.md for
storage-failure and retention boundaries.
