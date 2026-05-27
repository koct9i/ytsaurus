<!--
Draft number: 2
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master server architecture: cell topology and inter-cell communication

Core multicell layout, Hive messaging, and cross-cell balancing mechanics.

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
3. **Monitor Hive queue depth**: a growing queue depth on any Hive channel to a secondary cell is an early sign of a fan-out overload. Check the Hive Orchid service on the primary cell at `/orchid/hive/cell_mailboxes/{cell_id}/outcoming_message_count`; for the queue-size signal itself, use the profiling gauge `/hive/outcoming_messages_queue_size` with target-cell tags.

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

- The sync is **per remote cell** (not global): for a given remote cell `R`, wait until the local cell has received and applied messages from `R`'s outgoing Hive mailbox up to the remote `last_outcoming_message_id` captured for the sync. It does **not** guarantee that unrelated Hydra mutations queued on `R` are applied locally.
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
4. **Adjust ping period**: transactions with long timeouts can use a larger `ping_period` to reduce the ping mutation rate per transaction. The native client defaults `default_ping_period` to `5s` (and requires it to be less than the transaction timeout), while the Python wrapper defaults `ping_period` to `timeout / 3`.
