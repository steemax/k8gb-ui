# Install the published Helm chart

Deploy an agent in every participating cluster and Console in one or several
sites. Each Console queries all configured agents; it does not store desired
GSLB state. Kubernetes resources remain the source of truth. The optional webhook
adapts Traefik IngressRoutes; native Gslb management does not require Traefik.

This guide uses two sites, `east` and `west`, with application namespace `catalog`.
Replace the contexts, GeoTags, namespace, controller locator, ingress class and
all `.example.test` names with your own. The commands target the **0.1.2** chart
and Docker Hub images, not a local build. Run them after the matching artifacts
are published, and confirm the chart version with `helm search repo` first.

## Choose your deployment

First select **where Console runs**, then independently choose **native or
IngressRoute-managed GSLB**. See [deployment options](deployment-options.md) for
diagrams and the benefits/limits of each management mode.

| Choice | Console in east | Console in west | Agents | Optional Traefik webhook |
| --- | --- | --- | --- | --- |
| **A: one Console site** | Yes | No | Both sites | Enable in each site that manages IngressRoutes, or leave disabled |
| **B: distributed Console sites** | Yes | Yes | Both sites | Same independent choice as A |

The commands below support both options through `CONSOLE_IN_WEST`. Start with A
by setting it to `false`; set it to `true` for B. This controls Console placement,
not agent deployment. Both Consoles use one public origin,
`https://gslb.apps.example.test`, in option B. Publishing that hostname through
k8gb is a separate bootstrap step, not a side effect of installing the chart.

## Cluster registry and completeness

The topology is **configured, not auto-discovered**. Helm writes
`console.clusters` into Console's configuration. Every Console uses that entire
registry, even when running beside a local agent. There is no separate cluster
count: two entries mean two configured sites, three entries mean three sites.
RBAC and namespace allowlists can reduce which entries a given user may query.

```text
Browser -> any Console -> east-agent -> east Kubernetes API
                      -> west-agent -> west Kubernetes API
```

Agents do not register themselves with Console, contact peer agents or read other
clusters' kubeconfigs. In-cluster agents use the local ServiceAccount. Installing
an agent in another cluster does not automatically add it to any Console.

| Setting | Meaning |
| --- | --- |
| `console.clusters[].id` | Stable, unique DNS-label ID chosen by the operator; must match that site's `agent.clusterID` and cluster IDs in access bindings |
| `console.clusters[].name` | Human-readable UI label; defaults to `id` when omitted |
| `console.clusters[].url` | Dedicated HTTPS address for this site's agent, reachable from every Console; never a shared endpoint that can route to a different site's agent |
| `console.clusters[].geoTags` | Actual k8gb GeoTag(s) reported by this site; e.g. east has `[east]`, west has `[west]`; these need not equal cluster IDs |
| `console.clusters[].zones` | Explicit served zones in which this site is expected to participate; used to group hostnames and compare site presence |
| `console.clusters[].namespaces` | Console-side namespace allowlist; additionally restricted by user bindings, the agent allowlist and Kubernetes RBAC |
| `agent.geoTags` / `webhook.geoTags` | Allowed policy GeoTags for the managed group; normally `[east, west]` in both sites in this example, not just the local tag |
| `agent.controller` | Local k8gb Deployment used to discover served DNS zones for creation; it does not supply the cluster registry |

For example, an operator ID `dc-a` and UI name `Region A` can correspond to the
k8gb GeoTag `east`: set `agent.clusterID: dc-a` and the matching Console entry to
`id: dc-a`, `name: Region A`, `geoTags: [east]`. Bindings use `dc-a`; primary and
weight policies use `east`. Helm kube-context names are workstation deployment
targets only and are not used as runtime cluster IDs.

**What "complete" means:** for the requested namespace and authorized sites,
Console matches each observed hostname to a configured zone (the longest matching
suffix). Sites declaring that zone are expected participants. An available agent
with a complete inventory but no matching Gslb produces a missing-object signal;
an unreachable agent or partial inventory produces an unknown-source signal,
not a confirmed absence. If the hostname matches no declared zone, expected
participation is unknown. This is a zone-level expectation, not a separate
per-service membership list.

An unregistered site cannot be detected as missing. Nor can Console prove that a
user's authorized subset is the full infrastructure. Keep the complete intended
registry on every Console and let bindings restrict users. Reading k8gb
`DNS_ZONES` discovers domains, **not** site names, peer agent URLs or credentials;
it does not populate or replace `console.clusters`.

