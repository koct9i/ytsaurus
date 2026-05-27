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

### Changelog and snapshot storage { #changelog-snapshot }

Hydra durably stores committed mutations in *changelogs* (also called journals). A changelog is an append-only file; mutations are appended sequentially. On disk, changelog files are stored in the location configured as `changelogs` in the master static configuration.

Periodically, Hydra takes a **snapshot** — a complete serialized image of the automaton state. After a snapshot is written, changelogs before the snapshot point are no longer needed and can be pruned. On restart, Hydra loads the latest snapshot and then replays only the changelogs that follow it.

Snapshot creation uses a **fork** on master processes (forked child serializes the state while the parent continues running). This means the master process needs roughly double its working-set memory available at snapshot time. For large clusters the snapshot can be hundreds of gigabytes. Storage for snapshots is configured separately from changelogs.

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
   - Operators can force a snapshot explicitly, for example with `yt-admin build-master-snapshots --read-only --wait-for-snapshot-completion`.
   - Read-only mode first commits a barrier mutation, then switches the cell into read-only state, so the resulting snapshot has a clean empty tail changelog.

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
