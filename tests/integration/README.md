# Real kind acceptance tests

These tests are excluded from normal Go builds. Even with the `integration`
tag they skip unless `K8GB_UI_E2E=1` is set. They never create or delete a cluster,
install CRDs, use the default kubeconfig, or publish Git changes.

## Prerequisites

- An existing isolated kind cluster with context `kind-k8gb-ui-e2e-*` and a TLS
  API endpoint on a literal IPv4 loopback address (`127.*`).
- A dedicated absolute kubeconfig path in `K8GB_UI_KUBECONFIG`. Exec/auth-provider
  credentials, insecure TLS and kubeconfig proxies are rejected.
- Upstream canonical and legacy Gslb CRDs. Verified with upstream commit
  `330d33335981b9716bc8ca245660d90893eae272`.
- For TLS admission: Traefik IngressRoute, Middleware and TLSOption CRDs, verified with
  `v3.6.10`, plus a preloaded image containing the current webhook binary.
- Optionally the upstream `externaldns.k8s.io/v1alpha1` DNSEndpoint CRD, to exercise
  preservation of independently managed DNS objects. This case explicitly skips
  when that CRD is absent.

## Run

```sh
K8GB_UI_E2E=1 \
K8GB_UI_KUBECONFIG=/absolute/path/to/disposable-kind.kubeconfig \
K8GB_UI_E2E_WEBHOOK_IMAGE=k8gb-ui-webhook:e2e \
go test -tags=integration -count=1 -v ./tests/integration
```

Omit `K8GB_UI_E2E_WEBHOOK_IMAGE` to run native acceptance only. The TLS test uses
`imagePullPolicy: Never`; it cannot download an image as a side effect.

The TLS fixture creates a short-lived CA and serving key in memory, its own
Secret, Service and Pod, and a uniquely named MutatingWebhookConfiguration.
The registration uses `failurePolicy: Fail`, `sideEffects: None`, and an exact
namespace selector for the newly allocated namespace. Cleanup deletes only that
registration and namespace, with UID preconditions. The cluster is left running.
The webhook Pod runs without a service-account token or reconciliation loop;
the test explicitly invokes the exported Go Reconciler after proving admission
and Plan/Apply have not created children.

## Coverage And Limits

Native tests use real server-side dry-run and both Gslb API groups for create,
update, noop, required reason, namespace restrictions, tampered plan tokens,
stale UID/resourceVersion, post-plan drift, changed targets and object replacement.

TLS tests exercise real API-server admission, rejected requests, entrypoint
snapshot/restoration, provider Plan/Apply, owned Ingress/Gslb/Middleware creation
and deactivation, idempotent reconciliation, native-child edit rejection, and
preservation of manual `backendqaz`, IP/hostname annotations, independent native
Gslbs, strategy extensions and canonical `serviceHealthPolicy`.

Operational tests exercise reversible IP overrides for both API groups, restart
recovery from the Gslb annotation snapshot, post-plan drift, native settings
preservation, and TLS-admitted site exclude/settings/include with the latest
backend. Local siblings and live parent routing remain unchanged. Synthetic
Service collisions block exclusion; API success is not DNS convergence.

The pinned upstream schemas do not define legacy `spec.loadBalancers`. Kubernetes
prunes that unknown field; this suite does not claim to verify its preservation.
It does verify retained operational IP/hostname annotations instead.

No k8gb controller is installed by this suite. Passing results establish Kubernetes
API/schema/admission and explicit adapter-reconciliation acceptance, not DNS
propagation, external traffic, controller health or production readiness.
