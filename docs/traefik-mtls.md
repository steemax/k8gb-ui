# Traefik mTLS and DNS SAN filtering

Traefik installation settings are assumed identical across participating sites.
The form has one shared EntryPoint and TLSOption selection, not per-site mappings.
Each agent still checks its own Kubernetes resources. A missing/ineligible policy
blocks that target; multi-cluster changes are not an atomic transaction.

## Installation policy

EntryPoint names are static Traefik settings. Configure an explicit approved
list on both the agent and webhook; arbitrary names and a shared TLS/mTLS
entrypoint are supported. The list is an operator declaration, not runtime
discovery or proof that a listener/Service port is reachable.

Agent example:

```text
--traefik-adapter
--traefik-mtls-entrypoints=websecure,secure-client
--traefik-ordinary-entrypoints=websecure,grpc,grpctls
--traefik-tls-option-namespaces=traefik
```

Matching webhook example:

```text
--mtls-entrypoints=websecure,secure-client
--ordinary-entrypoints=websecure,grpc,grpctls
--tls-option-namespaces=traefik
```

Supply only entrypoints actually installed in your Traefik. Omitting policy
namespaces restricts TLSOption discovery/references to the application namespace.
Apply `deploy/traefik-security-rbac.yaml` in each approved policy namespace,
adjusting ServiceAccount subjects. Agent needs GET/LIST on TLSOptions; webhook
needs GET for admission/reconciliation. Neither needs Secret permissions.
Traefik itself must be configured to resolve the selected policy and CA Secrets,
including any cross-namespace references required by your installation.

The legacy agent `--traefik-mtls-entrypoint` / `--traefik-mtls-tls-option` and
webhook `--mtls-entrypoint` / `--mtls-tls-option` remain fallbacks for old routes.
Prefer a clear `namespace/name` TLSOption reference. Historical
`namespace-name@kubernetescrd` is accepted only when unambiguous within approved
namespaces. File-provider options are not supported by this verified CRD selector.
Without an explicit entrypoint list or legacy TLSOption configuration, the UI
does not invent an available mTLS installation.

## Selecting a policy

When mTLS is enabled, choose:

- **mTLS entrypoint** from the approved shared list.
- **TLSOption** from existing Kubernetes objects. Only
  `clientAuthType: RequireAndVerifyClientCert` with valid nonempty CA Secret
  references and an observed UID/resourceVersion is eligible.
- Optionally, **DNS SAN patterns**, when the certauthz plugin is configured.

The form displays CA Secret names in the TLSOption's namespace, not certificate
contents. No CA or Secret is created, copied, read or synchronized by k8gb-ui.
The catalog does not prove these Secrets exist or contain the correct CA chain;
Traefik owns that validation and runtime loading. Identical Secret names on two
sites do not prove identical contents: distribute CA material independently.
Nonverifying/optional client-certificate policies are shown as unavailable.

New selections are stored on the source IngressRoute:

```yaml
metadata:
  annotations:
    k8gb-ui.io/mtls: 'true'
    k8gb-ui.io/mtls_entrypoint: secure-client
    k8gb-ui.io/mtls_tlsoption: traefik/client-auth
```

The API represents the latter fields as `traefik.mtlsEntrypoint` and
`traefik.tlsOption: {namespace: traefik, name: client-auth}`. The agent checks the
live policy and pins its UID/resourceVersion in the signed plan. Admission and
reconciliation independently recheck the policy. Read-only validation on dry-run
does not create/update resources; the webhook continues to declare sideEffects
`None`. Kubernetes policy admins can still change a referenced TLSOption or CA
outside this application, so restrict their write permissions independently.

Both the generated global Ingress and the source IngressRoute use the chosen
entrypoint and TLSOption. Server-certificate fields, certificate resolvers and
TLS domains on the source are preserved. The adapter snapshots original
entrypoints and TLS options in source annotations and restores them on mTLS
disable. An absent `tls` block is distinguished from `tls: {}`. Legacy routes
with unrecoverable prior TLS options require an explicit ordinary TLS policy;
the adapter does not guess a less restrictive one. Temporary site exclusion and
per-Gslb IP overrides are not reset by security changes.

Using one entrypoint for ordinary TLS and mTLS is valid, but every router for
the same SNI hostname/entrypoint must reference a compatible TLSOption. Conflicts
can cause Traefik to fall back to its default TLS policy. The application cannot
inventory all routers from other providers; avoid externally introduced conflicts
and review the Traefik runtime configuration before exposing a service.

## Optional certauthz plugin

`github.com/famedly/traefik-certauthz` filters certificate **DNS SAN entries**, not
`Subject CN`, and does not verify certificate trust. Configure that plugin in
Traefik first, then explicitly declare the same installed alias on all sites:

```text
agent:   --traefik-cn-plugin=certauthz
webhook: --cn-plugin=certauthz
```

The historical flag and `commonNames` API field remain for compatibility; UI
labels and errors use DNS SAN. Configuration uses the plugin's `domains` array.
Patterns support letters, digits, dots, hyphens and wildcards, matching the
plugin's domain-list parser; this is not an arbitrary regex field.
Filtering requires mTLS and an explicitly configured alias. The form labels
runtime plugin loading as unverified: declaring an alias is not installing the
plugin or proof that it loaded successfully. A missing plugin can prevent its
router from being built, not silently bypass the filter. The native Gslb provider
does not configure Traefik authentication.

For repeatable local runtime verification, the optional two-cluster
[certauthz lab extension](../tests/dns-lab/README.md#optional-certauthz--dns-san-fixture)
installs the pinned plugin and tests allowed/denied DNS SAN values using clients
from the same CA. It also checks matching CN without an allowed SAN, suffix
matching, strict trust rejection and a shared TLS entrypoint. This does not add
production plugin/CA provisioning or replace per-installation runtime validation.

References: [Traefik TLSOption](https://doc.traefik.io/traefik/reference/routing-configuration/kubernetes/crd/tls/tlsoption/),
[TLS/SNI conflicts](https://doc.traefik.io/traefik/reference/routing-configuration/http/tls/tls-options/),
[certauthz source](https://github.com/famedly/traefik-certauthz).
