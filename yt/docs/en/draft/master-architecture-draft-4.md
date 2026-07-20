<!--
Draft number: 4
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master read request execution

Read-path execution model, scheduling, and cross-cell freshness behavior.

## Executing read requests { #read-execution }

Read requests in the object service follow a distinct pipeline from mutations.
Unlike mutations, which flow through the Hydra mutation queue, reads are served
directly from the automaton state after a freshness synchronization step.

### Request entry point and RPC thread

Every `ObjectService::Execute` call arrives on the **RPC heavy thread pool** (the global
YT RPC dispatcher, `NRpc::TDispatcher::GetHeavyInvoker()`). This pool handles initial
request parsing, sub-request classification, and all asynchronous bookkeeping. The
heavy invoker is *not* the automaton thread, so initial processing does not block
mutations directly.

### Sub-request classification

A single `Execute` call is a batch that may contain multiple sub-requests. Each
sub-request is classified independently:

| Class | Condition | Execution path |
|-------|-----------|----------------|
| **LocalRead** | Non-mutating, target path resolves to the local cell. | LocalRead executor (dedicated thread pool, see below). |
| **LocalWrite** | Mutating, peer is leader. | Automaton thread via `AutomatonScheduler`. |
| **Remote** | Target path belongs to another cell (cross-cell). | Forwarded via RPC to the target cell's object service. |
| **Cache** | Non-mutating, response found in the two-level master cache. | Answered immediately without touching the automaton. |

## Master cache and cached cluster metadata { #master-cache }

The master cache is a separate server role that fronts the master's
`ObjectService` for cacheable, non-mutating reads. It is not a Hydra peer and it
does not own durable master state. Instead, each master-cache process registers
itself in Cypress under `//sys/master_caches/<address>` and exposes a caching
`ObjectService` for every known master cell. Clients discover these addresses
through `GetClusterMeta(populate_master_cache_node_addresses=true)`.

There are several object-read cache layers that can participate in one native
read:

1. **Native-client object-service cache.** A native connection may create a
   local `CachingObjectService` and route reads through
   `EMasterChannelKind::ClientSideCache` (enabled by
   `TConnectionOptions::EnableClientSideCache`, default **true**). This cache is
   process-local to the client, proxy, node, scheduler, or other component that
   owns the connection. It uses the cluster connection's
   `caching_object_service` settings and forwards misses to the ordinary
   `Cache` channel, which means the miss goes to an external `master_cache`
   process when master-cache discovery is available and falls back to a master
   follower otherwise. A hit at this level does not leave the client process.
2. **External master-cache process.** A separate `master_cache` server role owns
   another in-memory `ObjectService` cache and forwards misses to the upstream
   master. A hit at this level does not contact any master peer at all.
3. **Master-internal cache.** Every master peer owns an in-memory
   `ObjectService` cache (`/object_service_cache` in master profiling/Orchid)
   configured by the master's static `object_service/master_cache` section and
   guarded by the dynamic flag
   `//sys/@config/object_service/enable_two_level_cache` (default **true**).
   This level is inside the master process. A request still reaches the master
   service and performs the normal request-level synchronization, but cache-hit
   sub-requests are completed from the stored response instead of entering the
   local-read executor and instead of acquiring an automaton block for reading
   Cypress state.

All three levels are important. The native-client level removes repeated
cacheable reads inside long-lived component processes. The external level
absorbs cross-process RPC traffic and `SyncWithUpstream` work before it reaches
master peers. The master-internal level protects the automaton and local-read
threads from repeated cacheable reads even when clients talk directly to masters.

