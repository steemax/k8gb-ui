# Security

## Reporting a vulnerability

Use the GitHub repository's **Security > Report a vulnerability** private channel
when it is enabled. Do not post tokens, certificates, private cluster addresses or
exploitable vulnerability details in a public issue. Maintainers must enable
private vulnerability reporting before the first public release.

## Verification

Use the Go version in `go.mod` or a newer patched, supported version:

```sh
go mod verify
go vet ./...
make test
make vulncheck
```

`make vulncheck` pins the official govulncheck tool and fails on reachable
vulnerabilities. Re-run it before release because the advisory database changes.
A successful SARIF export is not equivalent to a successful blocking scan.

As of 2026-10-23, source/package and symbol-aware binary scans find no affected
imported packages or reachable symbols. One module-only advisory remains:
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) covers the unmaintained
`golang.org/x/crypto/openpgp` package. The application imports Argon2, not OpenPGP;
OpenPGP is not linked into its binaries. No advisory is suppressed. A module-only
scan still reports this finding and must not be described as completely clean.

The Dockerfile pins the builder/runtime image digests, disables implicit Go
toolchain downloads, uses a nonroot runtime, and retains the binary symbol table
without DWARF for package-aware scans. These checks are not penetration testing
or a guarantee about every OS package in an image.

## Deployment boundaries

- Built-in and OIDC authentication both default to deny; writes require explicit
  administrator bindings, opt-in agent/Console flags and separate credentials.
- Agents are trusted, privileged service endpoints. Put them behind HTTPS and
  network restrictions; do not expose their internal plaintext listeners.
- The chart's read ClusterRole has cluster-wide visibility. The namespace API
  allowlist is not Kubernetes-level read isolation. Supply namespace Roles and
  set `rbac.create: false` when stronger isolation is required.
- Native exclusion interrupts the selected Ingress's local routing as well as
  DNS health. It is separately opt-in and requires explicit impact consent.
- Multi-cluster writes are independently planned/applied, not an atomic
  transaction. Unknown outcomes and unavailable audit sources remain visible.
- Audit ConfigMaps retain a bounded 700 KiB window per namespace/site. Forward
  stdout to external storage for durable retention or compliance requirements.
- Traefik mTLS requires operator-provided strict TLSOptions and CA trust.
  Additional DNS SAN filtering requires the certauthz plugin; it does not replace
  client-certificate trust validation.

See [authentication](docs/authentication.md), [mutations](docs/mutations.md) and
[chart configuration](charts/k8gb-ui/README.md) for setup details. The initial
release is a preview; a broader compatibility matrix remains to be established.
