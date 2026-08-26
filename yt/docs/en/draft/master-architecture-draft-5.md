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

### Master snapshot and changelog storage rotation { #storage-rotation }

Each master **peer** has its own local Hydra persistence. Changelogs and
snapshots use the same monotonically increasing segment ID: a peer appends
mutations to one active changelog, closes it at a checkpoint, starts the next
segment, and writes a snapshot for the checkpoint ID. Old files are not removed
as part of that rotation. A separate local janitor scans both directories and
removes complete obsolete generations after a newer snapshot makes them
unnecessary for recovery. Consequently, rotation limits control file size and
snapshot frequency, whereas janitor limits control retained disk usage; neither
set is a substitute for the other.

#### Static storage configuration

The peer-local stores are selected in each master process's static configuration.
They cannot be moved with `//sys/@config`; changing them requires updating every
peer's process configuration and restarting it according to the master
maintenance procedure.

| Static path | Options relevant to persistence |
|---|---|
| `changelogs` | Required `path`; `io_engine_type` and optional `io_engine`; `data_flush_size` (default 16 MB, legacy alias `flush_buffer_size`), `index_flush_size` (16 MB), `flush_period` (10 ms), optional `preallocate_size`, `recovery_buffer_size` (16 MB), `flush_quantum` (10 ms), and `changelog_reader_cache`. These settings control the local `.log` and `.log.index` files and their I/O, not segment retention. |
| `snapshots` | Required `path`; `codec` (`lz4`), `use_headerless_writer` (`false`), and `clean_temporary_files_on_store_initialize` (`true`). The master configuration uses a local snapshot store; these settings control snapshot representation and initialization, not when snapshots are made. |
| `hydra_manager` | All cadence, rotation, recovery-tail, and local-janitor options in the tables below may be supplied here as static defaults. `snapshot_background_thread_count` (default `0`) is master-specific. `close_changelogs` (default `true`) is a compatibility option no longer used by Hydra2 and should not be used as a rotation control. |

Keep `changelogs/path` on low-latency durable storage: a mutation cannot commit
until the changelog write quorum has persisted it. The snapshot path may be on a
different filesystem. The janitor correlates files by numeric ID even when the
directories differ, so do not independently delete or renumber files in either
directory.

#### Rotation and automatic snapshot triggers

The active leader checks whether it should establish a checkpoint every
`checkpoint_check_period`. A checkpoint rotates the changelog; the snapshot is
built asynchronously from the corresponding automaton state. Only one snapshot
build can be active at once.

| `hydra_manager` option | Default | Dynamic | Effect |
|---|---:|:---:|---|
| `snapshot_build_period` | 60 min | Yes | Maximum periodic interval before requesting a snapshot. The leader schedules the request with the splay below; the periodic trigger is skipped in read-only mode. |
| `snapshot_build_splay` | 5 min | Yes | Random delay added to periodic snapshot scheduling, preventing peers/cells from forking simultaneously. |
| `checkpoint_check_period` | 15 s | Yes | Period for checking periodic and changelog-limit checkpoint conditions. It bounds detection delay; it is not a snapshot interval. |
| `max_changelog_record_count` | 1,000,000 | Yes | Requests a checkpoint when the active segment reaches this many records. |
| `max_changelog_data_size` | 1 GB | Yes | Requests a checkpoint when the active segment reaches this payload size. The file can overshoot by the last batch and filesystem/index overhead. |
| `snapshot_build_timeout` | 5 min | Yes | Fails a snapshot build that takes too long. |
| `snapshot_fork_timeout` | 2 min | Yes | Terminates the process if `fork()` itself does not finish in time. This is a safety bound, not a scheduling setting. |
| `alert_on_snapshot_failure` | `true` | Yes | Publishes an alert after snapshot construction fails. |

The two changelog limits are OR conditions. Lowering either value increases the
number of checkpoints and snapshots and normally produces smaller recovery
tails, but also increases fork and snapshot I/O frequency. Increasing them does
not override `snapshot_build_period`. Manual `build_master_snapshots` and
recovery-driven snapshots are additional triggers.

After leader recovery, Hydra also requests a snapshot if **any** accumulated tail
limit is reached:

