# Two-cluster k8gb/DNS laboratory

Opt-in, **write-enabled local lab**, not an installer for an existing cluster.
It runs two independent kind clusters, real k8gb and CoreDNS, Traefik, the
optional adapter, and a healthy HTTP backend per site. No corporate domains,
credentials, default kube-contexts or host DNS configuration are used.

## Dependencies

Docker with roughly 8 GiB available memory, kind, kubectl, Helm, Go >=1.26.10,
Node.js >=18, OpenSSL and `dig`. Tested on Docker Desktop ARM64 with
`kindest/node:v1.36.1`, k8gb v1.0.0, k8s_crd v0.6.0, Traefik v3.6.10 and nginx
1.28-alpine. Local agent/webhook images are built but never pushed.

Use pinned public sources, not their moving default branches. Run harness
commands from this project's root:

```sh
git clone --depth 1 --branch v1.0.0 https://github.com/k8gb-io/k8gb /tmp/k8gb-v1.0.0
git clone --depth 1 --branch v3.6.10 https://github.com/traefik/traefik /tmp/traefik-v3.6.10
export LAB_STATE="$(mktemp -d /tmp/k8gb-ui-lab.XXXXXX)"
export DOCKER_CONTEXT=desktop-linux
export HELM_CACHE_HOME="$LAB_STATE/helm-cache"
export HELM_CONFIG_HOME="$LAB_STATE/helm-config"
export HELM_DATA_HOME="$LAB_STATE/helm-data"
helm repo add lab-coredns https://coredns.github.io/helm
helm repo add lab-externaldns https://kubernetes-sigs.github.io/external-dns
helm dependency build /tmp/k8gb-v1.0.0/chart/k8gb
docker pull registry.k8gb.io/k8gb-io/k8gb:v1.0.0
docker pull registry.k8gb.io/k8gb-io/k8s_crd:v0.6.0
docker pull traefik:v3.6.10
docker pull nginx:1.28-alpine
node tests/dns-lab/lab.cjs init --state-dir="$LAB_STATE" \
  --docker-context="$DOCKER_CONTEXT" --upstream=/tmp/k8gb-v1.0.0 \
  --traefik-crds=/tmp/traefik-v3.6.10/docs/content/reference/dynamic-configuration
node tests/dns-lab/lab.cjs install --state-dir="$LAB_STATE"
node tests/dns-lab/lab.cjs console --state-dir="$LAB_STATE"
```

Every Kubernetes command uses an explicit owned kubeconfig/context. The harness
never switches the workstation's current context. `install` reapplies initial
fixture annotations: **do not rerun it to restart Console**. Keep private tokens,
TLS keys, kubeconfigs and the entire runtime state directory out of Git.

## Manual testing

### Keycloak OIDC and Helm review

After builtin RBAC installation (below), an isolated Docker Keycloak can reuse
the same two-cluster fixtures and protected UI. Other Docker workloads are never
modified. The pinned lab image is `quay.io/keycloak/keycloak:26.5.2` in dev mode,
not a production identity-provider deployment.

```sh
node tests/dns-lab/keycloak.cjs install --state-dir="$LAB_STATE"
AUTH_MODE=oidc PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  node tests/dns-lab/builtin-rbac.browser.cjs --state-dir="$LAB_STATE"
node tests/dns-lab/keycloak.cjs status --state-dir="$LAB_STATE"
```

UI: **https://127.0.0.1:8792/?namespace=demo**. Identity provider:
**https://127.0.0.1:8844**, realm `k8gb-ui-lab`, client `k8gb-ui`. Both have private
lab certificates; Console explicitly trusts the issuer CA, never skips TLS.
Runtime credentials are in `$LAB_STATE/keycloak/credentials.txt`, outside source:
`admin` belongs to `platform-admins`; `demo-reader` belongs to `team-demo` and can
only read `demo`; `realm-admin-only` has the Keycloak realm administration role
but no bound GSLB group, so it gets no namespaces/write access. ID-token groups
come from a client-level group membership mapper (full path false). Exact HTTPS
callback, authorization code + PKCE S256; password grant is disabled.

The same browser/API matrix is run for both login modes: real reversible TTL
changes in two namespaces/sites, forbidden foreign reads and viewer writes,
authenticated subject in audit, retained preexisting policy/history. OIDC audit
subjects are immutable provider `sub` IDs, not display names. Sanitized results
are in `artifacts/oidc-rbac-live`; credentials/tokens/cookies are never recorded.

Switch modes explicitly; neither mode ever falls back to anonymous admin:

```sh
node tests/dns-lab/keycloak.cjs builtin --state-dir="$LAB_STATE"
node tests/dns-lab/keycloak.cjs oidc --state-dir="$LAB_STATE"
```

Separate session keys invalidate cookies when changing modes. OIDC Console
logout clears the local session only; upstream Keycloak SSO may immediately log
the same user back in. Use separate browser profiles or log out of Keycloak to
switch accounts. `lab.cjs stop` stops only the owned Keycloak/proxy and preserves
its database volume; it does not delete other Docker containers or trust stores.

The [Helm chart](../../charts/k8gb-ui/README.md) has builtin/OIDC,
read-only/writer agent and optional webhook profiles. An additional, isolated
install/upgrade check leaves review releases in `k8gb-ui-chart-review` on each
site without replacing the current UI or reconciling any application fixtures:

```sh
node tests/dns-lab/helm.cjs --state-dir="$LAB_STATE"
```

### Builtin users and RBAC, without OIDC

To replace the owned lab's anonymous loopback admin with real local accounts:

```sh
node tests/dns-lab/builtin-rbac.cjs install --state-dir="$LAB_STATE"
PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  node tests/dns-lab/builtin-rbac.browser.cjs --state-dir="$LAB_STATE"
node tests/dns-lab/builtin-rbac.cjs status --state-dir="$LAB_STATE"
```

Open **https://127.0.0.1:8792/?namespace=demo**. The old HTTP URL on port 8791
redirects GET/HEAD requests to HTTPS and rejects mutations; it no longer provides
anonymous admin access. The Console backend on 18891 also requires a valid
session. Browser certificate warnings are expected for the private, short-lived
lab CA; no certificate is installed into workstation trust. Console verifies its
two HTTPS agent endpoints against that CA, without an insecure transport override.
The loopback TLS proxies forward credentials; they do not inject users or roles.

Installation creates two randomly generated accounts. Read their passwords from
`$LAB_STATE/builtin-rbac/credentials.txt` (mode 0600, directory 0700):

- `admin` / `platform-admins`: admin binding for all configured clusters/namespaces;
- `demo-reader` / `team-demo`: viewer binding for `demo` on east and west only.

The mounted-format users file contains Argon2id hashes. Passwords, hashes, cookie
key, CA/private keys and config backups stay in the private runtime directory,
never the source/artifact tree. There are no default credentials or OIDC/Keycloak
dependencies. This lab-only installer does not provision production accounts.

Both agents keep their existing flags, two replicas and audit history; their
explicit namespace allowlist expands from `demo` to `demo,rbac-private`. The
admin wildcard does not bypass the Console registry, agent namespace limits or
Kubernetes ServiceAccount permissions. This test covers all **configured** scope,
not blanket access to system namespaces or every Kubernetes resource.

Two isolated native fixtures are installed per site: `demo/rbac-demo` and
`rbac-private/rbac-private`, backed by real Ingress/Service resources. Verification
uses HTTPS login and the actual Console/agent APIs. It checks reader read-only
capabilities and hidden write controls, foreign namespace overview/catalog/audit
403s, URL/header tampering, rejection of a stolen admin plan, and 401s without
authentication. Admin changes fixture TTL 5 to 6 and back to 5 through the UI in
both namespaces on both sites, with an authenticated `builtin:admin` audit actor.
Other application policies, certauthz settings and retained history are preserved.
Unknown apply outcomes are never retried. Inspect the fixtures after any failure.

Reports/screenshots go to `artifacts/builtin-rbac-live`, excluding credentials,
cookies and signed plan tokens. The TLS proxy and Console remain running for
manual testing. Restart this mode using `builtin-rbac.cjs start`, not
`lab.cjs console`. Baseline install/anonymous Console commands refuse to overwrite
authenticated mode. Existing unsigned mutation harnesses no longer authenticate;
use the logged-in UI. `certauthz.cjs probe` remains read-only and usable.
The usual `lab.cjs stop` also stops the owned TLS proxy when removing the lab.

### Optional mTLS fixture

With a Console rebuilt/restarted from the same source version, extend an owned
lab without resetting existing GSLB policies:

```sh
node tests/dns-lab/mtls.cjs install --state-dir="$LAB_STATE"
node tests/dns-lab/mtls.cjs verify --state-dir="$LAB_STATE"
```

`install` builds/reloads only local agent/webhook images, grants TLSOption reads
(no Secret access), adds the `secure-client` listener to the lab Traefik, and
creates an isolated `mtls-lab` IngressRoute, strict/weak TLSOptions and private
test PKI. It does not rebuild/restart Console. Traefik's lab Deployment uses a
Recreate rollout because of its single-node hostPort, causing a short local HTTP
interruption during install. No image is published or workstation trust changed.

