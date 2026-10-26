# k8gb-ui Helm chart

Deploy Console, the site agent and the optional Traefik webhook. k8gb and its CRDs
must already exist. Traefik CRDs/controller are needed only for the optional
adapter. The chart does not install Keycloak, certauthz, CA, TLSOptions or CRDs.

The chart requires Kubernetes 1.26 or newer. It has been checked with Helm 4.2.3
and a two-site kind/k8gb v1.0.0 lab; Helm 3 and a broader Kubernetes/k8gb matrix
are not yet validated. Treat 0.1.0 as a preview, not a production support guarantee.
It is licensed under [Apache-2.0](LICENSE).

## Images and chart repository

All components use `steemax/k8gb-ui` with distinct tags: `0.1.0-console`,
`0.1.0-agent`, `0.1.0-webhook`. The operator can override each component's
`image.repository`, `image.tag` and `image.pullPolicy`, plus `imagePullSecrets`.

The chart repository is hosted on GitHub Pages. Once the maintainer publishes
it, replace `GITHUB_OWNER` and `GITHUB_REPOSITORY` with the actual GitHub identity
(which need not match the Docker Hub account):

```sh
helm repo add k8gb-ui https://GITHUB_OWNER.github.io/GITHUB_REPOSITORY/
helm repo update
helm upgrade --install ui k8gb-ui/k8gb-ui --version 0.1.0 \
  -n k8gb-ui --create-namespace -f /absolute/private/console-values.yaml
```

Publish matching images before the chart. The repository's
[release guide](../../docs/releases.md) describes first-time Pages setup and
manual publication workflows. For an unpublished/local chart use the source
directory instead, as shown below.

## Topology and profiles

Install an **agent release in every participating cluster**. Install Console in
one or several sites with the same explicit cluster registry and access bindings.
Each Console independently federates the per-site journals; deduplication uses
cluster/event identity. There is no cross-cluster ConfigMap replication.

| Profile | Values |
| --- | --- |
| Console, builtin users | [builtin.yaml](examples/builtin.yaml) |
| Console, OIDC groups | builtin example + [oidc.yaml](examples/oidc.yaml) |
| Site agent, read-only native | [agent.yaml](examples/agent.yaml) |
| Admin mutation API | selected profile + [writes.yaml](examples/writes.yaml) |
| Traefik adapter + webhook | agent profile + [traefik.yaml](examples/traefik.yaml) |

Examples contain no credentials. Replace `.example.test`, namespaces, cluster
IDs, images, Secret names, ingress class and bindings with your own values.
Defaults deliberately fail until required authentication/registry values are
provided. `local` anonymous loopback mode is not a publishable chart profile.
Run the commands below from the repository root. Before publication, build the
images locally or load them into kind. Builds use Go 1.26.10 or newer; the Dockerfile
pins its Go builder and nonroot runtime by digest. Never reuse a published version
tag for different content.

```sh
make images VERSION=0.1.0
helm lint charts/k8gb-ui -f charts/k8gb-ui/examples/builtin.yaml
helm template ui charts/k8gb-ui -n k8gb-ui -f /absolute/private/console-values.yaml
helm upgrade --install ui charts/k8gb-ui -n k8gb-ui --create-namespace \
  -f /absolute/private/console-values.yaml --wait --timeout 180s
```

Run the analogous agent command with an **explicit kube-context** for each site
and its own private values. The chart never creates application/controller
namespaces. Create these before installation. `serviceAccountName` reuses an
existing account; `rbac.create: false` lets the operator manage all RBAC.

The generated read ClusterRole grants cluster-wide reads, even when
`agent.namespaces` restricts the agent API. It grants no Secret access. Application
write and audit permissions are namespace-scoped. For Kubernetes-level read
isolation too, use `rbac.create: false` and supply namespace Roles/RoleBindings,
plus the narrowly scoped controller Deployment/webhook reads you need.

## Credentials and network

- Console `authSecretName` mounts at `/run/auth`: builtin needs `users.yaml`
  (Argon2id hashes, see [authentication](../../docs/authentication.md)) and
  `session-key` (32+ raw random bytes); OIDC needs `session-key` and optionally
  `client-secret`/`issuer-ca.crt`.
- Use `console.secretMounts` for per-site `token`, optional `write-token`, CA and
  optional client certificate/key. Reference these paths in `console.clusters`.
  Do not put credential values in Helm values or ConfigMaps.
