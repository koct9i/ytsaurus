<!--
Draft number: 4
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master server architecture: read request execution

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
