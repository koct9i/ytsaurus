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
| 4. New leader elected | A new leader is chosen. | Write traffic can recover, while read traffic may still see a short gap due to lease/grace logic. |
| 5. Leader grace delay ends | New leader starts serving reads after old lease is guaranteed expired. | Normal service resumes for this cell. |

In multicell setups, this timeline applies **independently to each cell**.

Possible multicell interference during failover:

- A failover in one secondary cell does not stop unrelated traffic on other cells, but any operation that touches the failed cell (for example, table/chunk metadata hosted there) will stall or retry.
- Cross-cell reads can see amplified latency because they may need both local catch-up (`SyncWithUpstream`) and remote Hive synchronization with the recovering cell.
- If the primary cell is the one failing over, user-visible impact is broader because the primary hosts the root Cypress tree and coordinates global metadata flows.

### Changelog and snapshot storage { #changelog-snapshot }

Hydra durably stores committed mutations in *changelogs* (also called journals). A changelog is an append-only file; mutations are appended sequentially. On disk, changelog files are stored in the location configured as `changelogs` in the master static configuration.

Periodically, Hydra takes a **snapshot** — a complete serialized image of the automaton state. After a snapshot is written, changelogs before the snapshot point are no longer needed and can be pruned. On restart, Hydra loads the latest snapshot and then replays only the changelogs that follow it.

Snapshot creation uses a **fork** on master processes (forked child serializes the state while the parent continues running). This means the master process needs roughly double its working-set memory available at snapshot time. For large clusters the snapshot can be hundreds of gigabytes. Storage for snapshots is configured separately from changelogs.

{% note warning %}

The disk used for changelogs should have **good sequential-write performance**. Slow changelog writes increase mutation latency for all writers. NVMe SSDs are recommended.

{% endnote %}

### Erasure journals (tablet cells only)

For tablet cells the journal write amplification can be reduced by using **erasure journals**: instead of writing a full mutation to each of N replicas, the mutation is split and XOR-parity parts are written, similar to erasure-coded chunks. This reduces write amplification at the cost of slightly higher read latency during recovery. Erasure journals are most beneficial for ordered dynamic tables (queues) where write throughput is critical.

## Single-cell master setup { #single-cell }

In a minimal deployment the entire master state lives in a single **master cell**. The cell consists of several peer processes (typically three or five) running Hydra. All of Cypress, all chunk metadata, all transaction state, and all global objects reside in this one cell.

A single cell can handle:

- Millions of nodes in Cypress.
- Hundreds of millions of chunks.
- Thousands of Data Nodes.

CPU and RAM of the master process are the main scaling limits. When the cluster grows beyond what a single cell can serve, the multi-cell architecture is used.

## Multi-cell master architecture { #multicell }

Multi-cell is the horizontal scaling approach for master servers. There are two orthogonal dimensions of sharding:

| Dimension | Name | What is sharded |
|-----------|------|-----------------|
| Chunk metadata | **Chunk multi-cell** | Tables are split so that their chunk-list trees live on secondary cells while the Cypress node stays on the primary cell. |
| Cypress subtrees | **Portals** | Entire Cypress subtrees are moved to secondary cells, so different parts of the namespace are hosted on different cells. |

### Primary and secondary cells

In any multi-cell setup there is exactly one **primary cell** and one or more **secondary cells**. Together they form the **master group**.

- The primary cell hosts the root Cypress tree, global objects (accounts, media, racks, etc.), and coordinates transactions.
- Secondary cells host chunks (when the `chunk_host` role is assigned) or Cypress subtrees (when the `cypress_node_host` role is assigned).

Each individual cell is itself a Hydra group (leader + followers), completely independent from other cells. The master group is the union of all cells.

### Cell tags and Object IDs { #object-id }

Every cell in the master group has a short numeric **cell tag**. Cell tags are unique not only within a cluster but within a whole group of federated clusters (for cross-cluster replication scenarios).

Every persistent object (Cypress node, chunk, transaction, account, etc.) has a 128-bit **Object ID** with the following structure:

