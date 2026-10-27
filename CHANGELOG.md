# Changelog

Versioned images and Helm charts are published separately through the manual
[release workflows](docs/releases.md).

## 0.1.2

### Changed

- DNS policy diagrams show manually excluded sites in both reported-policy and
  unsaved-form views. Excluded sites remain visible but receive no modeled
  answers; weighted shares, failover and GeoIP scenarios use eligible sites only.
- Excluded sites use muted, dashed nodes and remain distinct from simulated
  failures. An excluded failover primary stays excluded until explicitly included;
  an all-excluded group displays a static "No eligible sites" state.
- Creation/settings previews retain existing per-route exclusions and use the
  matching fresh catalog state when an observed Gslb snapshot lags behind.
- Added unit and browser coverage for all balancing modes, weighted shares,
  exclusion/inclusion transitions, pending reconciliation and desktop/mobile themes.

### Release

- Chart version, appVersion, component image defaults, workflow input and install
  examples target 0.1.2. Existing published versions are not replaced.
- Agent/webhook behavior, mutation APIs, authentication, RBAC and configuration
  are unchanged. No new credentials, permissions or migration are required.

## 0.1.1

### Added

- Administrator bulk site exclusion and inclusion for native Gslbs and
  Traefik-managed global routes. Select objects in card or row view, choose
  clusters, and review signed plans before applying changes.
- Card selection using checkboxes or right-click, selection persistence across
  views and filters, and explicit counts for selected objects hidden by filters.
- Fresh routing-state checks, already-in-state skips, shared-route deduplication,
  and per-resource results. Each operation supports up to 32 routing targets.
- Mandatory reason, explicit partial-apply consent and a separate native Ingress
  local-routing impact confirmation. Failed or unknown outcomes are not retried
  automatically; a fresh preview skips targets already in the requested state.
- Deactivation preview regressions for both supported Traefik API groups,
  with and without mTLS. Preview preserves existing children; cleanup follows
  committed source deactivation and background reconciliation.
- Frontend unit and browser coverage for bulk operations, read-only boundaries,
  failed catalog refreshes, and light/dark desktop/mobile layouts.

### Release

- Chart version, appVersion, component image defaults and installation examples
  target 0.1.1. The image-publication workflow defaults to the same version.
- Authentication, RBAC, mutation APIs, webhook behavior and read-only defaults
  are unchanged. No new credentials, permissions or data migration are required
  for this update. Existing opt-in native site-traffic permissions still apply.

## 0.1.0

- Initial independent multi-cluster Console, per-site agent and optional Traefik
  webhook preview, with native Gslb management, plan/apply, traffic controls,
  authentication, RBAC, federated audit and Helm installation.
