# Contributing

Open an issue before making a large behavioral change. Include the k8gb,
Kubernetes, ingress-controller and Helm versions in bug reports; redact cluster
addresses and credentials. Report security issues privately as described in
[SECURITY.md](SECURITY.md).

Use the Go version in `go.mod`, Node.js 24+ and Helm 4.2.3. Before submitting:

```sh
go mod verify
go vet ./...
make test
make vulncheck
```

Keep changes focused. Authorization and mutation changes need tests for both
successful and denied requests, stale resource versions and partial failures.
The [DNS lab](tests/dns-lab/README.md) is opt-in and must target disposable,
explicitly selected clusters. Never run mutation tests against an implicit
workstation context.

User-facing text and documentation are in English. Do not commit tokens,
private values, kubeconfigs, runtime state or generated reports. Retain license
notices. Contributions are provided under the project's [Apache-2.0 license](LICENSE).
