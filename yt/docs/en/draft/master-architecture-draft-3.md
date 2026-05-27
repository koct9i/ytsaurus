<!--
Draft number: 3
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master server architecture: transaction lifecycle and mutation pipeline

Transaction coordinator behavior, lifecycle cost model, and mutation ordering internals.

## Master transaction lifecycle and cost model { #transaction-lifecycle }

This section complements the user-facing [Transactions](../user-guide/storage/transactions.md#master_transactions) page. It focuses on operator-visible behavior: where transaction work runs, what scales with transaction count, and why some workloads overload one coordinator cell.

### Lifecycle

1. **Start**
   - The client chooses a coordinator cell according to [coordinator selection rules](./master-architecture-draft-2.md#coordinator-selection).
   - The start RPC creates the transaction object on that coordinator cell as a Hydra mutation.
   - `replicate_to_master_cell_tags` can replicate the transaction to selected cells immediately. Without it, replication is usually deferred until the first cross-cell access.

2. **Attach participants on demand**
   - A transaction is initially known only to its coordinator and any cells replicated eagerly at start time.
   - When a read or mutation under that transaction touches an object on another cell, the transaction is replicated to that cell before local execution.
   - For mutating paths, replication uses boomerang ordering so that the transaction context and the mutation arrive safely on the leader in the required order.
   - For read paths, replication is lighter, but it can still require the same [cross-cell freshness synchronization](./master-architecture-draft-4.md#read-transactions) as other transactional reads.

3. **Acquire locks and update state**
   - Locks are taken on the cells that own the affected objects. Conflicts are detected when locks are acquired, not at commit time.
   - Nested transactions inherit their parent's coordinator. Transactions with prerequisite transactions are pinned to the same coordinator cell.
   - The coordinator tracks the participant set so that commit or abort can later be propagated to every cell that observed the transaction.

4. **Keep the lease alive**
   - While the transaction is active, the client periodically sends keep-alive pings.
   - The effective ping period is `min(configured_ping_period, timeout / 2)`.
   - If `ping_ancestors` is enabled, the same ping also renews the lease of every ancestor transaction.
   - If the coordinator stops receiving pings for longer than the timeout, the transaction expires and is aborted.

5. **Finish**
   - `abort_tx` aborts the transaction and all nested children.
   - `commit_tx` succeeds only after child transactions have already committed.
   - For cross-cell transactions, commit uses the [transaction replication](./master-architecture-draft-2.md#transaction-replication) protocol: once locks are held, the coordinator commits and broadcasts the result to participant cells through Hive.

6. **Survive failover**
   - Transaction state is durable because it lives in Hydra changelogs and snapshots.
   - Leader failover does not by itself abort active transactions. The practical risk is lease expiration while the client is disconnected or while pings are delayed.

### Feature summary

| Feature | What it changes operationally | Main trade-off |
|---------|-------------------------------|----------------|
| Root transaction | Coordinator is chosen randomly from cells with `transaction_coordinator` role | Best load spreading |
| Nested transaction | Reuses parent coordinator and lock lineage | Simpler semantics, but creates coordinator stickiness |
| Prerequisite transactions | Force a single coordinator cell for all prerequisites | Preserves ordering, but can serialize unrelated work |
| `replicate_to_master_cell_tags` | Pays replication cost at start instead of first touch | Lower first-access latency, higher upfront traffic |
| `ping_period` | Controls steady-state keep-alive rate | Lower rate reduces load, but slows expiry detection |
| `ping_ancestors` | Renews ancestor leases together with the child | Fewer lease surprises, more work per ping |
| Cross-cell transactional reads | May need transaction replication and `SyncWith` before the read | Fresher view, higher read latency |

### Cost model

For an operator, transaction cost is dominated by coordinator mutations, cross-cell replication, and participant fanout.

**Start**

```text
start_latency
  ≈ client_rpc
  + coordinator_hydra_commit
  + optional_eager_replication
```

If the transaction starts only on the coordinator, the first cross-cell access pays the deferred replication cost later.

**Steady state**

```text
effective_ping_period = min(ping_period, timeout / 2)
coordinator_ping_qps ≈ active_transactions / effective_ping_period
```

If `ping_ancestors` is enabled, one ping refreshes more leases, but the coordinator still executes extra transaction work for the ancestor chain. Deep transaction trees therefore amplify coordinator load even when the user-visible transaction count looks moderate.

**First cross-cell touch**

```text
first_touch_latency
  ≈ transaction_replication
  + optional_sync_with_remote_cell
  + local_lock_or_mutation_cost
```

This is why workloads that touch many cells under the same transaction usually show worse tail latency than purely local transactions.

**Commit**

```text
commit_cost
  ≈ coordinator_hydra_commit
  + hive_fanout_to_participants
  + participant_apply_cost
```

For a single-cell transaction, the last two terms are absent. For a multi-cell transaction, the extra work grows roughly with the number of participant cells, not with the number of individual objects.

### Resource footprint

**CPU**

- The coordinator pays for start, ping, commit, and abort mutations.
- Participant cells pay when the transaction is replicated there, when locks are taken locally, and when the final commit or abort is applied.
- Coordinator overload usually appears first on the automaton thread, because all of this work is serialized there.

**Memory**

- Every active transaction occupies memory on the coordinator.
- Every replicated participant cell stores its own copy of transaction metadata.
- Memory grows with transaction count, nesting depth, participant-cell count, lock count, staged objects, branched nodes, and prerequisite metadata.

The user-facing transaction attributes listed in [Transactions](../user-guide/storage/transactions.md#attributes) are a good proxy for what consumes memory: `nested_transaction_ids`, `staged_object_ids`, `branched_node_ids`, `locked_node_ids`, `lock_ids`, and `resource_usage`.

**Network**

- Start, ping, commit, and abort each generate control-plane RPC traffic.
- Multi-cell transactions additionally generate Hive traffic for replication and finalization.
- Cross-cell transactional reads can add `SyncWith` traffic before the read itself runs.

### Practical implications

- Many long-lived transactions mostly consume coordinator memory and steady ping bandwidth.
- Deep or wide transaction trees mostly consume coordinator automaton CPU.
- Transactions that touch many cells mostly consume Hive traffic and participant apply capacity.
- Increasing `timeout` alone does not reduce load if `ping_period` stays small. To reduce ping pressure, both must be tuned together.

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
- **No reordering across cells.** A mutation on cell A and a mutation on cell B are independent; there is no global sequence number. Cross-cell ordering is managed by Hive channel sequence numbers (see [Hive](./master-architecture-draft-2.md#hive)).
- **Follower replication is pipelined.** The leader tracks per-follower state (`NextExpectedSequenceNumber`, in-flight counts). When a follower is healthy ("fast mode"), the leader sends the next batch immediately without waiting for the previous acknowledgment, up to the in-flight limits. When a follower falls behind or returns an error, it is demoted to "slow mode": only one request is sent at a time until it catches up.

### Follower modes

| Mode | Behavior | Trigger |
|------|----------|---------|
| **Fast** | Leader pre-advances `NextExpectedSequenceNumber` and sends the next batch immediately. | Default when follower is healthy. |
| **Slow** | Leader waits for acknowledgment before sending the next batch. | Follower RPC error, or follower returns `mutations_accepted=false`. |

In slow mode, replication to that follower takes at least one round-trip per batch. This does not by itself slow quorum commit in a 3-peer cell if the leader and another voting follower are healthy; it delays commits only when the slow follower is actually needed for quorum (for example, if another voting peer is unavailable or also slow).

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