| Part | Meaning |
|------|---------|
| A | Hydra term (election epoch) in which the object was created |
| B | Mutation sequence number within that term |
| C | Encoded **Object Type** and **Cell Tag** of the creating cell |
| D | Random component |

The cell tag embedded in part C allows any component to determine which master cell owns a given object without contacting the master. In particular, Data Nodes use the cell tag in chunk IDs to route heartbeats to the correct secondary cell.

### Object replication policies

Each type of persistent object has one of two replication policies:

| Policy | Examples | Description |
|--------|----------|-------------|
| **Cell-local** | Chunks, chunk lists | The object lives only on one cell (determined by its cell tag). |
| **Globally replicated** | Accounts, media, racks, Data Nodes, tablet cell bundles | When created or modified on the primary cell, the object is automatically replicated to all secondary cells via Hive. Every cell therefore has a full copy of these objects, with some attributes being cell-local (e.g. per-cell resource usage). |

Globally replicated objects enable each cell to perform local validations (quota checks, codec validation, etc.) without round-tripping to the primary cell on every mutation.

### Native and external table objects { #native-external }

When chunk multi-cell is used, a table has two representations:

- The **native** object lives on the primary cell (or portal cell). It is a normal Cypress node attached to the namespace tree. Locks and Cypress-level operations are performed against the native object.
- The **external** object lives on the secondary cell that owns this table's chunks. It has no Cypress attachment but owns the chunk-list tree. Chunk metadata, replication, and erasure operations are performed against the external object.

When a table is created, the primary cell creates the native object and, via Hive, orders the appropriate secondary cell to create the corresponding external object. The `external_cell_tag` attribute records which secondary cell hosts the chunk lists.

### Cell roles { #cell-roles }

The role of each secondary cell is configured at `//sys/@config/multicell_manager/cell_descriptors/{cell_tag}/roles`. Current roles:

| Role | Description |
|------|-------------|
| `chunk_host` | The cell hosts chunk metadata. New chunks are automatically assigned to chunk-host cells. |
| `dedicated_chunk_host` | The cell is dedicated to hosting chunk metadata. |
| `cypress_node_host` | The cell can host Cypress subtrees moved there via portals. Subtree assignment is manual. |
| `transaction_coordinator` | The cell can act as transaction coordinator for cross-cell transactions. |
| `ex_transaction_coordinator` | The cell can coordinate externalized transactions. |
| `sequoia_node_host` | The cell can host Sequoia metadata nodes. |

If no roles are configured, the secondary cell is idle only when no default roles are applied (for example, for dynamically propagated descriptors, or when `remove_secondary_cell_default_roles` is enabled). Otherwise, non-dynamically-propagated secondary cells may still receive the default roles `cypress_node_host | chunk_host`.

## Portals: Cypress sharding { #portals }

A **portal** is a special Cypress node that transparently redirects requests to a subtree hosted on a different (portal) cell. From the user perspective a portal looks like a normal directory, but internally the subtree's metadata lives on the portal cell.

Portals allow the Cypress namespace to be distributed across multiple cells when a single cell can no longer efficiently manage the full tree. Subtrees can be **externalized** (moved to a portal cell) or **internalized** (moved back to the primary cell). The process serializes the entire subtree and transmits it via Hive.

The portal cell behaves like a primary cell for its subtree: it hosts Cypress nodes, manages locks, and coordinates transactions touching that subtree. Chunk lists for tables within the portal's subtree still live on secondary chunk-host cells as usual.

## Hive: inter-cell messaging { #hive }

**Hive** is the reliable messaging layer that connects all cells in the master group (and, more broadly, any pair of Hydra cells, including tablet cells and cross-cluster replication). Every pair of cells that need to communicate has a **unidirectional channel** from A to B; bidirectional communication requires two channels.

### Mailboxes

Each channel has a **mailbox** on each side:

- The *sender mailbox* queues outgoing messages.
- The *receiver mailbox* tracks the application status of incoming messages.

Messages in Hive are always **mutations** to be applied on the receiving cell. This is natural: the only durable way to affect a cell's state is to run a mutation on it.

### Reliable and unreliable messages

