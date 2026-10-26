# k8gb-ui demo

[Quick interface overview](k8gb-ui-overview.gif) (0:40, looping GIF, English
captions): resources, native/webhook creation, DNS policies and change previews,
site/IP controls, mTLS, audit, RBAC and themes.

[Watch the full demo](k8gb-ui-demo.mp4) (2:40, 1080p, English captions, no audio).

| Time | Feature |
| --- | --- |
| 00:00 | All GSLBs in one workspace |
| 00:08 | Create native GSLB across both sites |
| 00:19 | Create GSLB from IngressRoute |
| 00:32 | Round robin |
| 00:37 | Weighted round robin |
| 00:42 | Failover and recovery |
| 00:55 | GeoIP and fallback |
| 01:00 | Update policy across both sites |
| 01:08 | Exclude a site without deleting GSLB |
| 01:17 | Real DNS convergence |
| 01:21 | Include the site again |
| 01:27 | Override published IP addresses |
| 01:35 | Reset IPs to the original snapshot |
| 01:42 | Native exclusion also stops local routing |
| 01:51 | Restore the native Ingress |
| 01:57 | mTLS and DNS SAN authorization |
| 02:03 | Real certificate checks on both sites |
| 02:07 | Disable GSLB, not just one site |
| 02:15 | Enable GSLB again |
| 02:25 | Audit: who changed what and where |
| 02:31 | OIDC groups and read-only access |
| 02:36 | Light and dark themes |

Recorded in the [local two-cluster lab](../../tests/dns-lab/README.md).
DNS policy animations are illustrative models, not live traffic telemetry.
The tour also verifies actual DNS A responses over UDP/TCP, mTLS/DNS SAN
authorization and namespace-scoped access. Native site exclusion affects local
Ingress routing; webhook-managed exclusion preserves the source Traefik route.

[Back to the project](../../README.md)