| `hydra_manager` option | Default | Dynamic | Tail measured since the last successful snapshot |
|---|---:|:---:|---|
| `max_changelogs_for_recovery` | 20 | Yes | Number of changelog segments. |
| `max_changelog_mutation_count_for_recovery` | 20,000,000 | Yes | Number of mutations. |
| `max_total_changelog_size_for_recovery` | 20 GB | Yes | Total mutation payload size. |

These recovery limits do not delete files and do not rotate the live segment by
themselves during ordinary leading. They force a new snapshot immediately after
leader recovery so that the next restart is not repeatedly burdened by an
excessive tail.

#### Retention and janitor behavior

The local janitor runs independently in every master process. Its six options
are members of the same static `hydra_manager` section and are all dynamically
overridable:

| `hydra_manager` option | Default | Meaning |
|---|---:|---|
| `enable_local_janitor` | `true` | Starts peer-local cleanup. Turning it off stops future passes but does not restore removed files. |
| `cleanup_period` | 10 s | Interval between directory scans. |
| `max_snapshot_count_to_keep` | 10 | Snapshot count target. The newest snapshot is nevertheless always preserved. |
| `max_snapshot_size_to_keep` | unset | Optional byte target for retained snapshots. |
| `max_changelog_count_to_keep` | unset | Optional changelog count target. |
| `max_changelog_size_to_keep` | unset | Optional byte target for retained changelogs. |

An unset optional limit does not constrain that dimension. If both count and
size are set, the stricter threshold wins; `0` is allowed but still cannot make
the janitor remove the newest snapshot or files needed after it. The janitor
computes one safe threshold from the snapshot and changelog listings, then
removes `.snapshot`, `.log`, and matching `.log.index` files whose IDs are
strictly below it. Important safeguards are:

- with no snapshot present, it removes nothing;
- it never advances cleanup beyond the newest snapshot ID, so the changelog tail
  after that snapshot remains recoverable;
- it retains at least the newest snapshot regardless of a zero count or size
  target;
- changelog `0` is retained conservatively until the configured threshold and
  available snapshots permit removing its generation.

Thus, a count is a target rather than a promise that exactly that many files will
remain. A large tail after the newest snapshot, files created between cleanup
passes, malformed filenames (which are warned about and skipped), and deletion
errors can all leave more files. Retention is also peer-local: do not infer that
all peers have identical files merely because they use identical limits.

To change the live settings cluster-wide, write individual options below the
dynamic `hydra_manager` map. The config is replicated to secondary master cells,
and each peer reconfigures its local janitor and Hydra manager. Updating leaves
unrelated options intact, unlike replacing the entire map:

```bash
yt set //sys/@config/hydra_manager/snapshot_build_period '"90m"'
yt set //sys/@config/hydra_manager/snapshot_build_splay '"10m"'
yt set //sys/@config/hydra_manager/max_changelog_record_count 1500000
yt set //sys/@config/hydra_manager/max_changelog_data_size '"2GB"'
yt set //sys/@config/hydra_manager/max_snapshot_count_to_keep 8
yt set //sys/@config/hydra_manager/max_snapshot_size_to_keep '"500GB"'
yt set //sys/@config/hydra_manager/max_changelog_count_to_keep 40
yt set //sys/@config/hydra_manager/max_changelog_size_to_keep '"200GB"'
yt set //sys/@config/hydra_manager/cleanup_period '"30s"'
yt set //sys/@config/hydra_manager/enable_local_janitor %true
```

Optional dynamic fields that are absent fall back to the process's static
`hydra_manager` value. Remove an individual dynamic override to return that
option to its static value. Store paths, snapshot codec, local changelog
flush/index settings, `snapshot_background_thread_count`, and the compatibility
`close_changelogs` option are static-only.

#### Metrics, Orchid, and operational checks

Query every peer of every primary and secondary master cell. Disk files and
janitor execution are local, while snapshot initiation is leader-driven.

