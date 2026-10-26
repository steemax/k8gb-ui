# Deployment options and management modes

There are **two independent decisions**: where the web UI runs, and which
resource is the entry point for managing an application's GSLB configuration.
The optional webhook is not a different UI or a requirement for multiple sites.

## Components

| Component | Responsibility | Required where? |
| --- | --- | --- |
| Console | Browser UI, authentication, access bindings, change previews and federated audit | One cluster, or several Console sites |
| Site agent (`gslb-info`) | Reads local Kubernetes resources and executes explicitly enabled, scoped mutation plans | Every cluster being managed |
| Optional Traefik adapter/webhook | Validates source IngressRoute changes and reconciles its generated resources | Each cluster using this automation, not every cluster running a Console |
| k8gb and DNS | Evaluate application health and publish GSLB DNS answers | Participating GSLB clusters; supplied by the operator |

The chart installs the first three components according to values. It does not
install k8gb, DNS delegation, ingress controllers, CRDs, certificates, CA Secrets,
TLSOptions, certauthz or an identity provider. Native configuration remains in
Kubernetes, not in a Console database or a separate Git repository.

## A. One Console, many agents

```text
Browser -> HTTPS Console (east)
                 +--> HTTPS agent east -> Kubernetes east
                 +--> HTTPS agent west -> Kubernetes west
                 +--> HTTPS agent ...  -> Kubernetes ...
```

Install Console in one selected cluster and agents in all managed clusters.
Console does not have to be colocated with an agent; it needs network access to
every configured agent endpoint. The Helm chart can enable either component
independently.

This keeps UI deployment and credential distribution simple. You can increase
`console.replicas` within the Console cluster, but losing that whole site still
loses the UI endpoint. Existing application GSLB and DNS continue operating:
Console and agents are not in the application's DNS/data serving path.

For the [installation guide](installation.md), select
`CONSOLE_IN_WEST=false`: east has Console + agent; west has only an agent.
The remaining clusters follow the same agent-only pattern.

## B. Distributed Consoles, many agents

```text
Browser -> https://gslb.apps.example.test (GSLB)
                  +--> Console east --+
                  +--> Console west --+--> HTTPS agent east -> Kubernetes east
                                      +--> HTTPS agent west -> Kubernetes west
                                      +--> HTTPS agent ...  -> Kubernetes ...
```

Every Console connects to **all** configured agents, not just its local agent.
Run Console in every desired UI site, publish the same hostname at those sites,
and let the configured DNS policy choose the UI ingress address. Console sites
can be a subset of the clusters being managed. Multiple replicas within each
site are an additional, independent setting.

For the installation guide, select `CONSOLE_IN_WEST=true`. The Console service
and Ingress are installed in both sites using the same public hostname. The
chart does **not** create the Gslb publishing that hostname; see the bootstrap
example below.

### Shared-hostname requirements

- Same public HTTPS origin, auth mode and access bindings on every Console.
- Same cookie-signing/session key on those Consoles, distributed as Secrets by
  the operator. Built-in mode also needs the same users file; OIDC needs the
  same issuer/client settings and exact shared-hostname `/auth/callback` URL.
  Update the OIDC example's placeholder callback when using this guide's hostname.
- Same complete agent registry, zones and namespace scopes. Mount the appropriate
  per-site read credentials and, if enabled, write credentials on every Console.
- A certificate valid for the shared public hostname at every published Console
  ingress. The site certificates/private keys do not have to be identical.
- A **distinct, stable URL for each agent**. Do not route all agent URLs through
  one shared GSLB name: Console verifies the returned cluster identity, and
  requests must reach the intended Kubernetes cluster.
- All Console sites must be able to reach all participating agent endpoints.
  An unavailable source is partial/unknown, not healthy or an empty journal.

