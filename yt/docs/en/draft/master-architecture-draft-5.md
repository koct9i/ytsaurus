<!--
Draft number: 5
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master performance and administration

Operational bottlenecks, monitoring, snapshots, and scaling guidance.

## Performance considerations { #performance }

### Automaton thread bottleneck

All persistent mutations on a single cell are applied serially on the automaton thread. The automaton thread is the primary throughput bottleneck. Monitor the metric `yt_resource_tracker_total_cpu{service="yt-master", thread="Automaton"}` — sustained load above 90% indicates the cell is under pressure.

Heavy single mutations (e.g. full heartbeats from nodes with hundreds of thousands of chunks) have been split into smaller batches to avoid blocking the automaton thread for too long.

### Mutation backlog and commit latency { #mutation-backlog }

Mutation latency for a single request is the sum of all pipeline stages:

```
latency ≈ serialization_wait + changelog_write + network_rtt_to_followers
          + automaton_queue_wait + automaton_apply_time
```

The most common sources of elevated latency are:

**1. Serialization wait (batching delay)**

Mutations accumulate in the draft queue until `SerializeMutations` runs. By default this executor fires every 5 ms (`mutation_serialization_period`). If the system is lightly loaded, a mutation submitted between two ticks simply waits up to 5 ms before it is even serialized. Enable `minimize_commit_latency: true` to trigger flush immediately after each serialization pass, trading slightly higher throughput for lower tail latency.

**2. Automaton queue depth**

After a mutation is committed (quorum reached) it is placed in a list to be applied by the automaton thread. If the automaton thread is already busy applying a large mutation, all subsequent mutations wait. The queue of committed-but-not-yet-applied mutations is visible in the Hydra monitoring endpoint as `last_offloaded_sequence_number - automaton_sequence_number`. A large gap here indicates automaton thread saturation.

**3. Mutation queue limits and restart**

The leader maintains a bounded in-memory queue of logged mutations that have not yet been confirmed as received by all peers (needed to retransmit to lagging followers). The queue is bounded by:

| Parameter | Default | Action when exceeded |
|-----------|---------|----------------------|
| `max_queued_mutation_count` | 100 000 | `LoggingFailed` — triggers quorum restart |
| `max_queued_mutation_data_size` | 2 GB | `LoggingFailed` — triggers quorum restart |

These limits protect against memory exhaustion when followers fall far behind. Hitting them causes the Hydra group to restart, which is disruptive. Monitor the metric `mutation_queue_size` and `mutation_queue_data_size` to detect growth before limits are reached.

**4. In-flight limits to followers**

To prevent network and memory overload, the leader caps the number of in-flight `AcceptMutations` requests to each follower:

| Parameter | Default | Effect |
|-----------|---------|--------|
| `max_in_flight_accept_mutations_request_count` | 10 | Maximum concurrent RPC calls per follower |
| `max_in_flight_mutations_count` | 100 000 | Maximum mutations in-flight per follower |
| `max_in_flight_mutation_data_size` | 2 GB | Maximum data in-flight per follower |

When a follower's in-flight limits are hit, the leader skips sending new mutations to that follower until acknowledgments arrive. This can delay quorum promotion if the follower is also a quorum member.

**5. Slow followers**

A follower in slow mode (after an RPC error or rejection) is sent only one request at a time. If that follower is needed for quorum, every commit waits for a full round-trip to that follower. Watch for log lines `Accept mutations mode is set to slow` — they indicate a follower recovery or network issue.

### Memory { #memory }

The master keeps the entire Cypress tree, all chunk metadata, and all replicated global objects in RAM. Memory usage grows with:

- Number of nodes in Cypress.
- Number of chunks and replicas.
- Number of globally replicated objects (accounts, media, etc.) × number of cells.
- Size of per-object metadata: user attributes, ACLs, schemas, table settings, tablet metadata, locks, active transaction state, and document values.

Monitor `yt_resource_tracker_memory_usage_rss{service="yt-master"}`. Because snapshot creation uses `fork`, the master process must have at least **double** its working-set memory available on the host.

#### Estimating memory by object kind { #memory-by-object-kind }

Use the master RSS metric for the process-level total, then use resource accounting attributes to find which user-visible objects contribute most of the persistent automaton state. The most useful attributes are:

- `@resource_usage/master_memory` — memory charged directly to one object.
- `@resource_usage/detailed_master_memory` — direct memory split by object kind, for example `nodes`, `chunks`, `attributes`, `tablets`, and `schemas`.
- `@resource_usage/chunk_host_cell_master_memory` — memory charged on chunk-host cells rather than on the cell that owns the Cypress node.
- `@recursive_resource_usage` — subtree total, useful for directories and account subtrees.
- `//sys/accounts/<account>/@recursive_resource_usage` and `//sys/accounts/<account>/@recursive_committed_resource_usage` — totals by account; the committed variant excludes active transactions.

Typical drill-down workflow:

```bash
# Cluster/process total. Use this for host sizing and snapshot fork headroom.
yt_resource_tracker_memory_usage_rss{service="yt-master"}

# Account-level ranking.
yt get //sys/accounts/<account>/@recursive_resource_usage
yt get //sys/accounts/<account>/@recursive_committed_resource_usage

# Subtree-level ranking.
yt get //path/to/subtree/@recursive_resource_usage

# Per-object breakdown.
yt get //path/to/object/@resource_usage
yt get //path/to/object/@resource_usage/detailed_master_memory

# Documents: find candidates, then sample serialized value size carefully.
yt find //path/to/subtree --type document
yt get //path/to/document/@value | wc -c
```

Interpret `detailed_master_memory` as a direction for remediation rather than as an exact replacement for RSS. RSS also includes allocator fragmentation, caches, transient queues, RPC buffers, Hydra mutation backlog, and other process overheads that are not charged to a Cypress object. Document node contents are another important caveat: a document's `@value` is stored in master memory and in snapshots as a YSON tree, but it is not exposed as a separate `detailed_master_memory` category. Estimate it by finding large `document` nodes and inspecting the size of `@value`; treat document payload as master memory even when the charged breakdown points only to the node itself or its attributes. In a multicell cluster, inspect the same attributes on the native cell that owns the object and remember that globally replicated objects are copied to every master cell.

Use the master Orchid ref-counted tracker when the RSS gap is large or when you suspect memory is held by C++ ref-counted objects rather than by account-charged Cypress resources. It is available under the master monitoring Orchid at `/orchid/monitoring/ref_counted` and reports both totals and per-type rows with `objects_alive`, `objects_allocated`, `bytes_alive`, and `bytes_allocated`:

```bash
# Total tracked ref-counted objects in one master process.
curl -s 'http://<master-host>:<monitoring-port>/orchid/monitoring/ref_counted/total'

# Per-type rows; sort locally by bytes_alive to find the largest holders.
curl -s 'http://<master-host>:<monitoring-port>/orchid/monitoring/ref_counted/statistics' \
  | jq 'sort_by(.bytes_alive) | reverse | .[:30]'
```

These Orchid stats are process-local, so query the leader and followers of each master cell when comparing peers or cells. They are not account resource usage and should not be added to `@resource_usage/master_memory`; use them to explain the uncharged part of RSS and to identify broad implementation-level holders such as YSON trees, protobufs, RPC buffers, caches, or other ref-counted helper objects. If the total ref-counted `bytes_alive` grows together with RSS while account `master_memory` stays flat, investigate the largest per-type rows and the corresponding subsystem before looking for user-owned Cypress objects.

The fields usually point to the following causes:

| Dominant field | Common cause | How to reduce it |
|----------------|--------------|------------------|
| `nodes` | Too many Cypress nodes: tiny files, temporary directories, operation artifacts, many map nodes, many links. | Delete unused subtrees; add TTL/cleanup for `//tmp`-like areas; pack many small items into tables instead of separate Cypress nodes; avoid creating one object per event/job when a table row is enough. |
| `chunks` or `chunk_host_cell_master_memory` | Too many chunks or replicas: small output chunks, append-heavy tables, unmerged intermediate data, excessive replication factor. | Run merge/auto-merge or rewrite tables with larger chunks; tune writers to produce larger chunks; remove obsolete data; avoid unnecessarily high replication factors; add secondary chunk-host cells when per-cell chunk metadata is the bottleneck. |
| `attributes` | Large or numerous user attributes, ACLs, annotations, or metadata blobs on many objects. | Move large metadata into table rows or files; keep Cypress attributes small and structured; remove stale custom attributes; avoid storing large opaque JSON/YSON blobs as attributes. |
| large `document` node values | YSON document payloads stored directly in the master as part of the node value. They increase RSS and snapshot size even if they are not visible as a dedicated `detailed_master_memory` field. | Keep documents to small configuration/metadata blobs; move large or frequently changed content to Cypress files or tables; delete obsolete documents; avoid using documents as a high-load object database. |
| `schemas` | Many distinct large table schemas or repeated schema-like metadata. | Reuse schemas where possible; avoid excessive column counts; remove obsolete tables; avoid duplicating large schema metadata in custom attributes. |
| `tablets` | Many mounted dynamic-table tablets, especially with rich tablet metadata. | Merge small tablets, reduce tablet count where load allows, unmount or remove unused dynamic tables, and size tablet bundles so tablet metadata growth is intentional. |
| transaction-related attributes (`locked_node_ids`, `branched_node_ids`, `staged_object_ids`) | Long-lived or very large transactions. | Abort stale transactions; shorten transaction lifetimes; split very large transactions; avoid keeping many locks or staged objects open. |

When shrinking master memory, first remove or merge objects on the heaviest accounts/subtrees, then verify both the charged resources and the process RSS. Charged resources should drop immediately after the corresponding mutations are applied; RSS may decrease more slowly because of allocator behavior and because snapshots, mutation queues, and caches may still hold memory temporarily. Always keep enough free host memory for the next snapshot fork while the cleanup is running.

### Node registration and disposal

When a Data Node registers with the master (or its liveness transaction expires), the master must process a full heartbeat listing all chunks on that node. For nodes with many chunks, this is a heavy mutation. When a node is disposed (its liveness transaction aborted), the master must update the replica sets of all chunks on that node. Disposal of locations on a single node is processed sequentially. This is the primary reason why adding secondary chunk-host cells reduces the per-cell disposal cost: each cell handles only the chunks assigned to it.

### Changelog I/O latency

Mutation latency is directly tied to changelog write latency, because mutations are not committed until the write quorum has persisted the changelog entry. Use fast NVMe storage for changelogs. Monitor `yt_changelogs_available_space{service="yt-master"}` to ensure space does not run out.

### Snapshot I/O and forking

Snapshot creation causes the master process to fork. During the fork, copy-on-write pages may cause elevated memory usage. After the fork, the parent continues applying mutations while the child serializes state to disk. Slow snapshot writes extend the period of elevated memory pressure but do not block mutations. Monitor `yt_snapshots_available_space{service="yt-master"}`.

Do not treat the fork phase itself as free. A large master has a large virtual address space, and the kernel must duplicate enough process metadata and page tables to create the snapshot child. That work can pause the master long enough to delay control RPC replies. The election manager is especially sensitive because its `PingFollower` RPC timeout is `follower_ping_rpc_timeout` (default **1 second**), followers abandon a stale leader after `leader_ping_timeout` (default **5 seconds**), and the leader stops leading if alive voting followers no longer form a quorum. Hydra's own lease pings then have `leader_lease_timeout` (default **5 seconds**) while building and maintaining the mutation quorum. The risk is highest when several master peers build snapshots at the same time or when the host is under memory pressure: delayed pings can consume these budgets, slow down mutation availability after failover, or trigger leader/lease loss. Track `/fork_executor/fork_duration`; if fork time grows, stagger snapshots, keep memory headroom, and investigate host memory pressure first. Timeout increases should be coordinated across election and Hydra settings and treated as a trade-off: they reduce false failovers caused by long forks, but also delay real failure detection. Hydra also enforces `snapshot_fork_timeout` (default **2 minutes**); exceeding it terminates the process rather than leaving the master stuck in an unbounded fork attempt.

## Administration

### Snapshots and read-only mode

Use `yt execute build_master_snapshots '{set_read_only=%false}'` to build master snapshots without changing write availability. Set `set_read_only=%true` only when the procedure requires a fully quiesced master state, for example before major updates or before adding new master cells. In read-only mode the master accepts no ordinary mutations; this ensures the snapshot captures a clean state with an empty subsequent changelog. A common quiescing command is:

```bash
yt execute build_master_snapshots \
  '{set_read_only=%true;wait_for_snapshot_completion=%true;retry=%true;enable_automaton_read_only_barrier=%true}'
```

For the `yt execute` client-driver command shown above, `build_master_snapshots` accepts the following parameters:

- `set_read_only` (optional boolean, default `%false`): whether to enter Hydra read-only mode while building snapshots. Use `%false` for an ordinary manual snapshot and `%true` for a quiescing snapshot.
- `wait_for_snapshot_completion` (optional boolean, default `%false`): whether the command should wait until snapshot building completes before returning.
- `retry` (optional boolean, default `%false`): whether to retry a failed request to an individual cell.
- `enable_automaton_read_only_barrier` (optional boolean, default `%false`): whether to use the automaton read-only barrier before entering read-only mode.

These are client-driver defaults, not universal API defaults. A direct native API call constructed with `TBuildMasterSnapshotsOptions{}` also defaults `set_read_only` to `false`, but defaults `wait_for_snapshot_completion`, `retry`, and `enable_automaton_read_only_barrier` to `true`. Specify the values explicitly in operational commands instead of relying on the calling interface's defaults.

#### Repeated and partially read-only invocations

The command sends a request to the primary cell and every secondary master cell concurrently, but handles retries and results independently for each cell. The following table describes what happens at one cell; the whole command returns successfully only after every cell has produced a snapshot ID.

| Cell state when its request is handled | `set_read_only=%false` | `set_read_only=%true` |
|----------------------------------------|-------------------------|------------------------|
| Writable; no snapshot is being built | Starts a new snapshot and leaves the cell writable. | Enters read-only mode and starts a read-only snapshot. |
| Writable, or still entering read-only; a snapshot is already being built | The request receives an `Unavailable` error (`Snapshot is already being built`). With `retry=%true`, `build_master_snapshots` repeatedly sends a new request to this cell and, once the existing build has finished, starts a **second** snapshot. It does not attach to the existing writable snapshot build. With `retry=%false`, the whole command fails. | The same retry behavior applies until the cell has actually entered read-only mode. Once it is read-only, a request with `set_read_only=%true` can attach to the in-progress read-only build as described below. |
| Read-only; a read-only snapshot is still being built | The request cannot attach to that build because it did not request read-only mode. It fails with `ReadOnlySnapshotBuildFailed`. This error is not retried. | The request attaches to the existing read-only snapshot operation. If `wait_for_snapshot_completion=%true`, it waits for and returns that operation's result; otherwise it immediately returns the allocated snapshot ID. It does not start another snapshot. |
| Read-only; a valid snapshot covers the current state and the following changelog is still empty | Returns the existing snapshot ID. It does not build another snapshot or leave read-only mode. | Returns the existing snapshot ID. It does not build another snapshot. This makes a repeated quiescing command idempotent. |
| Read-only, but there is no valid snapshot for the current state (for example, the previous read-only build failed) | Fails with `ReadOnlySnapshotBuildFailed`. | Fails with `ReadOnlySnapshotBuildFailed`. |

`retry=%true` retries every per-cell error except `ReadOnlySnapshotBuildFailed`; retries are not limited to the `Unavailable` response caused by another snapshot build. A cell that keeps returning a retryable error can therefore prevent the command from completing. `ReadOnlySnapshotBuilt` is not treated as an error: the existing snapshot ID carried by that response is included in the command result.

The meaning of “returns” in the table depends on `wait_for_snapshot_completion`. With `%true`, the cell result is returned only after the snapshot has completed. With the client-driver default `%false`, a newly started or joined build returns its allocated snapshot ID immediately; the command can finish before snapshot I/O finishes and does not report a later build failure. On an already read-only cell whose completed snapshot is still valid, the existing snapshot ID is returned immediately for either setting.

In a multicell cluster, some cells may already be read-only while others remain writable. Each cell follows the table independently: a read-only cell reuses or joins its valid read-only snapshot operation, while a writable cell builds a new snapshot and, when `set_read_only=%true`, enters read-only mode. There is no all-cells transaction or rollback. Requests are issued concurrently, so other cells may successfully build snapshots or enter read-only mode before one cell returns a terminal error. After a failed command, inspect every cell rather than assuming their states are uniform; fix any read-only cell without a valid snapshot (or explicitly leave read-only mode when it is safe) before retrying.