`verify` changes only this fixture through Console plan/apply on both sites. It
checks trusted clients, no certificate and untrusted CA, verifies the server
certificate, exercises separate/shared entrypoints and ordinary HTTP restoration.
It leaves the fixture enabled on `secure-client`, visible in namespace `demo`.
The private PKI stays in `$LAB_STATE/mtls-pki`, never artifacts/Git. certauthz is
not installed by this fixture alone; use the optional extension below. Existing
shared DNS SAN patterns are retained by the mTLS round trip, but SAN-specific
authorization is not covered by that verifier. The read-only browser check
`tests/dns-lab/mtls.browser.cjs` handles both plugin-enabled and unconfigured labs.
See [mTLS setup](../../docs/traefik-mtls.md).

### Optional certauthz / DNS SAN fixture

After the mTLS fixture, install the pinned official
[`famedly/traefik-certauthz` v0.1.2](https://github.com/famedly/traefik-certauthz/tree/v0.1.2)
on **both** lab Traefik deployments and enable the same alias on agent/webhook:

```sh
node tests/dns-lab/certauthz.cjs install --state-dir="$LAB_STATE"
node tests/dns-lab/certauthz.cjs verify --state-dir="$LAB_STATE"
# Recheck the current fixture without any plan/apply or resource changes:
node tests/dns-lab/certauthz.cjs probe --state-dir="$LAB_STATE"
PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  ARTIFACTS_DIR=artifacts/certauthz-ui node tests/dns-lab/mtls.browser.cjs
```

Installation uses Traefik's public plugin loader, so the lab must reach the
plugin registry. It restarts only lab Traefik, agent and webhook deployments,
preserving replicas, GSLB policies and shared ConfigMap audit history. Traefik
uses a Recreate rollout because of its single-node hostPort; expect a short local
HTTP interruption. This is a lab extension, not automatic production installation.

Verification changes **only `demo/mtls-lab`** through Console plan/apply. First,
all four clients from the same trusted CA must pass strict mTLS without a SAN
filter. Then, with only `client.demo.k8gb.test` allowed, both sites and both
`secure-client` / shared `websecure` entrypoints must produce:

- permitted DNS SAN: HTTP 200;
- different DNS SAN, despite a matching Subject CN: HTTP 403;
- `client.demo.k8gb.test.attacker.test`: HTTP 403;
- no DNS SAN, despite a matching Subject CN: HTTP 403;
- no client certificate or a foreign CA: TLS rejection, not an HTTP 403.

The CA/server certificate is verified without `curl -k`. Requests use HTTP/1.1
to avoid macOS LibreSSL/curl obscuring rejected TLS alerts as HTTP/2 framing
errors. The test leaves `mtls-lab` enabled on `secure-client`, with strict mTLS
and the exact permitted SAN, available in the protected UI on port 8792 after
the authentication setup above (8791 redirects to HTTPS). Temporary
port-forwards are always stopped. Test certificates/keys stay under
`$LAB_STATE/mtls-pki`; only sanitized JSON is written to `artifacts/certauthz-live`.
No existing non-fixture application policy or prior audit record is replaced.

For a manual request, forward one cluster's Traefik port in a separate terminal:

```sh
kubectl --kubeconfig "$LAB_STATE/east.kubeconfig" \
  --context "kind-k8gb-ui-lab-<owner>-east" -n k8gb-ui-system \
  port-forward deployment/traefik 18443:8443 --address=127.0.0.1
curl --noproxy '*' --cacert "$LAB_STATE/mtls-pki/ca.crt" \
  --cert "$LAB_STATE/mtls-pki/client.crt" --key "$LAB_STATE/mtls-pki/client.key" \
  --connect-to mtls-lab.apps.k8gb.test:443:127.0.0.1:18443 \
  https://mtls-lab.apps.k8gb.test/
```

Use the actual owner from the lab state, not the default workstation context.
Replace `client.crt/key` with `other-san.crt/key` to observe HTTP 403. These are
short-lived private lab certificates, not credentials to distribute with the
project. SAN filtering applies to the generated global GSLB route; it does not
add a middleware to unrelated/local source routes.

### GSLB operations

After builtin/OIDC setup, open **https://127.0.0.1:8792/?namespace=demo**.
The initial loopback-only development Console uses **http://127.0.0.1:8791**;
that address becomes an HTTPS redirect when authenticated mode is installed.
Two baseline objects are installed in both east and west:

| Hostname | Management | Settings action |
| --- | --- | --- |
| `native.apps.k8gb.test` | Direct Gslb | Edit GSLB policy |
| `hello.apps.k8gb.test` | Webhook / Traefik | Edit source IngressRoute |

Native policy updates write only the Gslb strategy fields. Adapter settings
write source IngressRoute annotations; reconciliation maintains generated
Ingress/Gslb children. A generated child cannot be edited as a native Gslb.
Each site's detail shows its management mode and exact write target. Mixed
hostname groups retain separate per-site ownership modes.

The free Ingress `native-new` is available through **Create native GSLB**;
the disabled IngressRoute `hello-new` through **Create GSLB from IngressRoute**.
These create no Gslb until explicitly applied. To add fixtures to a lab created
before native support, without resetting existing fixture edits:

```sh
node tests/dns-lab/lab.cjs fixtures --state-dir="$LAB_STATE"
```

For adapter-owned global routes use **Exclude site / Include site**. Both modes
support **Override IPs / Reset IPs**. Every edit requires a reason, preview and
explicit confirmation.
Native exclusion is separately opt-in and also stops the ordinary local Ingress;
the [multi-cluster/native exclusion acceptance](#multi-cluster-creation-and-native-exclusion)
verifies its exclusion and restoration. Webhook exclusion preserves the source
IngressRoute. Loopback-only local admin mode is development-only; use the
authenticated builtin/OIDC setup for shared access.
The existing read-only demo on port 8790 remains independent.

Parent zone `k8gb.test` delegates `apps.k8gb.test` to both sites. Delegation is a
static lab fixture. Application A answers are generated by real k8gb from
backend health, cross-site discovery and exposed-IP annotations, not mocks.
This does not test a public DNS provider or external-dns.

```sh
dig @127.0.0.1 -p 15053 hello.apps.k8gb.test A +short
dig @127.0.0.1 -p 15054 hello.apps.k8gb.test A +short +tcp
dig @127.0.0.1 -p 15053 native.apps.k8gb.test A +short
curl -H 'Host: hello.apps.k8gb.test' http://127.0.0.1:18091
curl -H 'Host: hello.apps.k8gb.test' http://127.0.0.1:18092
```

DNS ports 15053/15054 serve east/west over TCP and UDP; 15353 is the parent DNS.
HTTP 18091/18092 shows `hello from east` / `hello from west`. Agent ports are
18791/18792. All published ports bind to `127.0.0.1`.

Docker node IPs appear in A answers. On macOS those IPs are inside Docker's VM;
use the forwarded HTTP ports above. Reserved override `203.0.113.77` has no HTTP
backend: its presence proves DNS publication, not application reachability.

TTL is 5 seconds; convergence is asynchronous. Exclusion changes only the
generated global backend, not its parent IngressRoute or local siblings. Cached
clients can still attempt an excluded route and fail. This is **not graceful
connection draining**.

## Repeatable acceptance

Read the real node IPs from `status`. The probe refuses pre-existing exclusions,
managed overrides, non-baseline policies and ambiguous fixtures. It uses only
Console's actual plan/apply API and real `dig`, with no kubectl writes:

```sh
node tests/dns-lab/lab.cjs status --state-dir="$LAB_STATE"
K8GB_LAB_EAST_IP=<east-node-ip> K8GB_LAB_WEST_IP=<west-node-ip> \
  node tests/dns-lab/probe.cjs --run-mutations --plan-delay-ms=10000 \
  --check-localtargets --report="$LAB_STATE/acceptance.json"
```

The report path must be new. Without `--run-mutations` it is read-only.
Acceptance requires **both authorities, over both UDP and TCP**, to match for
two consecutive rounds at every step:

1. Baseline: east + west IPs.
2. Exclude east: west IP only.
3. Include east: both original IPs.
4. Override east: `203.0.113.77` + west IP.
5. Reset east: both original IPs, exact original annotations restored.

The review delay spans several controller reconciles. Gslb status-only changes
do not invalidate a reviewed plan; configuration/UID changes do. The final
patch still checks the latest UID/resourceVersion atomically. The probe records
raw DNS replies, redacted plans, audit and convergence times. It never retries
unknown apply outcomes or clears overrides that existed before its run.
Do not manually edit the fixture during acceptance.

Coverage is round-robin IPv4 on the pinned release, not every strategy, IPv6,
external resolver caching, production HA, upgrades or a full version matrix.

## Browser management acceptance

With Playwright installed separately, this opt-in browser test changes native
policy to failover (primary west) on both clusters, confirms actual A answers,
restores round-robin, then changes/restores east's webhook TTL and verifies the
real DNS TTL. Every apply uses a 10-second review pause; no automatic mutation
retry or cleanup is attempted after failure. The initial policies must be the
unchanged round-robin / TTL 5 fixtures. Omit `--run-mutations` to inspect the UI
only. Screenshots are saved next to the exclusive-create report.

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright K8GB_LAB_WEST_IP=<west-node-ip> \
  node tests/dns-lab/management.browser.cjs --run-mutations \
  --report="$LAB_STATE/management-browser.json"
```

## Inline Workspace Review

The inline editor/audit and light/dark/mobile layouts can be reviewed without
changing any object. Existing native policies (including user-set failover) are
preserved. The optional `--run-webhook` changes east's webhook TTL by one second
and restores the original TTL, checking authoritative UDP/TCP DNS. It never
retries an unknown apply; inspect the report before manual recovery on failure.

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  node tests/dns-lab/workspace.browser.cjs
# Explicit opt-in for the reversible local demo webhook write:
PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  node tests/dns-lab/workspace.browser.cjs --run-webhook
```

Reports and screenshots go to `artifacts/workspace-review/` by default (override
with `ARTIFACTS_DIR`). This script targets only the documented loopback lab.

## Multi-cluster Creation and Native Exclusion

The explicit mutation test below creates `native-multisite` from `native-new`
and enables the `hello-new` IngressRoute on both sites. It checks real UDP/TCP
DNS, excludes only the new native east route, verifies that its local HTTP route
fails while west remains available, then includes east and verifies exact
Ingress restoration. Existing user-set native/webhook policies are preserved.

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  node tests/dns-lab/multisite.browser.cjs --run-mutations --state-dir="$LAB_STATE"
```

This is a one-time acceptance run on unused fixtures. Created demo GSLBs remain
available for manual tests. Re-running against occupied fixtures refuses to reset
them; inspect the recorded result after a failed or unknown apply. Reports and
screenshots go to `artifacts/multisite-review/` (or `ARTIFACTS_DIR`). The lab also
provides unused `native-next` and `hello-next` routes for further manual creation.

Native site operations need the separate `--enable-native-site-traffic` agent flag
and `agent-native-site-rbac.yaml`, mapped to `demo` by the lab's Kustomize overlay.
Production examples do not automatically grant these additional permissions.

`missing-reference.cjs --state-dir="$LAB_STATE"` is a separate disposable probe:
it verifies that a nonexistent Ingress reference leaves old DNS in pinned k8gb
v1.0.0, then deletes only its own UID-matched probe objects. It is not a native
site-exclusion implementation.

## Federated Audit Review

The lab installs the optional namespace-local ConfigMap journal and two agent
replicas per cluster. Each agent replica shares its cluster's `demo/k8gb-ui-audit`;
east and west still have independent journals, queried together by Console.

```sh
PLAYWRIGHT_MODULE=/absolute/path/to/playwright \
  node tests/dns-lab/audit.browser.cjs --state-dir="$LAB_STATE"
```

This read-only check starts temporary Console peers with the same cluster
registry in reversed order, compares exact retained events, and simulates an
unavailable west source with an owned local HTTP endpoint. It checks the partial
audit UI in light/dark/mobile layouts and always stops those temporary peers.
It does not restart agents or mutate Kubernetes resources. Reports are saved to
`artifacts/federated-audit/`. Existing agent history is intentionally preserved;
older agents may show `Retention not reported` until separately updated.

To verify replica-local writes and Pod restart persistence with real Kubernetes:

```sh
node tests/dns-lab/shared-audit.cjs --run-plans --state-dir="$LAB_STATE"
```

This creates planning-only audit events through every agent Pod (no application
apply), compares each cluster's replica journals, rolls the east agent Deployment,
and checks that the exact history survives. Application GSLB policy and traffic
settings must remain unchanged. Its temporary port-forwards are always stopped;
the two-agent-per-cluster deployment and audit history stay available for manual
testing. Reports go to `artifacts/shared-audit/`.

## Shutdown

```sh
node tests/dns-lab/lab.cjs stop --state-dir="$LAB_STATE"
```

Removes only recorded owned lab clusters, parent DNS container and Console.
Other Docker containers and clusters remain untouched. Keep the state directory
until cleanup completes: it is the ownership record. No automatic cleanup runs
when a test finishes, because the lab is also intended for manual use.