Console session and login-state cookies are signed; no Console-local session
database or sticky Console routing is required when configuration is consistent.
Use the same released application version on all copies. In-flight requests can
still fail during a site outage. DNS failover follows health detection and cache
expiry; it does not move established HTTP connections or make an ambiguous
mutation safe to retry. Inspect resources and audit before preparing a new plan.
Built-in login rate limits are per replica; a distributed ingress rate limiter
is an operator responsibility.

Agent replicas within **one** cluster use that cluster's stable credentials and
plan key. With more than one agent replica, enable its shared ConfigMap audit
journal. Different clusters retain independent journals; Consoles aggregate
and deduplicate them on demand. This is not cross-site replication or an archive
of a currently unreachable cluster. See [audit topology](audit-topology.md).

## Management modes

### Native Gslb: direct management, no adapter webhook

```text
UI -> site agent -> native Gslb -> k8gb -> DNS
                         |
                         +-> references existing Ingress or HTTPRoute
```

Choose this when your applications already have native routing resources and
you want to manage k8gb policy directly, without an additional admission webhook.
Traefik can still be your ordinary Ingress controller; **using Traefik does not
force use of the optional adapter**.

In UI, **Create native GSLB** selects an existing Ingress/HTTPRoute, DNS zone,
participating sites and policy. The agent creates a Gslb referencing that routing
resource. It does not generate the application route, Service or Deployment.
The routing target already supplies the hostname; native creation does not
rename it. Existing objects expose **Direct Gslb / Edit GSLB policy**.
This follows the upstream [resourceRef model](https://www.k8gb.io/latest/resource_ref/).

Updates change strategy, primary, weights and DNS TTL while preserving the
routing reference and other unmanaged settings. k8gb reconciles DNS. IP override
and reset are available; native site exclusion is a separate opt-in that also
stops local Ingress routing and requires explicit impact consent.
Native delete/deactivate is not offered by this release.

### Traefik webhook: manage the source route, generate GSLB resources

```text
UI -> site agent -> annotations on an existing IngressRoute
                           |
                    admission validates
                           |
                    source change commits
                           |
                    adapter reconciler
                           +-> global native Ingress
                           +-> Gslb referencing that Ingress -> k8gb -> DNS
                           +-> optional authorization Middleware
```

Choose this when applications use Traefik IngressRoutes and you want the source
route to be the GSLB configuration entry point. k8gb-ui bridges the supported
IngressRoute configuration to native resources understood by k8gb. It does not
add native IngressRoute support to upstream k8gb.

1. Create the application's IngressRoute and Kubernetes Service normally.
2. In **Create GSLB from IngressRoute**, select a supported route, DNS name/zone,
   target sites and policy. Select any approved TLS settings, preview and apply.
3. The agent updates GSLB annotations on each selected source IngressRoute.
   Admission validates the change without creating children, including on dry-run.
4. After the source commits, the reconciler creates/updates its global native
   Ingress and Gslb. Optional DNS SAN filtering also produces a plugin Middleware.
5. Subsequent UI edits target the source IngressRoute. Generated objects are
   reconciled automatically and are not independent native edit targets.

**The benefit:** you do not hand-maintain an additional DNS-facing Ingress and
Gslb for every application and site. Source route backend/settings and generated
resources converge through the adapter. Kubernetes annotations remain the source
of configuration; no extra desired-state store is introduced.

**The boundary:** the adapter handles its supported route subset, not arbitrary
Traefik configuration. For example, a TraefikService-only backend cannot be
represented as a native Ingress Service backend and is rejected, not guessed.
Application Deployments/Services and the original IngressRoute must already exist
in each selected cluster. You supply matching Traefik configuration, existing
TLSOptions/CA Secrets and, when needed, certauthz on every site.

Temporary site exclusion changes only the owned global route, preserving the
source IngressRoute; this is DNS withdrawal, not graceful connection draining.
Deactivating GSLB removes the adapter-owned generated resources while keeping the
source IngressRoute. These are different operations. Settings changes preserve
manual exclusion and managed IP overrides until explicitly reversed.

### Ownership and mixed environments

Both modes can coexist in the same UI and cluster for different objects. A
webhook-owned Gslb must be edited through its source IngressRoute; the native API
rejects direct writes to generated objects. Unknown ownership is not permission
to adopt a resource. Automatic conversion of an existing standalone Gslb to
webhook ownership is **not implemented**; see [management handover](management-handover.md).

Do not enable two reconcilers over the same source namespaces/resources or let
GitOps repeatedly overwrite fields you intend to manage in UI. Multi-cluster
operations use independent plans and results, not a distributed transaction or
automatic rollback of other sites. See [mutation safety](mutations.md).

## Four installation combinations

| Topology | Application management | Components |
| --- | --- | --- |
| A | Native only | Console at one site; agent at every site; adapter/webhook disabled |
| A | IngressRoute automation | Same UI placement; agent + adapter/webhook at participating Traefik sites |
| B | Native only | Console at multiple sites behind the shared address; agent at every site; adapter/webhook disabled |
| B | IngressRoute automation | Same distributed UI placement; agent + adapter/webhook at participating Traefik sites |

Read-only deployment is an additional permission choice, not another topology.
Start without writes. The [installation guide](installation.md) enables writes
and, independently, the optional webhook using separate value overlays.

## Publish Console through its own GSLB

This example uses **native Gslb publication for the Console endpoint**, even when
applications use the Traefik adapter. The chart-generated Console Ingress is
already a native routing target, so an extra Console IngressRoute is unnecessary.
Console publication does not need an admission webhook.

Prerequisites from the installation guide:

- `fullnameOverride: k8gb-ui`, release namespace `k8gb-ui`.
- Console enabled in the sites to be published, with a healthy
  `k8gb-ui-console` Service and same-named Ingress at each site.
- Ingress `host: gslb.apps.example.test` and
  `console.auth.publicURL: https://gslb.apps.example.test` on both sites.
- `apps.example.test` served and delegated by your existing k8gb/DNS setup.
  Set the matching shared-hostname OIDC callback when using OIDC.

After deploying both Consoles, create `$PRIVATE/console-gslb.yaml`:

```yaml
apiVersion: k8gb.io/v1beta1
kind: Gslb
metadata:
  name: console
  namespace: k8gb-ui
spec:
  resourceRef:
    apiVersion: networking.k8s.io/v1
    kind: Ingress
    name: k8gb-ui-console
  strategy:
    type: roundRobin
    dnsTtlSeconds: 30
```

Bootstrap it out of band with your administrative Kubernetes credentials, once
per participating Console site:

```sh
kubectl --context "$EAST_CONTEXT" create -f "$PRIVATE/console-gslb.yaml"
kubectl --context "$WEST_CONTEXT" create -f "$PRIVATE/console-gslb.yaml"
```

This is separate from Helm ownership and does not require adding the system
namespace to application users' agent allowlists. If a Gslb already publishes
this hostname, review and reuse its intended configuration rather than creating
a second publisher. For topology A, publish only the Console site; do not create
the west Console Gslb while Console is disabled there.

Verify each site locally first, then the authoritative DNS answers, the shared
HTTPS hostname and login continuity when requests reach a different site. A
Console process `/healthz` response does not prove every remote agent or identity
provider is reachable; verify those separately in UI. Keep a direct, trusted
administrative recovery path: publishing UI through the system it manages is
not a substitute for out-of-band Kubernetes access.

To publish Console itself through an IngressRoute instead, disable its
chart-generated Console Ingress, supply your own source route to the Console
Service, and deliberately opt into the adapter workflow. Do not create both
routes for the same hostname and assume conflicting ownership will be resolved
automatically. Secrets and that external publication configuration remain the
operator's responsibility.

[Install from GitHub Pages](installation.md) / [Back to README](../README.md)