## Which values are shared

The chart is the same in every cluster; the **effective values are not identical**.
Use common files with a small site override, applied last:

| Values / resources | East | West |
| --- | --- | --- |
| `agent.yaml` | Common agent settings | Same common settings, if namespaces/controller configuration match |
| `east.yaml` / `west.yaml` | `agent.clusterID: east`, east agent hostname | `agent.clusterID: west`, west agent hostname |
| `console.yaml` registry, bindings and auth settings | Full `[east, west]` registry | Same full registry when Console is enabled |
| Console placement | Enabled | Disabled in A; enabled in B |
| Agent auth Secret | East read/write tokens and plan key | Different west tokens and plan key; the Secret name may be the same |
| Console connection Secrets | Credentials for **both** agents | Credentials for **both** agents in B |
| Console users/session key | Shared across Console replicas/sites | Same values in B; not independently regenerated |
| Agent/webhook TLS Secret and CA | Valid for east endpoints/services | Valid for west endpoints/services; adjust the CA overlay if the issuer differs |
| Writes/Traefik overlays | Common when policy/configuration match | Common when policy/configuration match; otherwise supply explicit site overrides |

The examples below use `$PRIVATE/agent.yaml`, optional `console.yaml`,
`writes.yaml`, `traefik.yaml`, then `east.yaml` or `west.yaml`. Later `-f` files
override earlier ones; `--set` overrides them all. YAML lists, including
`console.clusters`, are **replaced, not merged by ID**: provide the full list in
the shared Console file, not a different one-entry list on each site.

These files are private installation inputs, not files automatically generated by
the chart. Application state remains in Kubernetes, not in this configuration.

## 1. Prerequisites and topology

- Kubernetes 1.26+ with working k8gb, its CRDs and DNS in each site. The current
  live acceptance test used Kubernetes 1.36.1, k8gb 1.0.0 and Helm 4.2.3 on ARM64;
  this is not a full compatibility matrix.
- `kubectl`, Helm, OpenSSL, explicit access to both cluster contexts, and
  permission to install the chart's RBAC resources.
- An existing ingress controller or TLS proxy. Console-to-agent requests require
  HTTPS with certificate/hostname validation; HTTP agent URLs are rejected.
- DNS and server certificates for the agent endpoints and the Console hostname.
  The chart does not install an ingress controller, cert-manager, CA or certificates.
- Existing application and k8gb-controller namespaces. The chart only creates its
  release namespace when `--create-namespace` is used.

| Site | Agent URL | Console URL |
| --- | --- | --- |
| east | `https://east-agent.example.test` | `https://gslb.apps.example.test` |
| west | `https://west-agent.example.test` | Same hostname in option B; no Console in option A |

Both agents must be reachable from every Console replica, including across sites.
Restrict agent ingress and HTTP backends to trusted Console traffic with your
firewall/NetworkPolicy or mesh. A bearer token is required even behind HTTPS;
optionally enforce client certificates at your TLS boundary. The chart does not
create these installation-specific network policies. Do not expose plaintext
Services publicly.

Choose context names from `kubectl config get-contexts`, then set:

```sh
export EAST_CONTEXT=your-east-context
export WEST_CONTEXT=your-west-context
export PRIVATE="$HOME/.config/k8gb-ui-install"
# A: false (one Console site); B: true (Consoles in both sites).
export CONSOLE_IN_WEST=false
umask 077
mkdir -p "$PRIVATE/east" "$PRIVATE/west"
chmod 700 "$PRIVATE"

helm repo add k8gb-ui https://steemax.github.io/k8gb-ui/
helm repo update k8gb-ui
helm search repo k8gb-ui/k8gb-ui --versions
```

Keep this directory outside Git. On **each** cluster create the `k8gb-ui` release
namespace if absent, and verify that `catalog` and the controller namespace
already exist. All Kubernetes commands below use explicit contexts.

```sh
kubectl --context "$EAST_CONTEXT" get namespace catalog k8gb
kubectl --context "$WEST_CONTEXT" get namespace catalog k8gb
kubectl --context "$EAST_CONTEXT" create namespace k8gb-ui
kubectl --context "$WEST_CONTEXT" create namespace k8gb-ui
```

Skip the respective `create namespace` command if it already exists; never
delete an existing namespace to retry installation.

