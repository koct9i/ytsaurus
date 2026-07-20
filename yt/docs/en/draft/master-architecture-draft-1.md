<!--
Draft number: 1
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master server architecture

This page describes the internal architecture of {{product-name}} master servers, covering the Hydra consensus engine, multi-cell topology, inter-cell communication, and performance characteristics relevant for users, administrators, and developers diagnosing performance issues.

## Role of the master

The master is the metadata server for a {{product-name}} cluster. It is responsible for:

- Storing the **Cypress** metainformation tree (directories, tables, files, and all their attributes).
- Tracking **chunk** locations — mapping table and file objects to the data chunks stored on Data Nodes.
- Managing **transactions** and locks.
- Keeping global objects such as accounts, media, racks, and data-center records.
- Orchestrating **tablet cells** for dynamic tables (assigning tablet cells to Tablet Nodes via heartbeats).
- Running the **chunk replicator**, which ensures chunks satisfy their replication policy.

Everything the master stores is durable and replicated across multiple master peers using the Hydra consensus engine.

## Hydra: the consensus engine { #hydra }

Hydra is the internal name for the consensus and state-machine replication layer used by {{product-name}} masters (and also by tablet cells). Hydra is similar in spirit to the [Raft](https://raft.github.io/) protocol.

### The automaton model

Hydra views the master as a **replicated state machine** (called an *automaton*). All durable state changes go through *mutations*: structured, serializable operations that are committed via consensus and then applied to the automaton in order.

Examples of mutations:

- `RegisterNode` — a Data Node reporting its full list of chunks (full heartbeat).
- `IncrementalHeartbeat` — a Data Node reporting newly added or removed chunks.
- `CreateTable` — a user request to create a table node in Cypress.
- `CommitTransaction` — committing an ongoing transaction.

Mutations are the **only** way to modify durable (persistent) state. Any variable that must survive a master restart must be changed exclusively through mutations.

In contrast, *transient* state lives only in the process memory and is rebuilt after every restart. Examples of transient state include the chunk refresh queue, replication queues, and in-flight job tracking. A mutation is allowed to modify transient state as a side-effect, but transient code paths must never modify persistent state.

### Two-thread model

Inside each Hydra peer there are two dedicated execution contexts:

| Thread | Role |
|--------|------|
| **Automaton thread** | Applies mutations sequentially to the state machine. All persistent state modifications happen here. |
| **Control thread** | Handles Hydra housekeeping: leader election, peer heartbeats, changelog I/O coordination, and snapshot management. |

The control thread must remain low-latency. Any blocking operation on the control thread (for example, a slow disk write) can delay peer heartbeats and cause quorum loss. Mutations that are expected to be heavy should be avoided on the automaton thread as well; a single slow mutation blocks all subsequent mutations and increases read latency.

### Leader, followers, and quorum

Each Hydra cell has **one leader** and one or more **followers**. Write requests (mutations) are always routed to the leader. The leader replicates each mutation to followers and considers it committed once a **write quorum** (`floor(N/2) + 1` peers) has persisted it.

Read requests are usually served by **followers** to distribute load. Before answering a read, a follower must ensure it has caught up to at least the state visible to the caller. This is done with the **SyncWithUpstream** operation: the follower contacts the leader to learn the current mutation sequence number, then waits until it has applied all mutations up to that point. In practice this adds roughly 10–20 ms of latency to most read requests.

Formally: after `SyncWithUpstream` completes on a follower, all mutations that *happened-before* the call on the leader are guaranteed to be applied on that follower.

The same mechanism also works across cells: a cell can sync with another cell's Hive mailbox before serving a cross-cell read.

### Leader lease and leader grace delay

After a leader is elected it does not serve requests immediately. There is a configurable **leader lease grace delay** during which the new leader waits before answering reads. The reason: the previous leader may have answered some read requests and those answers must not be invalidated. Even though the previous leader can no longer commit mutations (it no longer has leadership and cannot append new changelog records), it could still have cached responses in flight. The grace delay ensures the old leader's lease expires before the new leader starts serving.

There are two ping-based mechanisms on the control thread that matter for master availability. The **election manager** first chooses a leader and keeps the election epoch alive: the leader sends election `PingFollower` RPCs every `follower_ping_period` (default **1 second**) with `follower_ping_rpc_timeout` (default **1 second**), while followers renew a leader-ping lease and stop following if they do not receive a recurrent ping within `leader_ping_timeout` (default **5 seconds**). Invalid-state/invalid-epoch ping errors are tolerated only during `follower_grace_timeout` (default **5 seconds**) while peers are entering the epoch. The election leader considers itself healthy only while the alive voting followers still form a quorum; otherwise it stops leading.

Hydra then uses its own leader-lease ping rounds during leader recovery and lease tracking. The Hydra leader sends `PingFollower` requests to all peers and counts the local peer as an implicit success; a Hydra quorum is established only after enough voting peers answer from the `Following` or `FollowerRecovery` states. Each Hydra follower ping is bounded by `leader_lease_timeout` (default **5 seconds**), and normal lease checks run every `leader_lease_check_period` (default **2 seconds**). If a quorum of ping replies cannot be collected before the request timeouts, the lease check fails and the leader may lose its lease.

When the new leader is building a working Hydra quorum, both layers are on the critical path for accepting mutations. Election must keep a leader epoch with a voting quorum alive; then Hydra recovery waits for followers to enter recovery, acquires a changelog, replays recovery, waits for followers to recover, acquires the Hydra leader lease, starts the committer, and commits an initial heartbeat mutation before the cell is considered ready. The heartbeat mutation exercises the same write-quorum path as user mutations. Periodic heartbeat mutations are committed every `heartbeat_mutation_period` (default **60 seconds**) and are guarded by `heartbeat_mutation_timeout` (default **60 seconds**); if such a heartbeat cannot be committed in time, Hydra restarts the quorum. User mutation shipping to followers is separately bounded by `commit_flush_rpc_timeout` (default **15 seconds**). Thus a master that is slow to answer election pings, Hydra lease pings, or `AcceptMutations` requests can turn a transient latency spike into a failed quorum build, leader loss, or a disruptive Hydra restart.

This delay is short (typically a few seconds) but it is visible as an unavailability window during leader failover.

### Master failover and availability timeline { #failover-timeline }

Failover is local to a Hydra cell and usually completes within seconds, but clients observe it as a short sequence of degraded states:

| Phase | What happens | Availability impact |
|-------|--------------|---------------------|
| 1. Healthy leader | One peer is leader and accepts mutations; followers serve most reads after `SyncWithUpstream`. | Normal read/write latency. |
| 2. Leader loss detected | Current leader crashes, loses connectivity, or loses quorum. | New writes to this cell fail or are retried until a new leader is elected. |
| 3. Election and quorum recovery | Remaining peers run leader election and establish a new term. | This cell is temporarily unavailable for linearizable read/write traffic. |
| 4. New leader elected | A new leader is chosen, but it is still completing recovery steps before transitioning to active service. | External read/write traffic for this cell is still unavailable during grace delay, follower recovery, lease acquisition, and the initial heartbeat. |
| 5. Leader grace delay ends | Recovery completes, the old lease is guaranteed expired, and the new leader begins serving external traffic. | Normal read/write service resumes for this cell. |

In multicell setups, this timeline applies **independently to each cell**.

Possible multicell interference during failover:

- A failover in one secondary cell does not stop unrelated traffic on other cells, but any operation that touches the failed cell (for example, table/chunk metadata hosted there) will stall or retry.
- Cross-cell reads can see amplified latency because they may need both local catch-up (`SyncWithUpstream`) and remote Hive synchronization with the recovering cell.
- If the primary cell is the one failing over, user-visible impact is broader because the primary hosts the root Cypress tree and coordinates global metadata flows.

### Follower recovery { #follower-recovery }

A peer that has joined an election epoch as a follower does not immediately become a serving follower. It first enters `FollowerRecovery` and must catch its local automaton state up to the leader's committed state.

The recovery target is learned from the leader's initial Hydra ping or mutation stream. It consists of the leader's committed `segment_id` and committed `sequence_number` for the current term. The follower creates a follower committer, initializes it to the committed sequence number reported by the leader, and then runs recovery before it is allowed to serve normal follower reads.

Recovery proceeds in two modes:

1. **Changelog-only recovery**. If the follower is only slightly behind (`target_sequence_number - current_sequence_number < max_sequence_number_gap_for_changelog_only_recovery`), it tries to replay local changelogs from its current state. This is the fast path after a short restart or a small network gap.
2. **Snapshot-assisted recovery**. If the gap is too large, or changelog-only recovery fails, the follower finds the newest usable snapshot locally or on other peers, downloads it if necessary, loads it into the automaton, and then replays changelogs from the snapshot generation up to the target sequence number.

When persistent changelogs are enabled, follower recovery also synchronizes each changelog segment with the leader before replaying it:

- if the local segment has extra records beyond the leader's record count needed for the recovery target, the follower truncates the local tail;
- if the local segment is shorter, the follower downloads missing records from the leader;
- if truncation would remove a mutation that is known to have been reliably applied, recovery fails and the peer restarts instead of silently rolling back durable state.

After replay reaches the target state, the follower performs a final catch-up through the follower committer. During this catch-up it may already accept and log new `AcceptMutations` batches from the leader, but it does not transition to normal `Following` until recovery is complete. Once recovery completes, the follower may serve follower reads; write availability still depends on the leader having an active quorum.

Operationally, `FollowerRecovery` means the peer is alive but still not a healthy read replica. In Orchid `/hydra`, check `state` (it remains `FollowerRecovery` until the transition completes), `catching_up`, `automaton_sequence_number`, and `last_snapshot_id_used_for_recovery`; `active_follower` becomes true only after recovery completes. A peer stuck in `FollowerRecovery` is usually waiting on snapshot download/load, changelog read/download, changelog truncation, or mutation replay on the automaton thread.

### Entering and leaving read-only mode { #read-only-mode-transitions }

Hydra read-only mode is a replicated state-machine state, not just an RPC filter on one process. It is normally entered by a forced snapshot request with `set_read_only=true`, for example the administrative flow that builds master snapshots with `--read-only --wait-for-snapshot-completion`.

Entering read-only mode has several steps:

1. The active leader rejects concurrent snapshot-build attempts and marks `entering_read_only_mode=true` in its epoch state.
2. If the automaton read-only barrier is enabled, the leader first commits a heartbeat system mutation. This flushes mutations that were already serialized/scheduled before the transition. After that, the automaton-specific `GetReadyToEnterReadOnlyMode` hook is awaited; master components use this hook to stop starting workflows that would otherwise leave prepared transactions or other in-flight durable work behind the barrier.
3. The leader commits the `EnterReadOnly` system mutation. When this mutation is serialized/applied, the Hydra epoch and automaton `read_only` flags become true.
4. The leader builds the requested snapshot. The snapshot metadata records `read_only=true`; after it is loaded during recovery, the peer comes back in read-only mode.

Once read-only is active, ordinary user mutations are rejected with the Hydra `ReadOnly` error. System mutations, including `ExitReadOnly`, are still allowed; otherwise the cluster could not leave the mode. Periodic snapshot triggering is skipped while the cell is read-only, but the explicit read-only snapshot request either returns the in-flight/latest read-only snapshot result or reports that the requested result has already been lost.

Leaving read-only mode is also a mutation. The active leader commits `ExitReadOnly`; when the mutation is applied, Hydra clears the automaton read-only flag and advances the logical clock because an arbitrary amount of wall-clock time may have passed while no ordinary mutations were accepted. If a peer still observes itself as read-only after the exit mutation callback, Hydra schedules a quorum restart so all peers re-enter a consistent non-read-only epoch.

Nontrivial operational details:

- Read-only state is persisted in snapshot metadata. Restarting from a read-only snapshot does not by itself resume writes; an explicit `ExitReadOnly` action is required.
- Followers learn read-only snapshot requests from the leader. A follower in normal `Following` may build the requested snapshot locally; a follower still in `FollowerRecovery` records the snapshot id but cannot build a snapshot until recovery completes.
- Cross-cell read requests that require synchronization (`cell_tags_to_sync_with` or transaction/barrier synchronization) are rejected while the local Hydra manager is read-only. Plain local reads that do not require such synchronization can still be served by active peers.
- `//sys/@hydra_read_only` exposes the current cell's Hydra read-only flag through Cypress. The master Orchid `/hydra` exposes both `read_only` and `entering_read_only_mode`; `last_snapshot_read_only` shows whether the latest successfully built snapshot was a read-only snapshot.
- Do not confuse Hydra read-only mode with a read-only changelog or snapshot store used by dry-run/offline tooling. Hydra read-only is a consensus-visible automaton state; storage read-only is an I/O capability of the local persistence backend.

### Changelog and snapshot storage { #changelog-snapshot }

Hydra durably stores committed mutations in *changelogs* (also called journals). A changelog is an append-only file; mutations are appended sequentially. On disk, changelog files are stored in the location configured as `changelogs` in the master static configuration.

Periodically, Hydra takes a **snapshot** — a complete serialized image of the automaton state. After a snapshot is written, changelogs before the snapshot point are no longer needed and can be pruned. On restart, Hydra loads the latest snapshot and then replays only the changelogs that follow it.

Snapshot creation uses a **fork** on master processes (forked child serializes the state while the parent continues running). This means the master process needs roughly double its working-set memory available at snapshot time. For large clusters the snapshot can be hundreds of gigabytes. Storage for snapshots is configured separately from changelogs.

The `fork()` call itself is also latency-sensitive. Even though snapshot serialization is done by the child after the fork, the kernel must create a child process and copy the parent's virtual-memory metadata/page tables first. For a large master address space this can take noticeable time. During this interval the master process may not reply promptly to control RPCs, including election pings and Hydra leader-lease pings. Pauses around `follower_ping_rpc_timeout` can make the election leader temporarily mark a peer down; pauses around `leader_ping_timeout` can make a follower abandon the election epoch; pauses around Hydra `leader_lease_timeout` can make the Hydra leader miss a lease ping round. If enough voting peers pause at unlucky times, quorum construction or lease renewal can fail. Hydra records fork duration under the `/fork_executor/fork_duration` profiler timer and aborts if the fork does not complete within `snapshot_fork_timeout` (default **2 minutes**).

#### When the master initiates a snapshot { #snapshot-initiation }

Snapshots are initiated by the **active leader** of a Hydra cell. The leader requests a checkpoint in the following cases:

1. **Periodic snapshot timer**
   - Hydra keeps a deadline `now + snapshot_build_period + a random value between 0 and snapshot_build_splay`.
   - By default this is **60 minutes + up to 5 minutes of splay**.
   - The periodic trigger is skipped while the cell is read-only.

2. **Changelog record-count limit**
   - If the current changelog reaches `max_changelog_record_count`, the leader rotates to a new changelog and builds a snapshot.
   - Default: **1,000,000 records**.

3. **Changelog data-size limit**
   - If the current changelog reaches `max_changelog_data_size`, the leader also checkpoints.
   - Default: **1 GB** of changelog payload.

4. **Manual snapshot request**
   - Operators can force a snapshot explicitly. For a regular manual snapshot, use `yt execute build_master_snapshots '{set_read_only=%false}'`.
   - Entering read-only mode is optional: use `set_read_only=%true`, typically with `wait_for_snapshot_completion=%true`, only when the operation requires a quiesced master state and a clean empty tail changelog. In that mode, Hydra first commits a barrier mutation and switches the cell into read-only state before building the snapshot.

5. **Final recovery action**
   - Some recovery flows request `BuildSnapshotAndRestart` after recovery completes.

Only one snapshot can be built at a time. If a snapshot is already in progress, additional requests fail or reuse the current in-flight result, depending on the request mode.

#### How snapshots and changelogs are named { #snapshot-naming }

Local Hydra persistence uses a shared numeric ID space for snapshots and changelog segments:

- Snapshot file: `000000123.snapshot`
- Changelog data file: `000000123.log`
- Changelog index file: `000000123.log.index`

The numeric part is the **segment ID**.

Hydra tracks two related counters:

- **Sequence number** — a monotonic counter across all physical mutations in the cell. It is used for commit ordering, `SyncWithUpstream`, and recovery targets.
- **Version = (segment_id, record_id)** — the physical location of a mutation inside changelog storage.

Within a single changelog segment, `record_id` increases from `0`. When Hydra rotates to a new changelog, `segment_id` increases by one and `record_id` resets to `0`.

The important consequence is:

- Snapshot `N` is built **after** Hydra rotates into changelog segment `N`.
- Snapshot `N` therefore captures all mutations up to the end of segment `N-1`.
- Recovery loads `000000123.snapshot` and then replays changelog `000000123.log` and later segments.

The snapshot metadata also stores the exact last included mutation as `last_segment_id` and `last_record_id`, plus the corresponding `sequence_number`.

#### Hydra versions, revisions, and object attributes { #hydra-versions-and-object-revisions }

Hydra exposes several closely related numbers. They are easy to confuse because they all grow with mutations, but they answer different questions:

| Number | Structure | Scope | Meaning |
|--------|-----------|-------|---------|
| **Physical version** | `(segment_id, record_id)` | One Hydra cell | Changelog position of a physical mutation record. `segment_id` is the changelog/snapshot generation; `record_id` is the record inside that changelog. |
| **Sequence number** | signed 64-bit counter | One Hydra cell | Monotonic apply/commit order across all physical mutations. This is the number used by commit quorum state, catch-up, and `SyncWithUpstream`. |
| **Revision** | unsigned 64-bit integer | One Hydra cell | Compact public form of a **logical** Hydra version, computed as `(segment_id << 32) | logical_record_id`. Object attributes store revisions, not the two-field tuple. `0` is `NullRevision` and means "not set". |
| **Automaton version** | `(segment_id, physical_record_id, logical_record_id)` | One peer's automaton | The peer's applied automaton position. In normal mutation streams the logical and physical record ids usually move together; logical ids exist for compatibility with historical/logical record numbering. |

There is no cluster-wide Hydra revision. The numbers above are local to a single master cell. Comparing revisions from different cells is only meaningful as opaque diagnostics; it does not define global recency.

Master objects keep two revision attributes. Both are logical revisions: they are suitable for object-level freshness checks but are not physical changelog offsets.

- `@attribute_revision` — last mutation revision that changed the object's attributes (for example user attributes, ACL-related attributes, owner/account metadata, expiration attributes, annotations, and other attribute-like metadata).
- `@content_revision` — last mutation revision that changed the object's content or structure (for example table/file contents as represented in master metadata, document value, map-node children, links, locks that alter content state, and other type-specific content changes).
- `@revision` — `max(@attribute_revision, @content_revision)`, i.e. the latest visible change known for that object on this cell.

Cypress nodes can additionally expose `@native_content_revision` when the object is external to the cell serving the request. It records the content revision reported by the node's native cell, so it is useful when debugging portal/external-node propagation. For native objects, use `@content_revision`.

For transactional Cypress operations, a branch initially inherits the trunk object's `attribute_revision` and `content_revision`. Mutations inside the transaction update the branch. When the transaction is committed, the trunk receives the commit mutation's current revision for the affected attribute/content parts, not an independent wall-clock timestamp. This is why revision values are durable ordering tokens rather than time values.

The following attributes are visible through the Cypress API on objects that support the standard object proxy attributes:

```text
# Object-wide attributes
@id
@type
@native_cell_tag
@foreign
@life_stage
@revision
@attribute_revision
@content_revision

# Cypress-node additions
@native_content_revision   # present for external/foreign Cypress-node representations when available
```

The `//sys` Cypress node exposes cell-level Hydra state for the cell serving the request:

```text
//sys/@current_hydra_version   # string form: segment:physical_record(logical_record)
//sys/@hydra_logical_time
//sys/@hydra_read_only
```

The basic-attributes RPC path also returns `revision`, `attribute_revision`, and `content_revision`, so clients can use these fields without listing all attributes. Many Cypress commands accept revision prerequisites (for example "perform this mutation only if path P still has revision R"); the prerequisite is checked against the target object's current `@revision` on the cell that owns that path. Use this for optimistic concurrency, but do not treat it as a physical changelog position and do not use it to order updates across unrelated master cells.

Master Orchid exposes peer-local Hydra progress rather than per-object revisions. Under the master's monitoring Orchid `/hydra`, the most useful fields are:

- `automaton_version` — string form of the applied automaton version, i.e. the peer's current `(segment_id, physical_record_id, logical_record_id)` position;
- `automaton_sequence_number` — latest applied mutation sequence number;
- `state`, `active`, `active_leader`, `active_follower`, and `read_only` — whether this peer can serve traffic and whether mutations are accepted;
- `last_snapshot_id`, `last_snapshot_read_only`, and `last_snapshot_id_used_for_recovery` — snapshot generation diagnostics;
- on the leader-committer monitoring subtree, `next_logged_version`, `next_logged_sequence_number`, `committed_sequence_number`, and `committed_segment_id` show changelog logging and quorum-commit progress.

These Orchid fields are process-local. Query the leader and followers of each master cell separately when comparing recovery lag, follower catch-up, or suspected stale reads.

#### How many changelog files exist at once { #changelog-count }

At runtime there is exactly **one active changelog segment** per Hydra peer. In a replicated cell, each peer maintains its own active segment locally. Older segments remain on disk until cleanup removes them.

The number of retained historical files depends on:

- How frequently snapshots are built.
- How much history is needed for recovery after the latest snapshot.
- Janitor retention limits (`max_snapshot_count_to_keep`, `max_snapshot_size_to_keep`, `max_changelog_count_to_keep`, `max_changelog_size_to_keep`).

Hydra also forces a new snapshot right after leader recovery if the remaining tail after the last snapshot becomes too large. The trigger is based on:

- `max_changelogs_for_recovery`
- `max_changelog_mutation_count_for_recovery`
- `max_total_changelog_size_for_recovery`

This keeps restart and catch-up time bounded.

#### Flush and fsync behavior { #changelog-flush }

Mutation records are not fsynced one by one. The write path is batched:

1. Mutations are serialized on the control thread and appended to the active changelog queue.
2. The changelog dispatcher flushes queued data when any of these happens:
   - queued data reaches `data_flush_size` (default **16 MB**),
   - `flush_period` elapses since the previous flush (default **10 ms**),
   - an explicit/forced flush is requested.
3. Each flush issues a data-file flush (`FlushFile(..., Data)`), which is the durable persistence point for the changelog payload.
4. The changelog index is flushed separately:
   - asynchronously after `index_flush_size` bytes (default **16 MB**),
   - synchronously on explicit finish/close/rotation.

At the Hydra level, leader-to-follower mutation shipping is driven by a separate executor with period `mutation_flush_period` (default **5 ms**). This controls how often the leader tries to send logged mutations to followers; it is distinct from the local disk flush period of the changelog file itself.

#### How previous snapshots are removed { #snapshot-retention }

Old local Hydra files are removed by the **local Hydra janitor**:

- It runs every `cleanup_period` (default **10 seconds**).
- It is enabled by `enable_local_janitor` (default **true**).
- By default it keeps up to `max_snapshot_count_to_keep = 10` snapshots.
- Optional size-based limits can also be set for both snapshots and changelogs.

Cleanup is based on a **threshold ID** computed jointly for snapshots and changelogs:

- files with ID **strictly less** than the threshold can be removed;
- if no snapshot exists, nothing is removed;
- the latest snapshot is never removed by cleanup;
- changelogs newer than the latest snapshot are never removed;
- changelog `0` is treated conservatively so recovery does not lose its bootstrap segment unexpectedly.

As a result, cleanup removes older Hydra persistence generations — one snapshot together with the changelog tail that precedes the next retained snapshot — only after there is a newer snapshot that makes them obsolete for recovery.

#### How to monitor snapshots, state size, and changelog state { #snapshot-monitoring }

For per-peer Hydra state, inspect the master's monitoring Orchid subtree:

```text
/hydra
```

Useful fields include:

- `building_snapshot` — whether a snapshot is in progress now;
- `last_snapshot_id` — newest successfully built snapshot ID;
- `last_snapshot_read_only` — whether that snapshot was read-only;
- `last_snapshot_id_used_for_recovery` — which snapshot the peer loaded on startup;
- `automaton_sequence_number` — latest applied mutation sequence number;
- `read_only` — whether the peer is in read-only mode.

To monitor **live in-memory state size**, use:

- `yt_resource_tracker_memory_usage_rss{service="yt-master"}`

This is the best operational proxy for the current master state footprint. Because snapshot build uses `fork`, safe host memory should be budgeted conservatively at about **2 × the master's RSS at snapshot time**. Actual peak memory is often lower and depends on how many pages are dirtied while the child is writing the snapshot.

To monitor the **latest snapshot size**, use Hydra profiling gauges:

- `/compressed_snapshot_size`
- `/uncompressed_snapshot_size`

These reflect the most recently completed snapshot. For on-disk usage trends, also watch the actual contents of the `snapshots` directory and the free-space metric:

- `yt_snapshots_available_space{service="yt-master"}`

To monitor **changelog footprint and headroom**, use:

- `yt_changelogs_available_space{service="yt-master"}`
- `mutation_queue_size`
- `mutation_queue_data_size`

The first shows storage headroom. The latter two show the in-memory backlog of logged mutations that still must be retained for follower delivery.

For request-side consequences of these persistence mechanics, see [Mutation ordering and commit pipeline](./master-architecture-draft-3.md#mutation-pipeline) and [Performance considerations](./master-architecture-draft-5.md#performance). For the operator workflow that forces a clean read-only snapshot, see [Snapshots and read-only mode](./master-architecture-draft-5.md#snapshots-and-read-only-mode).

{% note warning %}

The disk used for changelogs should have **good sequential-write performance**. Slow changelog writes increase mutation latency for all writers. NVMe SSDs are recommended.

{% endnote %}

### Erasure journals (tablet cells only)

For tablet cells the journal write amplification can be reduced by using **erasure journals**: instead of writing a full mutation to each of N replicas, the mutation is split and XOR-parity parts are written, similar to erasure-coded chunks. This reduces write amplification at the cost of slightly higher read latency during recovery. Erasure journals are most beneficial for ordered dynamic tables (queues) where write throughput is critical.
