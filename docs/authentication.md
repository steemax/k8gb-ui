# Authentication and access policy

Choose one authentication mode explicitly. There is no anonymous network mode,
shared default password, self-registration or automatic fallback when an identity
provider is unavailable. The same application bindings protect the read APIs,
mutation plan/apply and federated audit in both authenticated modes.

| Mode | Identity source | Intended deployment |
| --- | --- | --- |
| `oidc` | Verified OpenID Connect ID token | Keycloak or another OIDC provider |
| `builtin` | Local users file with Argon2id hashes | No external identity service |
| `local` | Unauthenticated `local` identity | Literal loopback development only |

## Shared authorization

Bindings match exact subjects or group strings, then restrict clusters and
namespaces. Access is denied by default. An omitted role means read-only;
`role: admin` explicitly permits writes within that binding's scope. It does not
remove the separate `writesEnabled`, per-cluster write token, agent allowlist or
Kubernetes ServiceAccount RBAC requirements. Bindings are additive.

```yaml
bindings:
  - groups: [platform-admins]
    role: admin
    clusters: ['*']
    namespaces: ['*']
  - groups: [team-catalog]
    role: viewer
    clusters: [east, west]
    namespaces: [catalog]
```

An administrator of Keycloak, Kubernetes or the reverse proxy is not implicitly
an administrator of this application. Request headers cannot supply the user,
groups or audit actor. Console sends only authorized requests to agents and
replaces any caller-supplied mutation actor with its authenticated subject.
Bindings currently support cluster/namespace scope, not per-service permissions.

## OIDC and Keycloak

An optional `auth.issuerCAFile` adds a private issuer's PEM CA to the system
trust pool, with normal hostname/certificate verification still required. The
same HTTP client verifies discovery, token exchange and signing keys.

Start from `examples/console.oidc.yaml` (read-only deployment) or
`examples/console.writes.yaml` (explicit writes). Register exactly the configured
HTTPS `/auth/callback` URL, use Authorization Code flow, and mount the client
secret and a random cookie key as files. Public clients can omit clientSecretFile
when supported by their provider. Console always uses PKCE, state and nonce and
verifies the ID token signature, issuer, audience and expiration.

For Keycloak group bindings, add a **Group Membership** protocol mapper to the
client's dedicated scope or a default client scope:

- Token Claim Name: `groups` (or the exact configured `auth.groupsClaim`).
- Add to ID token: enabled. An access-token-only mapper is insufficient.
- Full group path: disabled for bindings such as `platform-admins`; alternatively
  use exact paths such as `/platform-admins` in the bindings.
- Make this a default scope: Console requests `openid`, not optional group scopes.
- Check the evaluated ID token contains a top-level string array, for example
  `"groups": ["platform-admins"]`. Nested realm/client role claims are not read.

A missing groups claim grants no group permissions; explicit subject bindings
can still match. A malformed groups claim is rejected. Role changes in the
identity provider take effect on next login/session expiration, at most one hour
with the current session lifetime (or earlier ID-token expiration).

## Built-in accounts without OIDC

Use `examples/console.builtin.yaml`. Set `auth.publicURL` to the actual HTTPS
Console origin, `auth.usersFile` to a mounted users file and `auth.cookieKeyFile`
to a random key containing at least 32 raw bytes. HTTPS is terminated by a trusted
ingress/proxy; do not expose the Console's plaintext backend directly.

Generate each password hash locally. The command prompts without terminal echo,
asks for confirmation and prints only a salted Argon2id PHC string:

```sh
go run ./cmd/password-hash
```

It also accepts bounded stdin for automation, never command-line passwords.
New passwords must contain 12..1024 bytes. The helper uses Argon2id with 19 MiB,
two iterations and one lane. Imported Argon2id v19 hashes must use 19..64 MiB,
2..4 iterations, 1..4 lanes, a 16..64 byte salt and a 32-byte result. Plaintext,
bcrypt, weak or unbounded parameters are rejected rather than silently migrated.

Use the placeholders in `examples/users.builtin.yaml` as a template:

```yaml
users:
  - username: operator
    passwordHash: '$argon2id$...generated hash...'
    groups: [platform-admins]
  - username: catalog-reader
    passwordHash: '$argon2id$...different generated hash...'
    groups: [team-catalog]
    disabled: false
```

These are placeholders, not usable default credentials. Usernames are
case-sensitive, 1..64 ASCII letters/digits or `_.@-`, starting with a letter or
digit. The authenticated subject is `builtin:<username>`; a subject binding must
use that prefix. Users have no implicit role: only the shared bindings grant
permissions. Keep usernames stable; changing a username changes its audit identity.

Mount the users file from a Secret rather than including it in images or Git:

```sh
umask 077
openssl rand 32 > session-key
kubectl -n k8gb-ui create secret generic console-auth \
  --from-file=users.yaml=./users.yaml \
  --from-file=session-key=./session-key
```

Console does not create, update or synchronize this Secret. The users file is
strict YAML/JSON, at most 1 MiB and 1000 accounts. Mount it as a Secret volume,
not a `subPath`, to receive Kubernetes updates. Each authenticated request reads
the mounted file: removed/disabled users cannot use their sessions; changing a
password hash permanently invalidates previous sessions. Rotate the hash when
re-enabling/recreating an account if old cookies must not become valid again.
Group changes update permissions without login. Missing/invalid
files fail closed. Revocation starts when the updated Secret reaches each pod,
not when the Kubernetes update request is submitted. Binding changes require a
Console restart. No separate credentials database is used.

Login uses a short-lived signed CSRF cookie and form token plus exact configured
Origin checks. Session cookies are signed, Secure, HttpOnly and SameSite=Lax,
last at most one hour, and contain no passwords or password hashes. Their payload
is not encrypted. Local sessions are distinguished from OIDC sessions and cannot
be reused after switching authentication modes. Sign-out clears browser cookies;
a copied session cookie remains usable until expiration or account/key revocation.

Brute-force protection is bounded per Console replica: five initial attempts per
account and direct peer IP, then one per ten seconds, a global rate limit and at
most two concurrent password checks. Forwarded IP headers are not trusted. Behind
a proxy all requests can share a peer-IP bucket. Add ingress-level distributed
rate limiting for production/multiple replicas; restarting a replica clears its
rate limits. Built-in authentication currently has no MFA or self-service password
reset; use OIDC where those controls are required.

## Multiple replicas and sites

Distribute the same auth mode, access bindings, users file and cookie key to every
Console serving the same public hostname. The common key allows a session to
work on any replica/site; the current users file supplies current local groups.
Different OIDC issuers/clients must not share a cookie key. Rotating the cookie
key and restarting every replica invalidates all sessions. Authentication config
distribution is an operator responsibility, separate from federated audit; audit
aggregation does not replicate credentials across Kubernetes clusters.

## Development-only access

`auth.mode: local` is intentionally not local account authentication. It permits
an unauthenticated browser only when Console binds to a literal loopback IP.
Writes additionally require `auth.localAdmin: true`. Do not publish it through
an ingress or proxy. To run without Keycloak in a shared environment, use
`builtin`, not `local`.

An opt-in [real two-cluster builtin RBAC laboratory](../tests/dns-lab/README.md#builtin-users-and-rbac-without-oidc)
exercises admin policy updates, read-only namespace isolation, API tampering and
audit identity using generated private local accounts. It replaces anonymous
loopback admin access with authenticated HTTPS; it does not relax the agent
HTTPS requirements or distribute credentials with this repository.

References: [Keycloak protocol mappers](https://www.keycloak.org/docs/latest/server_admin/index.html#_protocol-mappers),
[OWASP password storage](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html).