## 2. Prepare agent credentials and TLS

Generate distinct read, write and plan-signing credentials for each site. The
first installation remains read-only; the extra credentials allow a deliberate
writer upgrade later. Do not regenerate these files on routine upgrades.

```sh
for site in east west; do
  openssl rand -hex 32 > "$PRIVATE/$site/token"
  openssl rand -hex 32 > "$PRIVATE/$site/write-token"
  openssl rand -hex 32 > "$PRIVATE/$site/plan-key"
done

kubectl --context "$EAST_CONTEXT" -n k8gb-ui create secret generic agent-auth \
  --from-file=token="$PRIVATE/east/token" \
  --from-file=write-token="$PRIVATE/east/write-token" \
  --from-file=plan-key="$PRIVATE/east/plan-key"
kubectl --context "$WEST_CONTEXT" -n k8gb-ui create secret generic agent-auth \
  --from-file=token="$PRIVATE/west/token" \
  --from-file=write-token="$PRIVATE/west/write-token" \
  --from-file=plan-key="$PRIVATE/west/plan-key"
```

Provision an `agent-tls` TLS Secret in `k8gb-ui` on each site using your PKI or
certificate manager. For existing certificate/key files, for example:

```sh
kubectl --context "$EAST_CONTEXT" -n k8gb-ui create secret tls agent-tls \
  --cert=/path/to/east-agent-fullchain.crt --key=/path/to/east-agent.key
kubectl --context "$WEST_CONTEXT" -n k8gb-ui create secret tls agent-tls \
  --cert=/path/to/west-agent-fullchain.crt --key=/path/to/west-agent.key
```

Certificates must cover the respective agent hostnames. For private issuers,
place the trusted public CA bundles in `$PRIVATE/east/ca.crt` and
`$PRIVATE/west/ca.crt`; never distribute a CA private key to Console. For publicly
trusted issuers, omit `caFile` and the `ca.crt` connection Secret entries in step 4.
There is no TLS verification bypass.

## 3. Install read-only agents in both clusters

Create `$PRIVATE/agent.yaml` with this content:

```yaml
fullnameOverride: k8gb-ui
console:
  enabled: false
agent:
  enabled: true
  namespaces: [catalog]
  geoTags: [east, west]
  authSecretName: agent-auth
  controller:
    namespace: k8gb
    deployment: k8gb
  ingress:
    enabled: true
    className: your-ingress-class
    tlsSecretName: agent-tls
```

Create `$PRIVATE/east.yaml`:

```yaml
agent:
  clusterID: east
  ingress:
    host: east-agent.example.test
```

Create `$PRIVATE/west.yaml`:

```yaml
agent:
  clusterID: west
  ingress:
    host: west-agent.example.test
```

These overrides intentionally contain only local identity and endpoint settings.
Add controller/namespace differences here if needed. For the two-site example,
both agents allow policy tags `[east, west]`, while each Console registry entry
below declares only that site's own reported GeoTag.