Each object-service cache level maintains an in-memory SLRU cache (default
capacity: **1 GB**, unless overridden in that level's config) keyed by:

- master cell tag;
- authenticated user, unless the request disables the per-user cache;
- target path;
- service and method;
- request body;
- the `suppress_upstream_sync` and
  `suppress_transaction_coordinator_sync` flags.

Only non-mutating sub-requests with a caching header are eligible for these
caches. The external master-cache service additionally rejects requests with
unsupported attachments rather than caching them accidentally. Mutating requests
with a caching header fail with a "cannot cache mutating request" error. On a
client-side or external-cache miss, that cache forwards exactly one sub-request
to the next upstream cache/master channel; on a master-internal miss, the
sub-request continues through the ordinary local-read path. The level that
executed the miss stores the response together with the response revision and
success flag, and then serves waiting callers from the stored entry. On a hit,
the response is returned directly from the cache level that was hit.

### TTL splitting and stale serving { #master-cache-ttl }

The request's caching header carries separate TTLs for successful and failed
updates (`expire_after_successful_update_time` and
`expire_after_failed_update_time`) plus an optional
`success_staleness_bound`.

When a native-client or master-cache process uses `CachingObjectService`, that
cache consumes only a fraction of the requested TTL locally before forwarding a
miss to the next level. The dynamic option
`//sys/master_caches/@config/caching_object_service/cache_ttl_ratio` defaults to
`0.5` for external master-cache processes; the native-client cache uses the
`caching_object_service` configuration embedded in its cluster connection. For
example, with a 20-minute successful-update TTL and a 0.5 ratio, the current
cache level considers its entry fresh for 10 minutes; the forwarded request
header is rewritten with the remaining 10 minutes so upstream cache levels can
own the rest of the freshness budget. If second-level caching is disabled for a
request, the current `CachingObjectService` uses the full TTL and does not split
it with the upstream level.

Expired successful entries may still be returned while a refresh is in flight,
but only within `success_staleness_bound`. Returning such a stale successful
entry also forces the upper cache layer's staleness bound to zero, so stale data
is not compounded across multiple cache levels. Failed entries are not served as
stale entries after expiration. A caller can also request a
`refresh_revision`; an entry with a revision not newer than this value is
evicted and refreshed.

This means cache validation is time/revision based, not invalidation-message
based: native-client caches, external master-cache processes, and the
master-internal object-service cache do not subscribe to every master mutation.
A metadata change becomes visible through the cache after the relevant cached
entry expires, after a revision refresh is requested, or after the caller
bypasses/suppresses the cache according to its read options.

### Native-client object and attribute caches { #native-client-object-cache }

The native-client object-service cache described above is transparent for
ordinary YPath reads that use `TMasterReadOptions` and choose
`ReadFrom = ClientSideCache` (or a higher-level helper that does so). If the
connection was created with `EnableClientSideCache = false`, the requested
`ClientSideCache` channel is downgraded to the `Cache` channel; if no external
master cache is configured or discovered, `Cache` is downgraded to a follower
master channel. Thus disabling client-side cache removes only the local
per-connection cache; it does not by itself disable the external master-cache
process or the master-internal cache.

Some native components also build **object attribute caches** on top of YPath
reads. `TObjectAttributeCache` stores parsed attribute dictionaries by Cypress
path and fetches misses via `TBatchAttributeFetcher`, which sends batched
`get <path>/@` or `list <dir>/@` requests with the same `TMasterReadOptions`.
This means one logical attribute lookup may be cached twice:

1. in the component's object attribute cache, keyed by the cache's logical
   object key/path and governed by its `TAsyncExpiringCacheConfig`
   (`expire_after_access_time`, `expire_after_successful_update_time`, and
   `expire_after_failed_update_time`);
2. in one or more object-service cache levels underneath, keyed by the YPath
   request and governed by the request's master-read TTLs.

Invalidating the object attribute cache only removes or refreshes that
component-local parsed value. For caches that support
`InvalidateActiveAndSetRefreshRevision`, the cache records a minimum
`refresh_revision` for the path, and the next fetch passes that revision down in
the caching header so lower object-service cache levels must refresh entries
that are not newer. Caches that do not pass a refresh revision rely on their own
expiration time plus the lower object-service TTLs.

Operationally, if an object attribute change must be observed immediately by a
long-lived native component, check all relevant layers: invalidate or wait out
the component's attribute cache, ensure the read is not served by the
client-side object-service cache, and account for external/master-internal cache
TTLs unless the caller uses a refresh revision or bypasses cacheable master-read
options.

### Serving cluster metadata and cell directories { #master-cache-cluster-meta }

Several components periodically call `GetClusterMeta` through the cache path to
populate process-local cluster metadata:

- node, cluster, medium, user, and feature directories;
- timestamp-provider and master-cache node addresses;
- the **master cell directory**, including primary and secondary master
  connection configs and per-cell roles.

For the master cell directory specifically, the native connection owns a master
cell directory synchronizer. It periodically sends
`GetClusterMeta(populate_cell_directory=true)` through a cache channel to the
primary master. Defaults are:

- sync period: **60 minutes**;
- retry period after a failed sync: **15 seconds**;
- cache TTL for a successful update: **20 minutes**;
- cache TTL for a failed update: **15 seconds**.

The primary master builds the cell directory from the multicell manager's master
cell connection configs and current roles. Each master-cache process subscribes
to directory changes in its own native connection; when a new secondary master
cell appears in that directory, it registers an additional caching
`ObjectService` for the new cell. Disappearing cells are treated as an alert,
not as an ordinary cache eviction path.

### Operational effect on adding cells and changing roles { #master-cache-cell-changes }

Adding a secondary master cell or changing a cell's roles is committed on the
masters first, but proxies, nodes, schedulers, master caches, and other native
clients can continue using their cached master cell directory until their
synchronizer refreshes it or an explicit sync is forced. The visible effects are:

- A newly added master cell is not routable through a given component until that
  component has learned the new cell directory entry.
- New roles such as `chunk_host`, `cypress_node_host`, or
  `transaction_coordinator` are not used for automatic placement or coordinator
  selection by a component that still holds an older cell directory.
- If roles are changed and the old cached directory says a cell is eligible for
  some role, clients may continue trying that role until their directory cache
  expires or synchronizes.
- Master-cache processes that have not yet synchronized the new directory do not
  expose a caching service realm for the new cell, so requests that rely on
  master-cache routing to that cell can miss the new cache layer or fail over to
  another route depending on the caller's channel policy.

For planned cell addition or role changes, treat the master cell directory cache
as part of the rollout. After committing the topology or role change, wait for
`GetClusterMeta`/master-cell-directory synchronizers to complete on the affected
components, or temporarily reduce the synchronizer period and successful-update
TTL before the change. Avoid assigning traffic-critical roles and immediately
assuming every component will route to them; propagation is bounded by the
synchronizer period plus cache TTL and by retry behavior during failures.

### Related dynamic configs and cluster connection { #master-cache-configs }

There are three different configuration planes involved in this path. They are
easy to confuse during incidents:

| Plane | Cypress path / source | Main effect | Propagation model |
|-------|-----------------------|-------------|-------------------|
| **Master server dynamic config** | `//sys/@config` | Changes behavior of master peers themselves, including multicell role descriptors at `multicell_manager/cell_descriptors/<cell_tag>/roles` and master-side object-service cache behavior. | Applied by master dynamic-config managers; mutations to this document are durable master state, but each option still has its own reconfiguration semantics. |
| **Master-cache dynamic config** | `//sys/master_caches/@config` | Reconfigures master-cache processes, including `caching_object_service`, `cache_ttl_ratio`, request throttling, cache capacity, and the master-cache process's own `master_cell_directory_synchronizer` override. | Read by master-cache dynamic-config managers; affects master-cache processes, not master peers. |
| **Cluster connection** | `//sys/@cluster_connection` plus the static cluster connection shipped in component configs | Describes how native clients connect to masters and auxiliary services. Its `secondary_masters` list is the persistent client-side source for master cell addresses; it also carries `caching_object_service` settings for native-client object-service caches and dynamic sections for directory synchronizers and other client caches. | Native components consume it through their cluster-directory / connection update policy and through cached `GetClusterMeta` directory refreshes. Some fields are only read when a connection object is created. |

The word "dynamic" in `//sys/@cluster_connection` means "stored in a dynamic
source" rather than "every field is hot-reloadable". Existing native connection
objects reconfigure only the fields that have explicit reconfiguration code.
For example, changing cache sizes, retry periods, and directory-synchronizer
intervals is generally safe and is expected to take effect after the owning
component reloads dynamic config. Changing structural connection data such as
master cell IDs, master addresses, or the `secondary_masters` list should be
treated as a topology rollout: components must learn the new directory and, for
some services, may need restart with updated static config.

Safe-to-change examples:

- Lower `master_cell_directory_synchronizer/sync_period` and
  `expire_after_successful_update_time` before a planned role change, then
  restore them after all components learn the change.
- Tune `//sys/master_caches/@config/caching_object_service/cache_ttl_ratio` or
  cache capacity to trade freshness and memory for upstream master load.
- Change secondary-cell roles in
  `//sys/@config/multicell_manager/cell_descriptors/<cell_tag>/roles`, provided
  you understand that clients with an older cached cell directory may keep using
  old role information until refresh.

Not safe as an isolated live tweak:

- Removing or reusing a master cell tag/cell ID. Cell tags are embedded in
  object IDs and must be globally unique.
- Removing a secondary master from `//sys/@cluster_connection/secondary_masters`
  while objects, chunks, transactions, portals, or clients can still reference
  it.
- Editing an existing table's `external_cell_tag` to move data. Existing chunk
  trees are not rebalanced by changing this attribute.
- Relying on a role removal to stop all traffic immediately. Cached directories
  and already-open channels may keep sending requests until refresh or restart.

Corner cases to account for:

- If a component lowers its synchronizer period but keeps a long successful
  cache TTL, `GetClusterMeta` may still be served from cache and return the old
  directory until the TTL expires.
- If a master-cache process has stale cell-directory metadata, it may not have
  registered a caching service for a newly added cell yet. Direct master routing
  can work while master-cache routing for the same cell is still unavailable.
- Disabling or bypassing one cache layer does not necessarily disable the
  others. `EnableClientSideCache = false` bypasses only the native connection's
  local object-service cache; disabling external master-cache discovery bypasses
  only the separate `master_cache` process; use
  `//sys/@config/object_service/enable_two_level_cache` for the master-internal
  object-service cache.
- Failed `GetClusterMeta` refreshes use the shorter failed-update TTL and retry
  period, but they do not make an old successful directory magically fresh; a
  component either keeps its last good directory or fails requests that require a
  missing cell, depending on the caller and channel policy.
- During read-only master maintenance, requests that require cross-cell
  synchronization can fail; avoid combining topology changes with read-only
  windows unless the procedure explicitly requires it.

To wait for a change to take effect, verify both master state and client-side
observation:

1. Check the authoritative state on masters, for example
   `yt get //sys/@registered_master_cell_tags`,
   `yt get //sys/@dynamically_propagated_masters_cell_tags`, and
   `yt get //sys/@config/multicell_manager/cell_descriptors/<cell_tag>/roles`.
2. Force or wait for the master cell directory synchronizer on affected
   components. Operationally this usually means waiting at least one successful
   synchronizer cycle after the relevant cache TTL, or temporarily lowering the
   period/TTL before the rollout.
3. Confirm that a fresh `GetClusterMeta(populate_cell_directory=true)` from the
   same route the component uses contains the new cell and roles.
4. For master-cache routing, also verify that `//sys/master_caches` contains live
   master-cache instances and use the same master-cache route to perform a
   cacheable read against the relevant cell.

### Freshness synchronization (SyncWithUpstream)

Before any local sub-request is executed, the peer must guarantee that it is not
serving stale data. On followers, `SyncWithUpstream` means contacting the leader,
learning the current committed sequence number, and waiting until all mutations
up to that point have been applied locally.

In multicell mode, there is an additional freshness step for secondary masters:
before serving local reads, a secondary cell may also need to synchronize with
the primary cell via Hive so that cross-cell metadata imported from the primary
is up to date. This primary-to-secondary sync may be needed even when the
secondary peer itself is leader in its cell.

These synchronization steps are performed on the RPC thread (not the automaton
thread) and add roughly 10–20 ms in practice, potentially more if the request
must also wait for the secondary cell to observe fresh state from the primary.

The sync can be suppressed by setting the `suppress_upstream_sync` flag in the
request header. This skips both the follower-to-leader catch-up and, in
multicell mode, the primary-to-secondary freshness step, allowing lower latency
at the cost of potentially stale local or cross-cell metadata.

### Session queuing and fair scheduling

After sync completes, ready sessions are pushed into one of two lock-free
MPSC stacks:

- `AutomatonReadySessions_` — for LocalWrite sessions.
- `LocalReadReadySessions_` — for LocalRead sessions.

A periodic executor (`ProcessSessions`) runs **on the automaton thread** at a
configurable interval (`process_sessions_period`). On each tick it:

1. Drains both stacks and moves sessions into per-user fair schedulers:
   - `AutomatonScheduler_` for writes.
   - `LocalReadScheduler_` for reads.
2. Runs write sessions by repeatedly dequeuing from `AutomatonScheduler_` and
   calling `RunAutomatonSlow()` until the tick's **yield timeout** elapses
   (`yield_timeout`, default ~10 ms).
3. Triggers a quantum of local-read execution (see below).

The two fair schedulers ensure that a single busy user cannot starve other users'
requests.

### LocalRead executor and automaton blocking

Local read sub-requests are executed on the **LocalRead thread pool** (size
controlled by `local_read_thread_count`), which is separate from the automaton
thread. However, reads must still access automaton state — which is only safe
while no mutation is being applied. This exclusion is enforced by
`TAutomatonBlockGuard`:

```
ProcessSessions() on automaton thread:
  1. Run write sessions (automaton) for up to yield_timeout.
  2. Acquire TAutomatonBlockGuard   ← blocks new mutation application
  3. LocalReadExecutor_.Run(quantum_duration)  ← LocalRead threads execute reads
  4. Release guard                  ← mutation application resumes
```

While the guard is held, the automaton thread waits for the `LocalReadExecutor`
quantum to complete. This means that **a burst of expensive read requests can
delay mutation application**. The quantum duration is bounded by
`local_read_executor_quantum_duration`. Reads that exceed this quantum are
suspended and rescheduled on the next `ProcessSessions` tick.

The `enable_local_read_busy_wait` flag makes the automaton thread spin-wait
instead of sleeping during the read quantum, which keeps automaton CPU counters
accurate but consumes the core continuously.

There is also a `LocalReadOffloadPool_` (`LocalReadOff`) used for
off-loading specific heavy sub-operations within a read (for example,
serialization of very large attribute maps). Its size is configured via
`local_read_offload_thread_count`.

### Per-user throttling

Two throughput throttlers gate session execution:

| Throttler | Scope | Effect |
|-----------|-------|--------|
| `local_read_request_throttler` | All non-root users, all local reads | Limits RPS of local read sub-requests globally per peer |
| `local_write_request_throttler` | All non-root users, all local writes | Limits mutation injection rate from user requests |

Both throttlers are acquired concurrently with per-user YT-level rate limiting.
When a session is throttled it suspends (releases the thread), and resumes when
the throttler token becomes available. Throttling does not block the automaton
thread.

### Read request complexity limits

To protect against requests that traverse huge Cypress subtrees, the object service
enforces **read complexity limits** (`enable_read_request_complexity_limits`). Two
dimensions are tracked per sub-request:

| Metric | Config key |
|--------|-----------|
| Nodes visited | `default_read_request_complexity_limits.node_count` |
| Total result bytes | `default_read_request_complexity_limits.result_size` |

Callers may request per-sub-request overrides via the `read_complexity_limits`
extension in the YPath header, capped at `max_read_request_complexity_limits`.
When a limit is exceeded, the sub-request fails with an error; other sub-requests
in the same batch are unaffected.

### Cross-cell reads { #read-cross-cell }

When a sub-request targets a path that lives on a different cell (detected via the
resolve cache), the session forwards it as a Remote sub-request to the target cell's
object service. Forwarding happens on the RPC thread, not the automaton thread.

The receiving cell serves the forwarded request exactly like a local one (sync,
queue, execute). The originating cell collects responses and assembles the final
reply.

Cross-cell reads add at least one extra round-trip and an extra
`SyncWithUpstream` on the target cell. In total, a cross-cell read may take
20–50 ms or more depending on inter-cell latency.

### Transactions in read requests { #read-transactions }

If a read sub-request carries a transaction ID:

1. **Local transaction**: the transaction is already on this cell; no extra sync
   is needed.
2. **Remote transaction** (coordinator on a different cell): the object service
   must replicate the transaction to this cell before executing the read. This is
   handled by `TTransactionReplicationSessionWithoutBoomerangs`. Replication is an
   asynchronous Hive message that must be applied before the read can run; this
   adds an extra Hive sync round-trip on top of `SyncWithUpstream`.

For mutating sub-requests, transaction replication uses boomerang mutations
(`TTransactionReplicationSessionWithBoomerangs`) to ensure the mutation and the
transaction arrive at the leader in a safe order.

The `suppress_transaction_coordinator_sync` header flag skips the transaction
coordinator synchronization for read requests, accepting a risk of reading a
state where the transaction's visibility on this cell may lag slightly behind the
coordinator.

### Summary: threads involved in read execution

```
Incoming RPC
      │
      ▼ [RPC heavy thread pool]
  Parse, classify sub-requests
  SyncWithUpstream (async wait)
      │
      ▼ Push to LocalReadReadySessions_ stack
      │
      ▼ [Automaton thread, ProcessSessions tick]
  Per-user fair-scheduler enqueue
  Acquire TAutomatonBlockGuard
      │
      ▼ [LocalRead thread pool]
  Execute read sub-requests (per-quantum)
      │
      ▼ [Automaton thread]
  Release TAutomatonBlockGuard
      │
      ▼ [RPC heavy thread pool]
  Assemble and send reply
```