| Mode | Delivery guarantee | When usable |
|------|-------------------|-------------|
| **Reliable** | At-least-once, in order. If the sending cell restarts, the message survives because it was written to the persistent mailbox inside a mutation. | Can only be posted from within a mutation. |
| **Unreliable** | Best-effort. Not persisted; lost on sender restart. | Can be posted outside a mutation (e.g. from a periodic background task). Used for non-critical statistics. |

### Reliable delivery protocol

The reliable delivery protocol works as follows:

1. The sender's persistent mailbox is an ordered queue of outgoing mutations with two pointers:
   - **Persistent pointer**: the last message confirmed as durably applied by the receiver.
   - **Transient pointer**: the last message sent (but not yet confirmed).
2. Periodically, the receiver cell polls the sender asking for new messages above the transient pointer. Received messages are applied as mutations, and the receiver's running count of applied messages is persisted atomically with each applied mutation.
3. The receiver reports its count back to the sender, which advances the persistent pointer.

The protocol is similar to TCP acknowledgements. Because both the outgoing queue and the per-channel counter are persistent, messages survive restarts on either side.

{% note warning %}

Skipping or replaying mutations from a Hive channel is dangerous. The Hive delivery protocol uses sequence numbers that must stay consistent. A mismatch causes a sanity-check failure and may break inter-cell protocols (for example, the tablet-mount handshake would stall indefinitely if the confirmation message is lost).

{% endnote %}

### Message delivery latency

Hive messages are not pushed in real time. There are two delivery mechanisms:

1. **Periodic polling**: the receiver polls the sender on a configurable interval (typically a few hundred milliseconds to a few seconds).
2. **Explicit SyncWith**: a cell can call `SyncWith(remote_cell_id)` to block until all messages from that remote cell queued before the call are received and applied locally. This is used in cross-cell read operations that require causal consistency.

### The three-cell ordering problem { #three-cell-problem }

Hive channels are **per-pair** and do not provide global ordering across channels. Consider three cells A, B, C:

- A sends message M1 directly to C.
- A sends message M2 to B, and B forwards a dependent message M3 to C.

Even if A sent M1 before M2, C may receive M3 before M1, because the two paths (A→C and A→B→C) are independent channels. Code that relies on causal ordering across more than two cells must explicitly use `SyncWith` or design mutations to be idempotent and order-independent.

### Avenue: virtual mailboxes for tablets { #avenue }

The normal Hive channel is fixed between two cell IDs. For dynamic tables, a **tablet** is a virtual entity that can migrate between tablet cells during smooth tablet transfer. The *Avenue* abstraction provides a virtual mailbox that is logically bound to the tablet rather than to a fixed cell, and can migrate alongside the tablet across cell boundaries. This prevents message loss or duplication during tablet migration.

## Cross-cell features

### Account usage gossip { #gossip }

Account quotas must be enforced cluster-wide, but resource usage is tracked locally on each cell. A periodic **gossip** process aggregates per-cell usage:

1. Each secondary cell reports its local account usage to the primary cell via Hive.
2. The primary sums up contributions and computes a global usage figure.
3. The primary distributes the global usage back to all secondary cells so that each can enforce the global quota during chunk creation.

Because gossip is periodic (not synchronous), there is a small window during which a quota overcommit is possible. The system will detect and correct it during the next gossip cycle.

### Transaction replication { #transaction-replication }

Transactions in {{product-name}} use a simplified two-phase commit that exploits the fact that transaction commit on the master **cannot fail** once the necessary locks are held. This allows:

1. The transaction's coordinator cell (typically the primary cell) to commit the transaction by simply broadcasting a `Commit` message via Hive to all cells that participated in the transaction.
2. Cells that were not originally aware of the transaction are notified via Hive as soon as any action under the transaction touches their state.

Because Hive preserves per-channel ordering, transaction start and commit messages from the same coordinator always arrive in the correct order on every participating cell.

## Load balancing across secondary cells { #load-balancing }

### Chunk placement { #chunk-placement }