Use actual k8gb GeoTags, which need not equal cluster IDs. The controller locator
is used for DNS zone discovery. The agent reads literal `DNS_ZONES` from that
Deployment: the second colon-separated field is the served zone. It does not
resolve Secret/ConfigMap-backed environment values. See
[DNS zone selection](mutations.md#dns-zone-selection).

```sh
helm --kube-context "$EAST_CONTEXT" upgrade --install ui k8gb-ui/k8gb-ui \
  --version 0.1.2 -n k8gb-ui --create-namespace -f "$PRIVATE/agent.yaml" \
  -f "$PRIVATE/east.yaml" --wait --timeout 5m
helm --kube-context "$WEST_CONTEXT" upgrade --install ui k8gb-ui/k8gb-ui \
  --version 0.1.2 -n k8gb-ui --create-namespace -f "$PRIVATE/agent.yaml" \
  -f "$PRIVATE/west.yaml" --wait --timeout 5m
```

The default generated ClusterRole permits cluster-wide reads, **not writes or
Secret reads**. `agent.namespaces` restricts the API's namespace scope, not the
underlying Kubernetes read RBAC. For Kubernetes-level read isolation, set
`rbac.create: false` and provide your own namespace Roles/RoleBindings and the
required narrow controller Deployment permissions. See the
[chart RBAC reference](../charts/k8gb-ui/README.md#topology-and-profiles).

## 4. Add authenticated Console

The following uses built-in accounts, without OIDC or Keycloak. For OIDC instead,
use the [OIDC values profile](../charts/k8gb-ui/examples/oidc.yaml) and
[authentication guide](authentication.md#oidc-and-keycloak); explicit group/subject
bindings remain required in both modes.

For built-in users, generate two Argon2id hashes with the project helper (requires
Go 1.26.10+ on your workstation, **not in the clusters**). From a source checkout:

```sh
git clone https://github.com/steemax/k8gb-ui.git
cd k8gb-ui
go run ./cmd/password-hash > "$PRIVATE/operator.hash"
go run ./cmd/password-hash > "$PRIVATE/catalog-reader.hash"
openssl rand 32 > "$PRIVATE/session-key"
```

The helper prompts without echo; passwords are never command arguments. Create
`$PRIVATE/users.yaml`, replacing both placeholders with the generated full hashes:

```yaml
users:
  - username: operator
    passwordHash: 'REPLACE_WITH_OPERATOR_ARGON2ID_HASH'
    groups: [platform-admins]
  - username: catalog-reader
    passwordHash: 'REPLACE_WITH_READER_ARGON2ID_HASH'
    groups: [team-catalog]
```

Provision `ui-tls` in `k8gb-ui` on each Console site for `gslb.apps.example.test`,
as with `agent-tls` above. A needs these Console Secrets only in east; B needs
them in both sites. The loop respects the selected placement:

```sh
for context in "$EAST_CONTEXT" "$WEST_CONTEXT"; do
  if [ "$context" = "$WEST_CONTEXT" ] && [ "$CONSOLE_IN_WEST" != true ]; then
    continue
  fi
  kubectl --context "$context" -n k8gb-ui create secret generic ui-auth \
    --from-file=users.yaml="$PRIVATE/users.yaml" \
    --from-file=session-key="$PRIVATE/session-key"
  kubectl --context "$context" -n k8gb-ui create secret generic east-connection \
    --from-file=token="$PRIVATE/east/token" \
    --from-file=write-token="$PRIVATE/east/write-token" \
    --from-file=ca.crt="$PRIVATE/east/ca.crt"
  kubectl --context "$context" -n k8gb-ui create secret generic west-connection \
    --from-file=token="$PRIVATE/west/token" \
    --from-file=write-token="$PRIVATE/west/write-token" \
    --from-file=ca.crt="$PRIVATE/west/ca.crt"
done
```

The same users, session key, bindings and registry are needed on all Consoles
serving the same public hostname. Copy both sites' connection credentials to each
Console site. Do not put passwords, tokens or private keys in Helm values.

Create `$PRIVATE/console.yaml`:

```yaml
console:
  enabled: true
  authSecretName: ui-auth
  auth:
    mode: builtin
    publicURL: https://gslb.apps.example.test
    usersFile: /run/auth/users.yaml
    cookieKeyFile: /run/auth/session-key
    bindings:
      - {role: admin, groups: [platform-admins], clusters: ['*'], namespaces: ['*']}
      - {role: viewer, groups: [team-catalog], clusters: [east, west], namespaces: [catalog]}
  clusters:
    - id: east
      name: East
      url: https://east-agent.example.test
      geoTags: [east]
      zones: [apps.example.test]
      namespaces: [catalog]
      bearerTokenFile: /run/sites/east/token
      caFile: /run/sites/east/ca.crt
    - id: west
      name: West
      url: https://west-agent.example.test
      geoTags: [west]
      zones: [apps.example.test]
      namespaces: [catalog]
      bearerTokenFile: /run/sites/west/token
      caFile: /run/sites/west/ca.crt
  secretMounts:
    - {name: east, secretName: east-connection, mountPath: /run/sites/east}
    - {name: west, secretName: west-connection, mountPath: /run/sites/west}
  ingress:
    enabled: true
    className: your-ingress-class
    host: gslb.apps.example.test
    tlsSecretName: ui-tls
```

Set `zones` to your actual k8gb-served domains. Upgrade the same release, retaining
the agent values. The west command explicitly overrides `console.enabled`, so A
keeps an agent-only west deployment while B also starts Console there. Both
options still install agents in both sites.

```sh
helm --kube-context "$EAST_CONTEXT" upgrade --install ui k8gb-ui/k8gb-ui \
  --version 0.1.2 -n k8gb-ui -f "$PRIVATE/agent.yaml" -f "$PRIVATE/console.yaml" \
  -f "$PRIVATE/east.yaml" --wait --timeout 5m
helm --kube-context "$WEST_CONTEXT" upgrade --install ui k8gb-ui/k8gb-ui \
  --version 0.1.2 -n k8gb-ui -f "$PRIVATE/agent.yaml" -f "$PRIVATE/console.yaml" \
  -f "$PRIVATE/west.yaml" \
  --set console.enabled="$CONSOLE_IN_WEST" --wait --timeout 5m
```

Point the Console hostname to your intended ingress endpoint(s), then open
`https://gslb.apps.example.test`. A publishes only east; B can publish both
Console sites through the same k8gb hostname using the
[shared-hostname bootstrap](deployment-options.md#publish-console-through-its-own-gslb).
The example uses native Gslb publication for Console itself, regardless of
whether applications use native mode or the optional Traefik webhook.

Admin and reader should see only permitted namespaces;
**all mutations are still disabled**. Plain HTTP port-forwarding is not a supported
login path because authenticated sessions use Secure cookies.

## 5. Enable administrator writes and shared audit

First create the retained audit journal **once** in each allowed application
namespace on every site. For this example:

```sh
for context in "$EAST_CONTEXT" "$WEST_CONTEXT"; do
  kubectl --context "$context" -n catalog create configmap k8gb-ui-audit
  kubectl --context "$context" -n catalog label configmap k8gb-ui-audit \
    k8gb-ui.io/audit-journal=true
done
```

If a journal already exists, verify its label and preserve its data; do not delete
it or apply an empty `data` block. Helm grants access but does not own/reset this
runtime ConfigMap. It retains up to 700 KiB per namespace/site, without a fixed
operation count. Both Consoles query the site journals and deduplicate event IDs;
the ConfigMaps are not replicated between clusters. Stdout logging remains enabled.

Add `writeTokenFile: /run/sites/east/write-token` and
`writeTokenFile: /run/sites/west/write-token` to the respective cluster entries in
`$PRIVATE/console.yaml`. Create `$PRIVATE/writes.yaml`:

```yaml
console:
  writesEnabled: true
agent:
  writesEnabled: true
  replicas: 2
  audit:
    enabled: true
```

Rerun the two upgrade commands in step 4, adding `-f "$PRIVATE/writes.yaml"`
**after** the shared agent/Console files, before the site override, and keeping
`--set console.enabled="$CONSOLE_IN_WEST"` on west. A keeps Console disabled in
west while enabling its agent writes; B enables both Console and agent writes.
Only explicit `role: admin` bindings can write;
the reader remains read-only. Changes use preview/confirmation and have per-site
results, not an atomic distributed transaction.

Native Ingress exclusion is a separate opt-in `agent.nativeSiteTraffic: true`.
It also interrupts local Ingress routing and requires explicit impact consent;
it is not needed for native policy editing or IP override/reset.

## 6. Optional Traefik IngressRoute webhook

Skip this section for native-only operation. Install compatible Traefik CRDs and
the controller first. Do not install two managing webhooks over the same
namespaces/resources. Existing webhook-managed objects are not editable as native
Gslbs; ownership is checked by the API, not just hidden in the UI.

Provision a `webhook-tls` TLS Secret in `k8gb-ui` on each site. Its server certificate
must cover **`k8gb-ui-webhook.k8gb-ui.svc`**, derived from this guide's explicit
`fullnameOverride` and release namespace. Use your certificate issuer; an ingress
wildcard certificate is not sufficient for this Kubernetes service hostname.
Put the issuing public CA bundle in `$PRIVATE/webhook-ca.crt`, then encode it:

```sh
openssl base64 -A -in "$PRIVATE/webhook-ca.crt" > "$PRIVATE/webhook-ca.b64"
```

The example assumes the same trusted issuer on both sites. If issuers differ,
use a separate CA bundle for each site's upgrade. Supply only the public CA in
`caBundle`; the TLS private key belongs exclusively in the Secret.

Create `$PRIVATE/traefik.yaml`:

```yaml
traefik:
  enabled: true
  domains: [apps.example.test]
  ingressClass: traefik
  ordinaryEntrypoints: [websecure]
  mtlsEntrypoints: []
  tlsOptionNamespaces: [catalog]
  certauthzAlias: ''
webhook:
  enabled: true
  namespaces: [catalog]
  geoTags: [east, west]
  tlsSecretName: webhook-tls
```

Adjust the ingress class and entrypoints to your actual Traefik configuration;
the example does not enable mTLS or a plugin. Rerun the step 4 upgrades with both
the write and Traefik overlays:

```sh
helm --kube-context "$EAST_CONTEXT" upgrade --install ui k8gb-ui/k8gb-ui \
  --version 0.1.2 -n k8gb-ui -f "$PRIVATE/agent.yaml" -f "$PRIVATE/console.yaml" \
  -f "$PRIVATE/writes.yaml" -f "$PRIVATE/traefik.yaml" \
  -f "$PRIVATE/east.yaml" \
  --set-file webhook.caBundle="$PRIVATE/webhook-ca.b64" \
  --wait --timeout 5m
helm --kube-context "$WEST_CONTEXT" upgrade --install ui k8gb-ui/k8gb-ui \
  --version 0.1.2 -n k8gb-ui -f "$PRIVATE/agent.yaml" -f "$PRIVATE/console.yaml" \
  -f "$PRIVATE/writes.yaml" -f "$PRIVATE/traefik.yaml" \
  -f "$PRIVATE/west.yaml" \
  --set-file webhook.caBundle="$PRIVATE/webhook-ca.b64" \
  --set console.enabled="$CONSOLE_IN_WEST" --wait --timeout 5m
```

Admission validates without creating derived resources, including on dry-run.
The reconciler creates/updates the Gslb and global native Ingress only after the
source IngressRoute commits. It preserves manual exclusion and IP overrides.
For mTLS use existing `RequireAndVerifyClientCert` TLSOptions and CA Secrets;
DNS SAN filtering additionally needs certauthz installed in **every** Traefik.
See [Traefik TLS setup](traefik-mtls.md) before enabling those options.

## 7. Verify and upgrade safely

```sh
for context in "$EAST_CONTEXT" "$WEST_CONTEXT"; do
  helm --kube-context "$context" status ui -n k8gb-ui
  kubectl --context "$context" -n k8gb-ui get pods,svc,ingress
  kubectl --context "$context" -n k8gb-ui rollout status deployment/k8gb-ui-agent
  if [ "$context" != "$WEST_CONTEXT" ] || [ "$CONSOLE_IN_WEST" = true ]; then
    kubectl --context "$context" -n k8gb-ui rollout status deployment/k8gb-ui-console
  fi
done
```

With the optional component enabled, also check
`deployment/k8gb-ui-webhook` on each site. In the browser verify both agents are
available, the reader cannot modify objects or read another namespace, and admin
preview lists the intended sites. Test against a disposable service first and
query your actual k8gb DNS servers before and after each traffic change.

For every upgrade keep the **complete** chosen values files and per-site
overrides, including the Console placement flag; omitting the write/webhook
overlay can disable components. Do not
rerun the bootstrap credential/journal commands on ordinary upgrades. Auth,
connection and signing Secret changes require deliberate Deployment restarts.
Never overwrite a published chart or image version. Helm rollback does not
undo already committed application mutations or restore evicted audit entries.

### Add another cluster later

1. Prepare k8gb/DNS and the application namespaces in the new site, then install
   its agent with a new stable ID, dedicated URL and distinct credentials.
2. Mount that agent's connection credentials on **every Console site**. Add its
   full entry (ID, label, URL, local GeoTags, zones, namespace scope and credential
   paths) to the shared `console.clusters` list and add the corresponding Secret
   mount. Extend non-wildcard access bindings where appropriate.
3. Update allowed policy tags on participating agents/webhooks if the group gains
   a GeoTag. This allowlist does not register or configure a peer k8gb controller;
   configure k8gb and DNS separately as required by your topology.
4. Upgrade every Console deployment with the same updated registry. The chart's
   configuration checksum rolls Console pods when its configuration changes;
   credential-only Secret changes still require a deliberate restart.
5. Verify all expected sources and inventories. Existing services are not
   automatically copied to the new site: provision their routing/backend resources
   and explicitly select the new site when creating/applying GSLB configuration.

Do not remove an unreachable site from the registry just to hide its warning;
remove entries only when deliberately changing the intended topology.

Further configuration: [Helm reference](../charts/k8gb-ui/README.md),
[authentication](authentication.md), [mutation safety](mutations.md),
[audit topology](audit-topology.md), and [maintainer release process](releases.md).