| Signal | What it tells you |
|---|---|
| `yt_snapshots_available_space{service="yt-master"}` and `yt_snapshots_free_space{service="yt-master"}` | Filesystem headroom at the configured snapshot path. “Available” accounts for filesystem rules affecting the process; “free” is the raw free space. |
| `yt_changelogs_available_space{service="yt-master"}` and `yt_changelogs_free_space{service="yt-master"}` | Equivalent headroom at the changelog path. Alert on available space. |
| `/hydra/compressed_snapshot_size` and `/hydra/uncompressed_snapshot_size` | Compressed bytes written and logical bytes serialized for the latest successful snapshot; use their ratio and trend for capacity planning. These Hydra gauges carry the cell ID tag. |
| `/hydra/fork_executor/fork_duration` | Time spent creating the snapshot child, before asynchronous serialization. Growth indicates memory/page-table pressure and can threaten election timeouts. |
| `/changelogs/changelog_flush_io_time`, `/changelogs/changelog_close_io_time`, `/changelogs/changelog_truncate_io_time`, `/changelogs/changelog_read_io_time`, `/changelogs/queue_count`, `/changelogs/records`, and `/changelogs/bytes` | Local changelog I/O latency, queued dispatcher work, and cumulative traffic. Exact exported metric names depend on the monitoring exporter prefix and tags. |

The master monitoring Orchid `/hydra` does not expose directory byte totals or a
janitor “last pass” field. It does expose the state needed to interpret rotation:

- `building_snapshot`, `last_snapshot_id`, `last_snapshot_read_only`, and
  `last_snapshot_id_used_for_recovery` show snapshot progress and provenance;
- `automaton_version` gives `(segment_id, physical_record_id,
  logical_record_id)`, while `automaton_sequence_number` shows applied mutation
  progress;
- `state`, `active_leader`, `active_follower`, `read_only`,
  `entering_read_only_mode`, `catching_up`, and `acquiring_changelog` explain why
  a peer may not currently rotate, snapshot, or serve traffic.

Correlate `last_snapshot_id` with the segment ID in `automaton_version` and with
the actual numeric filenames. A widening segment gap means the recovery tail is
growing; a permanently true `building_snapshot`, repeated snapshot alerts, or a
flat `last_snapshot_id` while the tail grows calls for checking snapshot space,
fork duration, snapshot timeouts, and logs. Janitor actions are diagnosed in the
master log through `Janitor is removing Hydra file`, broken-file warnings, and
removal-failure warnings; there is currently no dedicated janitor metric or
Orchid subtree.

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

#### Failed or canceled snapshot builds

Snapshot construction can fail because the snapshot store is out of space or unavailable, serialization or writing fails, the child exceeds `snapshot_build_timeout`, or `fork()` exceeds `snapshot_fork_timeout`. It can also be **canceled** even when snapshot I/O itself is healthy: if an election, leader switch, or restart makes a peer leave its Hydra epoch, Hydra cancels that epoch's outstanding snapshot promise with `Hydra peer has stopped`. A client using `wait_for_snapshot_completion=%false` has already received the allocated ID and therefore cannot learn about any of these later failures from that invocation.

When a waited build fails or is canceled, use the following workflow:

1. Determine which cells failed. `build_master_snapshots` operates on cells independently, so retain the per-cell errors and do not assume that a command error means that no snapshots were created. Inspect every primary and secondary cell's leader Orchid `/hydra`, in particular `state`, `active_leader`, `read_only`, `entering_read_only_mode`, and `last_snapshot_read_only`. Compare the latest snapshot ID with the current changelog segment/record position to distinguish a valid snapshot with an empty tail from a read-only cell for which no valid snapshot was produced.
2. Find the first error in the affected peer's log. `Snapshot canceled` together with `Hydra peer has stopped` identifies epoch loss; nearby election and restart messages explain why the epoch ended. For a genuine build failure, inspect the nested snapshot-writer/fork error and check `yt_snapshots_available_space`, host memory pressure, `/fork_executor/fork_duration`, and configured `snapshot_build_timeout` and `snapshot_fork_timeout`. Treat a timeout increase as mitigation only after addressing slow storage or memory pressure.
3. For a **normal, writable** build (`set_read_only=%false`), wait for a stable active leader and correct the underlying resource or persistence problem, then issue the command again. The canceled attempt does not put the cell into read-only mode, and the replacement request allocates a new snapshot at the current checkpoint. Use `wait_for_snapshot_completion=%true` to verify the retry rather than merely obtaining an ID.
4. For a **read-only** build, first check whether another peer completed the requested snapshot and whether the cell now reports a valid current read-only snapshot. If it did, a repeated `set_read_only=%true` request returns that snapshot or joins a still-running read-only operation. If the cell is read-only with no valid snapshot, both values of `set_read_only` fail with `ReadOnlySnapshotBuildFailed`: read-only mode forbids advancing the automaton to a new checkpoint, and retrying cannot repair it in place.
5. To recover the latter case, fix the original cause, confirm that the maintenance procedure permits writes to resume, run `exit_read_only` for the affected cell (or `master_exit_read_only` only when all cells should resume), wait for a stable writable leader, and repeat the quiescing command with `set_read_only=%true` and `wait_for_snapshot_completion=%true`. Exiting is a deliberate availability and consistency decision, not an automatic rollback. If writes must remain frozen, do not repeatedly retry and do not exit read-only merely to clear the error; escalate to the incident-specific snapshot restore or master recovery procedure.

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

