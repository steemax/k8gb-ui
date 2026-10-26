# Federated Audit

## Read From Agents, Not Other Console Replicas

Every Console uses the same explicit cluster registry and asks each authorized
cluster agent for recent mutation events when Audit is opened or refreshed.
Requests run in parallel with bounded concurrency and per-source deadlines.
There is no Console-local mutation journal, background log replication, gossip,
leader election, or dependency on which Console received the user's request.

The agent that executes a mutation generates an event ID and emits the same event
to structured stdout and its retained history. Console merges source events,
deduplicates by cluster/event ID, and orders by UTC timestamp with stable cluster
and event-ID tie-breaks. Different lifecycle stages remain separate events.
Legacy events without IDs are retained, not guessed to be duplicates. Wall clocks
may differ across clusters; this ordering is not a distributed causal clock.

Two Consoles with the same registry, permissions, source state and retained time
window receive the same events. Concurrent writes or an unreachable agent can
produce different snapshots: the API reports `partial`, collection time and every
queried source, including unsupported/unavailable agents. A failure is never an
empty-success response. Namespace and cluster authorization apply before fan-out;
agent identity and event scope are validated before records reach the browser.

## Finding Events

Audit provides full-text search across object names, users, reasons, operation
names and before/after changes. Combine this with exact cluster, user, management
provider, operation and result filters. `Outcome unknown` is a separate result
filter: it does not hide the recorded lifecycle status. Time presets cover the
last hour, 24 hours or seven days; custom bounds use the browser's local time.
The default is all retained history, newest first.

Sort by time, object, displayed cluster name, user, operation or result, in either direction.
Pagination offers 25, 50 or 100 events per page. Filtering and sorting happen
before pagination and never change the underlying journal or server deduplication.
Filters survive refresh and reopening Audit within the same namespace in the
current page session; changing namespace resets them. They are not stored in a
URL or localStorage. Source coverage and retention warnings stay visible even
when filters match no records. Filters only search retained, authorized records,
not evicted events or external stdout archives.

## Deployment Requirements

- Publish any number of Console replicas, including behind their managed GSLB.
- Configure the same complete cluster registry and access bindings on every copy.
- Use a distinct, stable agent URL for each cluster, not the common GSLB hostname
  that could route an audit request to an agent with a different cluster identity.
- Agent access uses configured TLS/mTLS and read credentials. The browser never
  receives these credentials or contacts an agent directly.

## Default Memory Boundary

The existing 700 KiB buffer is per agent process, with byte-based eviction and no
fixed operation-count cap. Reads report its random startup identity, startup
time, storage/scope, retained bytes and namespace-specific eviction count. A
restart loses this history; stdout remains available to the operator's external
log collector. UI makes this limitation visible rather than claiming durability.

**Do not put independently buffered agent replicas behind one load-balanced URL
and expect identical audit history.** With the current memory implementation,
use one agent per cluster. More Console replicas do not solve or worsen this
agent-local storage boundary. Reads do not fetch historical stdout from dead Pods.

## Shared ConfigMap Mode

Install `deploy/agent-audit-configmap.yaml` in each participating application
namespace, replacing `app` with that namespace, before adding this agent flag:

```sh
--audit-configmap=k8gb-ui-audit
```

Every replica in one cluster uses the same ConfigMap name and cluster ID. The
example provisions an empty labeled ConfigMap and grants the agent only `get`
and `update` on that exact name in that namespace. No create/list/delete rights,
global ConfigMap permissions, extra ServiceAccount or desired-state database
are introduced. A custom name also requires updating the manifest's resourceName.
Do not replace its live data with an empty bootstrap object during upgrades.

The complete JSON journal is limited to **700 KiB per namespace per cluster**,
including journal metadata. There is no fixed record-count limit. Oldest events
are evicted by UTC time and event ID; a retained floor prevents an ambiguous
retry from resurrecting evicted history. Delayed events older than that floor
are outside the retained window. Keep cluster clocks synchronized.

Append re-reads the ConfigMap and uses resourceVersion compare-and-swap, with
bounded conflict retries. Stable event IDs prevent duplicate records after an
ambiguous Update reply. Every audit read fetches the shared Kubernetes journal;
it does not return a stale replica-local cache. ConfigMap UID and creation time
identify the retained history across Pod restarts. A deleted ConfigMap or a lost
cluster is not recovered from another cluster automatically.

stdout is emitted independently before persistence. Writes are best-effort, not
an atomic transaction with the application mutation: a successful mutation is
not rolled back because its audit write fails. Unsaved events have a separate
bounded local pending queue (700 KiB total per process), are retried on reads and
every five seconds, and get a final bounded shutdown flush. Persistence failure
is logged; reads fail explicitly rather than returning empty success. Known
pending/overflow records mark the source incomplete. A crash before persistence
can still lose pending records from the UI; the stdout collector is the durable
external sink. This is operational history, not a compliance-grade audit store.

Agents with explicit namespace allowlists check their provisioned journals on
startup. Without an allowlist, each used namespace must still be provisioned:
there is no automatic namespace scan or ConfigMap creation. `--audit-timeout`
bounds a persistence/read attempt (default two seconds, 100ms to ten seconds).

## Multiple Datacenters

For N clusters with R agent replicas in each, there are N independent journals
for the selected namespace, not N*R histories. Every Console reads these same N
journals and merges their events. An operation executed in west is recorded in
west's journal even when the user reached a Console in east. The optional
webhook and application controllers do not become log replication services.

An unavailable cluster makes its journal unavailable and the aggregate partial;
the reachable clusters remain queryable. This design does **not** provide a copy
of west's history in east during a west outage. That requirement needs a separate
replicated/central log backend, normally the existing stdout collector. A
permanent searchable/compliance archive belongs there, not in ConfigMaps.

This journal covers operations executed by the mutation API, not arbitrary
kubectl edits, admission changes or all Kubernetes audit events.
