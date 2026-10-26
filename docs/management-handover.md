# Management handover

Status: design for a separate, explicit operation. Automatic adoption is not
implemented. Enabling an IngressRoute is not a migration command.

## Current boundaries

- Direct management changes a standalone Gslb policy.
- IngressRoute management changes its source annotations. The optional adapter
  reconciles owned Gslb, Ingress and middleware children.
- Native policy APIs reject controller-owned objects and known ownership markers,
  even if the caller bypasses the browser.
- Native creation refuses an occupied routing reference. Catalog eligibility is
  advisory; live inventory is checked again during planning and applying.
- IngressRoute enable/update and adapter reconciliation reject another Gslb
  claiming the hostname in the same namespace. A collision is not permission to
  delete or adopt that object.

These checks are not a cross-resource/cluster transaction. A concurrent creation
can race an inventory check. Cluster admission/ownership policy is needed for
strict uniqueness; the UI never promises atomic multi-cluster commits.

## Proposed workflow

1. Select the existing Gslb, proposed IngressRoute source and explicit target
   clusters. Pin every identity by namespace, API, name and UID.
2. Read canonical/legacy Gslbs, routing targets and DNS ownership. Prove hostname,
   backend, port, TLS, strategy, primary, weights and TTL compatibility. Reject
   unsupported multi-host/routing configurations rather than approximating them.
3. Identify and stop the previous GitOps/controller writers. Verify the optional
   adapter and its namespace-scoped permissions are ready.
4. Preview the complete handover. Preserve the existing Gslb name/UID and its
   DNSEndpoint ownership; do not create a second publisher for the same hostname.
5. Introduce an explicit adoption/external-reference contract in the adapter.
   Current child-name hashing and cleanup rules cannot safely adopt arbitrary
   native objects by simply adding an ownerReference.
6. Persist a versioned handover snapshot on the Kubernetes resource, not in a UI
   database. Bind original ownership, reference and annotations to the source UID.
7. Commit through reviewed per-cluster plans, with an audit record and explicit
   administrator confirmation. Surface partial and unknown outcomes. Never retry
   an ambiguous apply or silently roll back another cluster.
8. Verify reconciliation, single DNS publisher and authoritative/peer DNS answers.
   API write success alone is not migration success.

Rollback must stop adoption reconciliation and restore only unchanged,
snapshot-matching fields. `gslb: false` is not a rollback command: it removes
owned generated children. OwnerReference garbage-collection semantics must be
reviewed before adopting an existing native Gslb.

## Native site exclusion

A missing Ingress reference is not exclusion: on k8gb v1.0.0 reference resolution
fails before the DNSEndpoint is saved, leaving previous DNS answers in place.
The local lab probe confirms this after four TTL periods.

A DNS-only companion Ingress can isolate health manipulation from live routing,
but is not a portable default: hostname-uniqueness admission may reject the
duplicate even with a separate ingressClass. Such policy must not be bypassed.

The opt-in direct native mechanism instead changes the existing Ingress backend
Service names. It affects **all local hosts and HTTP paths on that Ingress**, not
only DNS clients. It must have a separate local-impact confirmation, a managed
snapshot, conflict detection and conditional restore. It must not modify the
actual Service, EndpointSlice, Gslb policy or IP override.

A cleaner universal DNS-participation switch requires upstream k8gb support,
which is separate from this UI/adapter project. Directly patching generated
DNSEndpoints competes with k8gb reconciliation and is not offered.