#### Secondary-cell registration sequence

Registration starts only after the new secondary has recovered, elected a leader,
and completed world initialization. The secondary leader periodically checks its
persistent registration state. When the state is `None`, it commits a local
`StartSecondaryMasterRegistration` mutation. Applying that mutation changes the
state to `Registering`, creates the reliable Hive mailbox and master entry for the
primary, and sends `RegisterSecondaryMasterAtPrimary(cell_tag)` to the primary.
Consequently, seeing an active Hydra leader on the new cell is necessary but does
not by itself mean that multicell registration has completed.

The request is then processed in the following order:

1. **Validate and record the new cell on the primary.** The primary accepts only
   a secondary cell tag present in its static master configuration. It creates a
   reliable Hive mailbox for that cell, adds the cell to
   `registered_master_cell_tags`, recomputes cell roles and names, and writes
   `Master cell registered` to the primary log. An unknown cell is ignored. A
   duplicate request receives an error rather than running the bootstrap again.
2. **Send the dynamically propagated-cell set first.** Before global state is
   copied, the primary sends its complete
   `dynamically_propagated_masters_cell_tags` set to the new cell. Some of the
   following replication decisions depend on this set, so this message must
   precede replicated keys and values.
3. **Connect every registered master to every other master.** For each previously
   registered cell, the primary sends one registration message informing that
   cell about the newcomer and another informing the newcomer about the existing
   cell. Each recipient creates a Hive mailbox, records the peer in its registered
   cell set, recomputes roles and names, and logs `Master cell registered`. Thus,
   the message on an existing secondary is peer discovery; it is not that
   secondary registering at the primary again.
4. **Replicate global state in two passes.** The primary fires the
   `ReplicateKeysToSecondaryMaster` callbacks and then the
   `ReplicateValuesToSecondaryMaster` callbacks. Subsystems use the key pass to
   create global objects and identities on the new cell, and the value pass to
   copy attributes and relationships after their referenced objects exist. This
   includes state owned by the security, node-tracker, data-node-tracker,
   Cypress, cell, configuration, and object managers; it is not a copy of the
   complete primary Cypress tree.
5. **Acknowledge registration.** Only after the primary has enqueued the topology
   and global-state replication messages does it enqueue a reliable success
   response to the new cell. Reliable messages to that cell preserve their
   mailbox order. Applying the response changes the new cell's persistent state
   from `Registering` to `Registered` and writes `Successfully registered at
   primary master` to its log.

The `Master cell registered` line therefore has a local meaning: the process
that emitted it has added a peer to its own registered-cell set. For operational
verification, first find that line for the new cell tag on the primary, then
verify the corresponding peer-registration lines on all secondaries, the
`Successfully registered at primary master` line on the new secondary, and the
global-object replication activity on the new cell. Also check:

```bash
yt get //sys/@registered_master_cell_tags
yt get //sys/@dynamically_propagated_masters_cell_tags
```

Do not assign traffic-bearing roles or restart ordinary cluster components until
all of those checks are consistent. Registration makes the masters mutually
aware; it does not instantly update the cached master cell directories held by
proxies, nodes, schedulers, or master caches, as described below.

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
