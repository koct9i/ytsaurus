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
| A | Version segment identifier for the object |
| B | Version record identifier within that segment |
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

#### Read consistency for globally replicated objects { #global-object-read-consistency }

Because globally replicated objects are updated by posting the change as a Hive message from the native cell to every secondary cell, reads on different cells observe the state at different points in time:

| Read target | Consistency level | When up-to-date |
|-------------|------------------|-----------------|
| **Primary (native) cell** | Strongly consistent: always reflects the latest committed mutation. | Immediately after the write returns. |
| **Secondary cell** | Eventually consistent: reflects the state at the time the Hive message for the last write was applied. | After the Hive message for that write has been received and applied. |

The Hive channel between any two cells maintains strict per-channel ordering, so an observer on a given secondary cell always sees writes to the same object in the order they were applied on the primary. However, two independent secondaries may apply the same write at different times, so a client that reads from multiple secondary cells may temporarily see different values for the same attribute.

**When this matters operationally:**

- After modifying an account's quota on the primary cell (`//sys/accounts/<name>/@resource_limits`), a subsequent read from a secondary cell may still return the old quota until the Hive message is applied.
- After creating a new account, the account may not yet be visible on a secondary cell even if the primary cell has already confirmed the creation. Reading the account list from any secondary before the Hive message is applied will not include the new account.
- To guarantee that a secondary cell has applied a write, use `SyncWith` against that cell (see [SyncWith semantics](#syncwith-semantics)) or direct the subsequent read to the primary cell.

#### Global object update mechanics and performance implications { #global-object-update-mechanics }

Every write to a globally replicated object (account, user, group, medium, etc.) produces a **broadcast fan-out**: after the mutation is committed on the native cell, the object manager posts one Hive message per secondary cell carrying the updated writable attributes. The total cost of one attribute write scales with the number of secondary cells.

**Two-phase creation and removal**

Accounts, users, and groups (types with `TwoPhaseCreation` and `TwoPhaseRemoval` flags) use a distributed confirmation protocol to ensure every cell sees the object in a consistent state:

```
Creation:
  Native cell commits CreateObject mutation → object at CreationStarted
      │
      ├──► Hive: TReqCreateForeignObject → secondary cell 1  ──┐
      ├──► Hive: TReqCreateForeignObject → secondary cell 2  ──┤ all confirm
      └──► Hive: TReqCreateForeignObject → secondary cell N  ──┤ via TReqConfirmObjectLifeStage
                                                                │
                            vote count == N+1  ◄───────────────┘
                                     │
                          CreationPreCommitted
                                     │
                    secondaries confirm again
                                     │
                          CreationCommitted ← object becomes usable
```

Until `CreationCommitted` is reached, the object exists on the native cell but cannot yet be used as a valid reference (e.g., a new account cannot be set as the parent of another account until committed).

Removal follows the symmetric path (`RemovalStarted` → `RemovalPreCommitted` → `RemovalAwaitingCellsSync` → `RemovalCommitted`), where the object is removed from all cells only after every secondary has confirmed it released its reference counter.

**Performance implications of writes to globally replicated objects:**

- **Fan-out cost**: writing one attribute to a globally replicated object in a cluster with *N* secondary cells triggers *N* Hive messages. Each message is a mutation on a secondary cell's automaton thread.
- **Creation throughput**: two-phase creation requires two full rounds of all-secondary confirmations. Bulk account creation is therefore serialized and throughput is proportional to `1 / (2 × max_secondary_hive_round_trip_latency)`. Avoid creating thousands of accounts in a tight loop.
- **Hot global objects**: the root account (`//sys/accounts/root`) or other shared objects that are modified frequently (e.g., quota enforcement side effects from gossip) generate a steady stream of Hive fan-out mutations to all secondary cells. Heavy traffic to a hot global object can increase automaton-thread load on every cell in the cluster.
- **Large clusters**: the fan-out effect grows linearly with the number of secondary cells. Clusters with many secondary cells are more sensitive to bursts of global object mutations than single-cell deployments.

**Mitigation strategies:**

1. **Batch attribute updates**: where the type supports setting multiple attributes in one RPC (`MultisetAttributes`), batch changes to reduce the number of Hive fan-out rounds.
2. **Avoid unnecessary modifications to global objects**: even no-op attribute sets (setting an attribute to its existing value) can trigger a replication message depending on how the proxy is invoked.
3. **Monitor Hive queue depth**: a growing queue depth on any Hive channel to a secondary cell is an early sign of a fan-out overload. Check `/orchid/monitoring/hive/mailboxes/{cell_id}/outgoing_message_count` on the primary cell.

### Native and external table objects { #native-external }

When chunk multi-cell is used, a table has two representations:

- The **native** object lives on the primary cell (or portal cell). It is a normal Cypress node attached to the namespace tree. Locks and Cypress-level operations are performed against the native object.
- The **external** object lives on the secondary cell that owns this table's chunks. It has no Cypress attachment but owns the chunk-list tree. Chunk metadata, replication, and erasure operations are performed against the external object.

When a table is created, the cell hosting the native object creates it and, via Hive, orders the appropriate secondary cell to create the corresponding external object. The `external_cell_tag` attribute records which secondary cell hosts the chunk lists.

### Cell roles { #cell-roles }

The role of each secondary cell is configured at `//sys/@config/multicell_manager/cell_descriptors/{cell_tag}/roles`. Current roles:

| Role | Description |
|------|-------------|
| `chunk_host` | The cell hosts chunk metadata. New chunks are automatically assigned to chunk-host cells. |
| `dedicated_chunk_host` | The cell is dedicated to hosting chunk metadata for explicit placement via `external_cell_tag`. It is not considered for automatic chunk-host placement and must not be combined with `chunk_host`. |
| `cypress_node_host` | The cell can host Cypress subtrees moved there via portals. Subtree assignment is manual. |
| `transaction_coordinator` | The cell can act as transaction coordinator for cross-cell transactions. |
| `ex_transaction_coordinator` | The cell can coordinate externalized transactions. |
| `sequoia_node_host` | The cell can host Sequoia metadata nodes and must not be combined with `chunk_host`. |

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
2. Periodically, the sender posts outgoing messages above its persistent pointer to the receiver (via `PostMessages`). The receiver applies them as mutations, and persists its incoming progress atomically with each applied mutation.
3. The receiver returns its persistent/transient incoming pointers to the sender, which uses them to advance the sender-side persistent pointer.

The protocol is similar to TCP acknowledgements. Because both the outgoing queue and the per-channel counter are persistent, messages survive restarts on either side.

{% note warning %}

Skipping or replaying mutations from a Hive channel is dangerous. The Hive delivery protocol uses sequence numbers that must stay consistent. A mismatch causes a sanity-check failure and may break inter-cell protocols (for example, the tablet-mount handshake would stall indefinitely if the confirmation message is lost).

{% endnote %}

### Message delivery latency

Hive messages are not pushed in real time. There are two delivery mechanisms:

1. **Periodic delivery**: the sender schedules periodic `PostOutcomingMessages` runs and posts pending messages to the receiver via `PostMessages` on a configurable interval (typically a few hundred milliseconds to a few seconds).
2. **Explicit SyncWith**: a cell can call `SyncWith(remote_cell_id)` to block until all messages that were already queued on the remote cell before the call are posted by that remote sender via the normal `PostMessages` flow, delivered to the local cell, and applied locally. This is used in cross-cell read operations that require causal consistency.

### Confirmed `SyncWith` semantics (code-level) { #syncwith-semantics }

The exact guarantee implemented by `IHiveManager::SyncWith` is:

- The sync is **per remote cell** (not global): for a given remote cell `R`, wait until all mutations that were already queued on `R` at call time are durably applied on the local cell.
- The operation is effectively a local barrier keyed by the remote mailbox progress, so it is not transitive across third cells.
- `SyncWith(self)` is a no-op.
- If the remote cell is unknown/disconnected, the operation fails with `Unavailable`; if the sync exceeds `Config_->SyncTimeout`, it fails with `Timeout`.

On the object-service path, sync is part of request execution:

- The execute session collects cell tags from request metadata (`cell_tags_to_sync_with`) and transaction-replication context, then performs sync phases before/after invocation.
- By default, phase one also waits for the strongly ordered transaction barrier (Sequoia-related); this can be explicitly suppressed.
- If synchronization is required while the master is in read-only mode, the request fails (`Cannot synchronize with cells when read-only mode is active`).

### Examples and request impact { #syncwith-examples }

Typical effects on user-visible request behavior:

1. **Cross-cell metadata read with explicit sync target**
   - A client adds `cell_tags_to_sync_with=[X]` to force synchronization with cell `X` before serving a local read.
   - Result: stronger causal visibility for data replicated from `X`, at the cost of extra latency.

2. **Default execution path with automatic sync dependencies**
   - Object-service execution may add sync dependencies from transaction replication and run multiple sync phases.
   - Result: reads/writes that touch cross-cell transactional state can block longer than purely local requests.

3. **Suppressed synchronization flags**
   - Request flags such as `suppress_upstream_sync`, `suppress_transaction_coordinator_sync`, and `suppress_strongly_ordered_transaction_barrier` disable parts of synchronization.
   - Result: lower latency and fewer cross-cell waits, but the response may observe a less up-to-date or weaker ordered view.

4. **Remote cell failover or connectivity loss**
   - If a required remote cell cannot be synchronized with, `SyncWith` fails.
   - Result: the request returns a transient failure (`Unavailable`) instead of serving potentially inconsistent cross-cell state.

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

When a table is created (or when a table's chunk-list needs to be allocated to a secondary cell), the cell hosting the native Cypress node for that object calls `PickSecondaryChunkHostCell`. In the common case this is the primary cell, but for objects inside a portal subtree it is the corresponding external/portal cell for that subtree. The algorithm is:

1. Collect all registered secondary cells that have the `chunk_host` role.
2. Fetch the current chunk count for each candidate cell from the in-memory multicell statistics cache.
3. Compute the average chunk count across all candidates.
4. Split candidates into two groups: "low" (below average) and "high" (at or above average).
5. Assign a weight to each group and sample uniformly:
   - Low candidates get weight `256 + bias × 256` each.
   - High candidates get weight `256` each.
   - A random token in `[0, total_weight)` selects the winning cell.

The `bias` parameter (typically `1.0`) doubles the effective probability of low candidates compared to high ones. This gives **biased stochastic load balancing**: underloaded cells are strongly preferred but all cells can still receive new objects, avoiding starvation of any cell.

The statistics used for the decision come from periodic multicell node statistics updates (`TMulticellNodeStatistics`; see `GetMulticellNodeStatistics()`), so they can lag by a few seconds. In practice this means placement decisions during a burst of table creation events may not be perfectly balanced, but they converge quickly.

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

#### Coordinator hotspot risks and mitigation { #coordinator-hotspots }

Every active transaction generates periodic **keep-alive pings** to its coordinator cell. Each ping is a mutation on the coordinator's automaton thread, which can only execute one mutation at a time. When many transactions share the same coordinator, their pings arrive concurrently and are serialized on that cell's automaton thread, increasing mutation latency for all other operations on that cell.

The three situations that produce hotspots are:

| Pattern | Root cause | Effect |
|---------|-----------|--------|
| Many short-lived transactions all created as children of a single long-lived root transaction | Stickiness forces all children to the same coordinator as the root | All pings go to one cell regardless of how many coordinator cells are available |
| One coordinator cell given the `transaction_coordinator` role while others lack it | All new transactions are randomly assigned only to the single eligible cell | 100% of coordinator traffic lands on one cell |
| A client workload that creates very large transaction trees (deep nesting or wide fan-out) | Each level inherits the coordinator of its parent | The coordinator cell accumulates O(tree size) concurrent keep-alive pings |

**Observable symptoms of coordinator overload:**

- Automaton-thread latency on the coordinator cell rises even for unrelated operations (because mutations queue behind the flood of ping mutations).
- Commit latency for transactions on that cell increases.
- The primary cell (if it is the only `transaction_coordinator`) shows broader cluster-wide latency degradation.

**Mitigations:**

1. **Distribute coordinator roles**: assign `transaction_coordinator` to several secondary cells so that the random selection spreads load across them.
2. **Avoid unnecessary parent–child relationships**: if two groups of transactions do not share locks, start them as independent root transactions so they can land on different coordinator cells.
3. **Keep transaction trees shallow**: use `commit` / `start new root transaction` patterns instead of deep nesting when the sub-tasks are truly independent.
4. **Adjust ping period**: transactions with long timeouts can use a larger `ping_period` to reduce the ping mutation rate per transaction (the default ping period is `min(ping_period, timeout/2)`).

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

### Interference between read requests, mutations, and snapshots { #read-mutation-snapshot-interference }

Read requests, mutations, and snapshot operations are not independent in practice. They share Hydra queues, CPU, memory, and disk resources, so load in one path can increase latency in the others.

| Interference pair | Mechanism | User-visible effect |
|-------------------|-----------|---------------------|
| **Reads ↔ Mutations** | Follower reads wait for `SyncWithUpstream`; heavy mutation streams increase catch-up and automaton apply backlog. | Read latency increases, even for read-only workloads. |
| **Reads ↔ Snapshots** | Snapshot creation forks the master process; copy-on-write and memory pressure can slow CPU scheduling and cache locality while reads still execute. | Temporary tail-latency growth for reads during snapshot windows. |
| **Mutations ↔ Snapshots** | Normal snapshots do not stop mutations, but snapshot I/O and memory pressure can compete with changelog and automaton work. Read-only snapshots explicitly stop mutation acceptance. | Higher mutation commit latency; in read-only snapshot mode, write requests are rejected. |
| **Cross-cell reads ↔ Read-only mode** | Cross-cell reads that request synchronization (`cell_tags_to_sync_with` or transaction-replication sync) need `SyncWith`; object service rejects this when Hydra is read-only. | Read requests that require cross-cell freshness fail with `ReadOnly`. |

#### Practical guidance

1. Schedule snapshot builds outside peak write/read windows when possible.
2. For latency-sensitive reads, avoid unnecessary cross-cell synchronization tags; use them only when read-after-write visibility across cells is required.
3. Monitor both mutation and read indicators together: automaton CPU, mutation queue depth, and read latency percentiles.
4. Use read-only snapshot mode only for maintenance windows where temporary write unavailability is acceptable.

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

### Freshness synchronization (SyncWithUpstream)

Before any local sub-request is executed, the peer must guarantee that it is not
serving stale data. Followers call `SyncWithUpstream`: they contact the leader,
learn the current committed sequence number, and wait until they have applied all
mutations up to that point.  This sync phase is performed on the RPC thread (not
the automaton thread) and adds roughly 10–20 ms in practice.

The sync can be suppressed by setting the `suppress_upstream_sync` flag in the
request header, which allows callers to accept potentially stale data in exchange
for lower latency.

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

If no explicit roles are assigned, a secondary cell may still receive the default `cypress_node_host | chunk_host` roles unless `remove_secondary_cell_default_roles` is enabled or the cell is dynamically propagated. Assign roles explicitly to control what work the new cell serves after addition.

### Scaling recommendations

A single-cell master setup is sufficient for most clusters. Consider adding secondary chunk-host cells when:

- The automaton thread CPU on the primary cell is consistently above 70–80%.
- Master RSS memory is approaching the safety margin.
- Node registration and disposal operations are causing significant latency spikes (visible as long mutation queues in logs).

Starting with three secondary chunk-host cells provides a practical balance between operational complexity and capacity headroom. Up to 48 secondary cells are supported.

When new secondary cells are added, existing chunks do not automatically move — only new chunks are assigned to the new cells. To rebalance existing data, rewrite it into newly created tables (for example, via merge or copy-based workflows) so that new chunks are allocated under the current placement rules. Do not attempt to move an existing table's chunk tree by changing `external_cell_tag`: this attribute is not a supported knob for rebalancing existing tables.