When a table is created (or when a table's chunk-list needs to be allocated to a secondary cell), the primary cell calls `PickSecondaryChunkHostCell`. The algorithm is:

1. Collect all registered secondary cells that have the `chunk_host` role.
2. Fetch the current chunk count for each candidate cell from the in-memory multicell statistics cache.
3. Compute the average chunk count across all candidates.
4. Split candidates into two groups: "low" (below average) and "high" (at or above average).
5. Assign a weight to each group and sample uniformly:
   - Low candidates get weight `256 + bias × 256` each.
   - High candidates get weight `256` each.
   - A random token in `[0, total_weight)` selects the winning cell.

The `bias` parameter (typically `1.0`) doubles the effective probability of low candidates compared to high ones. This gives **biased stochastic load balancing**: underloaded cells are strongly preferred but all cells can still receive new objects, avoiding starvation of any cell.

The statistics used for the decision are collected by a periodic gossip round (see [Account usage gossip](#gossip)), so they can lag by a few seconds. In practice this means placement decisions during a burst of table creation events may not be perfectly balanced, but they converge quickly.

{% note info %}

The chunk count statistic reflects the *number of chunks* assigned to each cell, not the disk space or CPU usage. For heterogeneous cells (different hardware) this may produce suboptimal placement. Consider using dedicated chunk-host cells with uniform hardware if precise balance is required.

{% endnote %}

Chunk placement is irreversible for existing tables. The `external_cell_tag` attribute of a table records which secondary cell holds the chunk lists and **cannot be changed** without rewriting the data.

### Transaction coordinator selection { #coordinator-selection }

When a client starts a master transaction without specifying a coordinator cell, the transaction manager selects one randomly from all cells with the `transaction_coordinator` role. Because the selection is made client-side using a uniform random choice, transaction load is distributed evenly across coordinator cells over time.

Coordinator selection becomes **sticky** in the following situations:

| Situation | Rule |
|-----------|------|
| Transaction has a parent transaction | Coordinator is forced to the same cell as the parent. |
| Transaction specifies `CoordinatorMasterCellTag` | The named cell is used. |
| Transaction lists prerequisite transactions | All prerequisites must be from the same cell; that cell is used as coordinator. |

Stickiness ensures parent and child transactions, as well as dependent transactions, are always managed by the same coordinator. This simplifies lock inheritance and commit ordering but can create hotspots if many long-lived transactions are pinned to one cell. When in doubt, avoid creating many child transactions under a common parent unless they must share coordinator state.

## Mutation ordering and commit pipeline { #mutation-pipeline }

All durable state changes in a Hydra cell follow a strict pipeline:

```
Client/internal code
      │
      ▼
 MutationDraftQueue          (lock-free MPSC queue, accessible from any thread)
      │
      ▼ SerializeMutations() — runs on control thread, period: mutation_serialization_period (5 ms default)
      │
      ▼ LogMutations()       — assigns sequence numbers, serializes to record format, appends to Changelog
      │
      ├──► Changelog::Append()  [leader local disk, async write]
      │
      ├──► FlushMutations()  — sends records to followers via AcceptMutations RPC
      │         (each follower writes to its own Changelog)
      │
      ▼ OnMutationsLogged() / OnMutationsAcceptedByFollower()
      │
      ▼ MaybePromoteCommittedSequenceNumber()
      │      — scans per-peer LastLoggedSequenceNumber
      │      — promotes CommittedState when quorum (floor(N/2)+1) peers have logged
      │
      ▼ OnCommittedSequenceNumberUpdated()
      │
      ▼ ScheduleApplyMutations()  [posted to automaton thread]
      │
      ▼ ApplyMutations()          [automaton thread, serial]
      │
      ▼ Promise resolved → RPC handler returns to caller
```

Key properties of this pipeline:

- **Strict FIFO per cell.** Mutations within a single cell are always applied in their sequence-number order. No mutation can be applied until all preceding mutations have been applied.
- **Batching.** The control thread accumulates mutations from the draft queue and logs them as a batch. Batch size is bounded by `max_commit_batch_record_count` (default 10 000 records). The serialization and flush executors run every `mutation_serialization_period` / `mutation_flush_period` (both default to 5 ms). Enable `minimize_commit_latency` to trigger flush immediately after serialization instead of waiting for the next period tick.
- **No reordering across cells.** A mutation on cell A and a mutation on cell B are independent; there is no global sequence number. Cross-cell ordering is managed by Hive channel sequence numbers (see [Hive](#hive)).
- **Follower replication is pipelined.** The leader tracks per-follower state (`NextExpectedSequenceNumber`, in-flight counts). When a follower is healthy ("fast mode"), the leader sends the next batch immediately without waiting for the previous acknowledgment, up to the in-flight limits. When a follower falls behind or returns an error, it is demoted to "slow mode": only one request is sent at a time until it catches up.

### Follower modes

| Mode | Behavior | Trigger |
|------|----------|---------|
| **Fast** | Leader pre-advances `NextExpectedSequenceNumber` and sends the next batch immediately. | Default when follower is healthy. |
| **Slow** | Leader waits for acknowledgment before sending the next batch. | Follower RPC error, or follower returns `mutations_accepted=false`. |

In slow mode, follower replication latency is at least one round-trip per batch, which can significantly slow quorum commit when `N=3` (only 2 peers needed but one is lagging).

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

### Memory

The master keeps the entire Cypress tree, all chunk metadata, and all replicated global objects in RAM. Memory usage grows with:

- Number of nodes in Cypress.
- Number of chunks and replicas.
- Number of globally replicated objects (accounts, media, etc.) × number of cells.

Monitor `yt_resource_tracker_memory_usage_rss{service="yt-master"}`. Because snapshot creation uses `fork`, the master process must have at least **double** its working-set memory available on the host.

### Node registration and disposal

When a Data Node registers with the master (or its liveness transaction expires), the master must process a full heartbeat listing all chunks on that node. For nodes with many chunks, this is a heavy mutation. When a node is disposed (its liveness transaction aborted), the master must update the replica sets of all chunks on that node. Disposal of locations on a single node is processed sequentially. This is the primary reason why adding secondary chunk-host cells reduces the per-cell disposal cost: each cell handles only the chunks assigned to it.

### Changelog I/O latency

Mutation latency is directly tied to changelog write latency, because mutations are not committed until the write quorum has persisted the changelog entry. Use fast NVMe storage for changelogs. Monitor `yt_changelogs_available_space{service="yt-master"}` to ensure space does not run out.

### Snapshot I/O and forking

Snapshot creation causes the master process to fork. During the fork, copy-on-write pages may cause elevated memory usage. After the fork, the parent continues applying mutations while the child serializes state to disk. Slow snapshot writes extend the period of elevated memory pressure but do not block mutations. Monitor `yt_snapshots_available_space{service="yt-master"}`.

## Administration

### Snapshots and read-only mode

Use `yt-admin build-master-snapshots --read-only --wait-for-snapshot-completion` to create a clean snapshot with an empty subsequent changelog. This is required before major updates or before adding new master cells. In read-only mode the master accepts no mutations; this ensures the snapshot captures a fully quiesced state.

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

Adding secondary cells requires a complete cluster downtime. For detailed steps, see [Extending master servers](../../admin-guide/cell-addition.md).

After cells are added and global objects are replicated, assign roles via:

```bash
yt set //sys/@config/multicell_manager/cell_descriptors/<cell_tag>/roles '[chunk_host]'
```

Until roles are assigned, secondary cells accept no work and serve no traffic.

### Scaling recommendations

A single-cell master setup is sufficient for most clusters. Consider adding secondary chunk-host cells when:

- The automaton thread CPU on the primary cell is consistently above 70–80%.
- Master RSS memory is approaching the safety margin.
- Node registration and disposal operations are causing significant latency spikes (visible as long mutation queues in logs).

Starting with three secondary chunk-host cells provides a practical balance between operational complexity and capacity headroom. Up to 48 secondary cells are supported.

When new secondary cells are added, existing chunks do not automatically move — only new chunks are assigned to the new cells. To rebalance existing data, rewrite it into newly created tables (for example, via merge or copy-based workflows) so that new chunks are allocated under the current placement rules. Do not attempt to move an existing table's chunk tree by changing `external_cell_tag`: this attribute is not a supported knob for rebalancing existing tables.