Neither value of `set_read_only` makes an already read-only cell writable. `set_read_only=%false` means “do not enter read-only mode,” not “exit read-only mode.” Use `master_exit_read_only` or the per-cell `exit_read_only` command to resume writes.

### Leaving read-only mode

Read-only mode persists until it is explicitly cleared. After a read-only snapshot procedure is complete and writes may resume, run:

```bash
yt execute master_exit_read_only '{}'
```

`master_exit_read_only` exits read-only mode on all master cells. It accepts the optional boolean parameter `retry`, which controls retrying the operation on transient failures. To leave read-only mode for one cell instead, use `yt execute exit_read_only '{cell_id=<cell-id>}'`.

### Monitoring master health

Key checks:

- `quorum_health` (Odin) — verifies all masters are in quorum.
- `master_alerts` — reads `//sys/@master_alerts`; any non-empty value should be investigated.
- `yt_resource_tracker_total_cpu{thread="Automaton"}` — automaton thread CPU.
- `yt_resource_tracker_memory_usage_rss{service="yt-master"}` — master RSS.
- `yt_changelogs_available_space` / `yt_snapshots_available_space` — disk space for Hydra storage.
- `mutation_queue_size` / `mutation_queue_data_size` — leader in-memory mutation backlog. Sustained growth indicates followers falling behind or slow changelog I/O.

The `//sys` Cypress node exposes multicell status including registered cell tags:

```bash
yt get //sys/@registered_master_cell_tags
yt get //sys/@dynamically_propagated_masters_cell_tags
```

### Adding new master cells

Adding secondary cells requires a complete cluster downtime. For detailed steps, see [Extending master servers](../admin-guide/cell-addition.md).

After cells are added and global objects are replicated, assign roles via:

```bash
yt set //sys/@config/multicell_manager/cell_descriptors/<cell_tag>/roles '[chunk_host]'
```

If no explicit roles are assigned, a secondary cell may still receive the default `cypress_node_host | chunk_host` roles unless `remove_secondary_cell_default_roles` is enabled or the cell is dynamically propagated. Assign roles explicitly to control what work the new cell serves after addition.

After adding cells or changing roles, remember that non-master components learn
the master cell directory through cached `GetClusterMeta` responses. Proxies,
nodes, schedulers, and master-cache processes may keep an older view until their
master-cell-directory synchronizer refreshes it (60-minute default sync period,
20-minute default successful-update TTL). For traffic-critical role changes,
wait for directory synchronization or temporarily shorten these intervals before
expecting all clients to route to the new roles. See
[Master cache and cached cluster metadata](./master-architecture-draft-4.md#master-cache)
for the cache and validation details.

Treat `//sys/@cluster_connection/secondary_masters` and master static configs as
the topology source for clients, not as a low-risk runtime tuning knob. Adding
cells must append new entries; do not remove or rewrite existing cell IDs/tags
unless you are following a dedicated recovery procedure. Before relying on a
newly assigned role, check the authoritative master-side role descriptor and then
verify the same role through a fresh `GetClusterMeta(populate_cell_directory=true)`
request from the route used by the affected components.

### Scaling recommendations

A single-cell master setup is sufficient for most clusters. Consider adding secondary chunk-host cells when:

- The automaton thread CPU on the primary cell is consistently above 70–80%.
- Master RSS memory is approaching the safety margin.
- Node registration and disposal operations are causing significant latency spikes (visible as long mutation queues in logs).

Starting with three secondary chunk-host cells provides a practical balance between operational complexity and capacity headroom. Up to 48 secondary cells are supported.

When new secondary cells are added, existing chunks do not automatically move — only new chunks are assigned to the new cells. To rebalance existing data, rewrite it into newly created tables (for example, via merge or copy-based workflows) so that new chunks are allocated under the current placement rules. Do not attempt to move an existing table's chunk tree by changing `external_cell_tag`: this attribute is not a supported knob for rebalancing existing tables.