- Agent `authSecretName` needs `token`; writes also require **distinct**
  `write-token` and `plan-key` entries. Keep the signing key stable across agent
  replicas. Read and write tokens must not be interchangeable.
- With `console.writesEnabled`, add `writeTokenFile` to each writable cluster.
  This does not grant users write access: only explicit `role: admin` bindings do.
- Console mounts no Kubernetes API token. Agent/webhook API access never includes
  Secrets; mounting their own Secret volumes does not need a Secret-read Role.
- Console/agent Services are internal HTTP, never LoadBalancer/NodePort. Expose
  them through a trusted **HTTPS** Ingress or service-mesh proxy with an operator
  certificate. No TLS verification bypass exists. Restrict backend access with
  your network policies; these transport policies are installation-specific.
- If `ingress.enabled`, `host` and `tlsSecretName` are mandatory. The chart does
  not assume a particular ingress class, issuer or cluster domain.
- New auth/connection/signing secrets require a deliberate Deployment restart;
  no credentials or hashes are included in rollout checksums. Share session keys
  across Console replicas if sessions should survive switching between them.

## OIDC / builtin RBAC

Roles are application bindings, not Kubernetes or Keycloak administration roles.
Keycloak must emit a top-level array `groups` in the **ID token**: group membership
mapper, full path disabled, add to ID token enabled. The Console requests only
`openid`, so attach the mapper directly to the client or its default client scope.
Use the exact HTTPS callback and PKCE S256. Both login modes default to deny.

`auth.issuerCAFile` adds a private issuer CA to system roots; it does not turn off
TLS verification. OIDC logout clears the Console session only. The upstream SSO
session remains; use Keycloak logout or a separate browser profile to switch
accounts. OIDC group changes are reflected on a new login/session expiry.

## Shared audit journal

With `agent.audit.enabled`, **pre-provision** a ConfigMap named
`agent.audit.configMapName` in every `agent.namespaces` namespace, with label
`k8gb-ui.io/audit-journal: 'true'`. Use the
[bootstrap manifest](../../deploy/agent-audit-configmap.yaml); its initial `data`
can be empty. The chart creates the
namespace-scoped get/update Role and RoleBinding, **not the journal itself**.
Multiple agent replicas require this shared journal. Runtime limits remain
700 KiB per namespace/site, not a fixed operation count. Eviction is expected;
stdout remains the durable external logging integration.

Never adopt this runtime ConfigMap into Helm or replay an empty `data` block
over an existing journal. Install/upgrade/rollback/uninstall leaves its content
and lifecycle with the operator.

## Optional webhook and mTLS

`webhook.enabled` requires `traefik.enabled`, explicit namespaces/GeoTags, an
existing TLS Secret and base64 PEM `webhook.caBundle`. Issue the certificate for
`<fullname>-webhook.<release-namespace>.svc` (see rendered Service). No private key
is stored in values. Admission is fail-closed, side-effect-free, CREATE/UPDATE
only, and selected by the explicit namespace names. Only the background
reconciler changes committed derived resources. Do not install two managing
webhooks for the same namespaces/resources. When using a pre-existing webhook,
leave ours disabled and set `traefik.webhookConfigurationName` explicitly.

Traefik configuration must match across sites: ordinary/mTLS entrypoint lists,
TLSOption namespaces and optional certauthz alias. No internal local-domain mode
is enabled. Existing `RequireAndVerifyClientCert` TLSOptions and CA Secrets are
operator-managed. certauthz must be installed separately in every Traefik before
setting the alias; it filters **DNS SAN**, not Subject CN.

Native site exclusion is a separate opt-in `agent.nativeSiteTraffic: true` with
writes enabled: it also interrupts local Ingress routing. It is off by default.

## Verification

`go test ./tests/helm` renders and validates both auth modes, native/Traefik RBAC
profiles and unsafe-value rejections. The opt-in owned-lab acceptance is:

```sh
node tests/dns-lab/helm.cjs --state-dir=/absolute/owned/lab/runtime
```

It installs into separate review namespaces on east/west, tests read-only to
writer upgrades, two-replica ConfigMap audit, webhook admission dry-run and live
RBAC, and verifies existing demo/private workloads were not changed. Review
releases remain installed for inspection; nothing is pushed to a registry/Git.
