---
type: Draft Article
title: "Draft-8: Dynamic tables production operations overview"
last_modified: 2026-05-27T00:00:00Z
tags: [dynamic-tables, operations, administration]
status: draft
---

# Dynamic tables production operations overview

## Operating dynamic tables in production { #operations_in_production }

### Data path and background processes

For both sorted and ordered dynamic tables, writes are first placed into in-memory `dynamic store` and then converted into on-disk chunks by flush.

After flush, background compaction and partitioning continue reorganizing data:
- Compaction reduces overlap, merges versions/tombstones, and applies deletion policies.
- Partitioning keeps tablet internals balanced for reads and background work.

This means write success and physical layout optimization are different stages. For effective operation, monitor not only write/query latency but also tablet structure and background progress.

### Administration quick checklist

Before enabling high traffic on a new table:
1. Choose table type and schema carefully:
   - [Sorted dynamic tables](../user-guide/dynamic-tables/sorted-dynamic-tables.md): key-value and range queries with MVCC.
   - [Ordered dynamic tables](../user-guide/dynamic-tables/ordered-dynamic-tables.md): queue-like append/read-by-index workload.
2. Set realistic tablet count and balancing configuration.
3. Select table format (`optimize_for`) based on access pattern.
4. Set retention/cleanup settings (`min_data_ttl`, `max_data_ttl`, `min_data_versions`, `max_data_versions`).
5. Mount and check tablet/cell health before opening traffic.

During operation:
1. Watch table/tablet attributes (`@tablets`, `@tablet_errors`, structure counters).
2. Track key latency/RPS/error metrics on tablet nodes.
3. Remount after changing mount- or reader-related options.
4. Use forced operations (for example, forced compaction) only as an explicit operational intervention.

### Tablet-cell reign during upgrades and movement

Tablet cells have a **tablet reign**, the tablet automaton's persistence
compatibility version. It is analogous to master reign, but it versions
tablet-cell snapshots and mutations and has its own numeric range. Inspect the
current reign on the tablet node that hosts a cell:

```bash
yt get //sys/tablet_nodes/<address>/orchid/tablet_cells/<cell-id>/reign
```

For an individual tablet, `yt get #<tablet-id>/orchid/reign` reports the reign
associated with that tablet. Prefer the cell-slot path when checking which
binary/reign is running on a particular node; the tablet path is useful when
diagnosing a specific tablet or smooth-movement action.

Reign matters operationally during tablet movement. Source and target servants
exchange reign-bearing state, and smooth movement rejects incompatible content
instead of applying it under a different persistent format. During a rolling
upgrade, compare the source and target cell reigns when movement is aborted or
stalls around a node restart. Do not compare tablet-reign numbers with master or
chaos reigns: each is a separate compatibility domain.

For the other reigns and system-table schema versions used by cluster
components, see [Component compatibility and persistent-state versions](components-compatibility-draft-22.md).

### Tablet actions

A tablet action is a master object under `//sys/tablet_actions` that coordinates
a multi-step change while keeping the participating tablets locked to one
operation. Balancers and the cell decommissioner create actions, and operators
can inspect them when a move or reshard does not finish. A tablet cannot join a
second action until the first action reaches a terminal state and is unbound.

#### Action types

| `@kind` | Operation | Important constraints |
| --- | --- | --- |
| `move` | Moves one or more tablets to specified cells, or lets the master choose healthy destination cells. | Tablets remain logically unchanged, but the ordinary path freezes and unmounts them before mounting them at the destination. |
| `reshard` | Replaces a consecutive tablet range using `pivot_keys` or `tablet_count`. | All tablets belong to one table; `inplace_reshard` keeps the result in the same cell and can retain preloaded chunks. |
| `smooth_move` | Moves one mounted tablet by creating an auxiliary servant and switching service to it. | Requires exactly one destination cell, avenues, and smooth movement enabled; it is unavailable for replicated tables and tables linked to hunk storage. |

For a user-created action, required and optional creation attributes include
`kind`, `tablet_ids`, `cell_ids`, `pivot_keys`, `tablet_count`,
`skip_freezing`, `inplace_reshard`, and `correlation_id`. Use the normal table
commands for routine move and reshard operations; direct action creation is
primarily useful to control or diagnose an advanced workflow. The resulting
object exposes those inputs together with `@state`, `@error`, and retention
attributes:

```bash
yt list //sys/tablet_actions --attributes kind,state,tablet_ids,cell_ids,error
yt get //sys/tablet_actions/<action-id>/@
```

`@correlation_id` connects an action to tablet-balancer logs. On failure,
inspect `@error` before retrying; also inspect the tablets and target cells
rather than assuming that a stationary state is a scheduler delay.

#### Execution sequence and states

An ordinary `move` or `reshard` normally follows:

```text
preparing
  -> [provisionally_flushing -> provisionally_flushed]
  -> [freezing] -> frozen
  -> unmounting -> unmounted
  -> mounting -> mounted -> completed
```

Square brackets mark conditional stages. A provisional flush is requested only
when required for the action. `skip_freezing` advances directly to `frozen`,
but does not skip unmount and mount. At `unmounted`, a reshard replaces the old
tablet range and the master validates mount settings; a move only computes or
uses its destination assignment. If the action has no explicit `cell_ids` and
the bundle has no healthy cells, it becomes `orphaned` instead of mounting.
Once the bundle is healthy, the decommissioner's orphan scan kicks the action
back into the sequence at `unmounted`.

A `smooth_move` uses a separate sequence and does not freeze or unmount the
main servant:

```text
preparing -> mounting_auxiliary -> waiting_for_smooth_move -> completed
```

If smooth movement fails after it starts, the action enters
`aborting_smooth_move`, removes or aborts the auxiliary servant as appropriate,
and ends in `failed`. Other action errors normally lead through `failing` to
`failed`; the manager attempts to remount tablets missed by the action before
unbinding it. `completed` and `failed` are terminal states. Do not remove a
running `smooth_move` action: the master rejects that removal because the
servant handoff must first finish or abort.

#### Controls, retention, and throttling

Finished actions are reference-counted and normally disappear automatically.
Creation attributes `keep_finished`, `expiration_time`, and
`expiration_timeout` control retention and are mutually exclusive.
`keep_finished=%true` retains the result indefinitely; `expiration_time` uses
an absolute deadline; `expiration_timeout` starts when the action finishes.
The master scans finished actions according to:

```bash
yt get //sys/@config/tablet_manager/tablet_action_manager/tablet_actions_cleanup_period
```

The default cleanup period is one minute. Retention affects observability, not
execution: keeping a finished object does not keep its tablets locked or count
it as an active action. Smooth movement has an additional master switch:

```bash
yt get //sys/@config/tablet_manager/enable_smooth_tablet_movement
```

There is no single global tablet-action execution throttler. The producer of
an action controls admission. In particular, tablet-cell decommission uses
`decommission_throttler` to limit newly created move actions and
`kick_orphans_throttler` to limit restarts of orphaned actions. Tablet
balancers have their own schedules and concurrency controls, while explicitly
created user actions are validated and start immediately. Consequently, tune
the component generating excess actions rather than treating action state
transitions as a throughput queue. The decommission-specific throttlers are
described below.

#### What limits tablet-action concurrency

Distinguish **admission rate**, **number of active actions**, and **execution
throughput**. They are limited at different layers:

* **Tablet exclusivity.** A tablet can be bound to only one unfinished action.
  An overlapping move or reshard is rejected rather than queued. Actions also
  require tablets of one owner in stable, compatible states; reshard inputs
  must form a consecutive range. These checks often impose the first practical
  concurrency limit on repeated operations against one table.
* **Producer budgets.** Parameterized tablet-balancer groups can limit the
  number of actions selected in a balancing pass with `max_action_count` under
  the bundle's `@tablet_balancer_config/groups` settings. Balancer schedules
  determine how frequently another batch is planned. The legacy master
  balancer also avoids some bundle or table balancing work while relevant
  actions are active. These are producer policies, not a master-wide cap on
  actions created by every source.
* **Decommission admission.** `decommission_throttler` is shared by the
  tablet-cell decommissioner and charges one unit for each move action it
  creates. It limits the rate at which decommission work is admitted, but it
  is not a ceiling on actions already active. `kick_orphans_throttler`
  similarly limits how many orphaned actions are restarted.
* **Destination availability.** A move without explicit `cell_ids` needs a
  healthy cell with usable tablet capacity in the same bundle. If none is
  available, the action becomes `orphaned`. Explicit targets must be active
  and in the same bundle. Thus adding action admission capacity cannot overcome
  an unhealthy bundle or a lack of target slots.
* **Per-tablet execution.** Freeze, flush, unmount, mount, snapshot, preload,
  and smooth-servant handoff are asynchronous tablet-node work. Node CPU,
  memory, store flush bandwidth, network, and the capacity of destination
  cells determine how quickly admitted actions cross these barriers. One slow
  participant keeps a multi-tablet action in its current state.
* **Master serialization and validation.** State changes are Hydra mutations,
  and the master validates mount configuration and assignment before mounting.
  Under master load, mutation throughput can therefore become another bound,
  even when tablet nodes have spare capacity.

For diagnosis, compare the number of unfinished objects in
`//sys/tablet_actions` with the rate at which their `@state` changes. A stable,
small action population usually points to producer admission or schedule
limits. A growing population whose states do not advance points instead to
tablet-node work, destination health/capacity, or master mutation throughput.
Do not raise a producer limit until identifying which of these layers is the
bottleneck.

### Decommissioning and removing a tablet cell

Removing a tablet-cell object normally starts an asynchronous, graceful
decommission; it does not immediately destroy the cell. Use either of the
equivalent object paths:

```bash
yt remove //sys/tablet_cells/<cell-id>
# or
yt remove '#<cell-id>'
```

The master first stops treating the cell as an active placement target and
moves each of its tablets through tablet actions. Once every master reports
that the cell is decommissioned and the aggregate tablet count is zero, the
primary master asks the tablet node to decommission the cell. After the node
acknowledges this step, the master removes the cell object automatically. An
unfinished tablet action involving the cell delays this workflow: resolve or
wait for the action rather than repeatedly issuing `remove`.

The component that requests removal is not necessarily the component that
performs the drain. An operator can issue the remove request directly. The
Bundle Controller can also request removal when automatic tablet-cell
management is enabled and the bundle has more cells than its target. In that
case the controller selects cells, removes their Cypress nodes transactionally,
and records removal as in progress. It limits only the concurrency of these
Cypress write requests and reports `stuck_tablet_cell_removal` if a cell does
not disappear before the configured timeout. The controller does **not** move
the tablets or run the cell protocol: the remove request reaches the primary
master, whose tamed-cell manager changes the lifecycle state, and whose tablet
cell decommissioner creates and supervises the evacuation actions.

Avoid `yt remove --force` during routine operations. Force removal skips the
normal node-side completion gate and is intended for recovery when graceful
decommission cannot finish. In particular, it can discard cell transaction
leases instead of waiting for the normal handoff.

#### Status and progress

The cell remains addressable by object ID while decommission is in progress.
Inspect these attributes together:

```bash
yt get '#<cell-id>/@tablet_cell_life_stage'
yt get '#<cell-id>/@tablet_count'
yt get '#<cell-id>/@status'
yt get '#<cell-id>/@multicell_status'
yt get '#<cell-id>/@peers'
```

`@tablet_cell_life_stage` is the clearest phase indicator:

| Value | Meaning |
| --- | --- |
| `running` | No removal is in progress. |
| `decommissioning_on_master` | The masters are draining tablets and converging multicell status. |
| `decommissioning_on_node` | The drain is complete at the masters and the tablet node has been asked to finish decommissioning. |
| `decommissioned` | The node acknowledged decommission; automatic object removal may still be pending. |

During `decommissioning_on_node`, the tablet node suspends the cell and puts
its transaction manager into removing mode. It acknowledges completion only
after the tablet map is empty, the transaction manager and transaction
supervisor are decommissioned, and the lease manager is fully decommissioned.
The node checks these predicates periodically (10 seconds by default). Thus a
cell with `@tablet_count = 0` can legitimately remain in this stage while
transactions or leases drain. `suppress_tablet_cell_decommission` bypasses
neither condition; it deliberately prevents the check from completing.

Use `@tablet_count` as the drain-progress counter. It must reach zero before
the cell can advance to node-side decommission. `@status` contains the
cluster-wide `health` and `decommissioned` result; on a multicell cluster,
`@multicell_status` exposes each master's contribution and helps identify a
lagging secondary master. `@peers` is useful when the lifecycle is stuck on
the node side: check peer addresses, states, and last-seen information. The
final successful state is usually disappearance of the object:

```bash
yt exists '#<cell-id>'
```

Do not interpret a healthy cell as a completed drain. Health, lifecycle,
tablet count, and multicell decommission status describe different gates.

##### Why `@tablet_count` can rise at the start

Treat `@tablet_count` as a progress observation, not as a monotonically
decreasing counter. In particular, requesting decommission for every cell does
not cancel tablet actions that were already running. The decommissioner first
lets an unfinished action that uses a retiring cell become unlinked. This
includes both an action whose tablet is still mounted in that cell and an
action that already names the cell as a destination. Such an action can finish
mounting its tablets before the decommissioner starts moving those tablets
away, so the destination cell's count rises and only then begins to fall.

A concurrent reshard can make the increase larger: when the action reaches
`unmounted`, it replaces its input range with the requested number of output
tablets. If those outputs are mounted in the retiring cell, `@tablet_count`
reflects the new, larger tablet cardinality before the drain actions are
created. This is why draining all cells simultaneously does not make the first
sample a fixed upper bound on the remaining work.

On a multicell cluster, `@tablet_count` is also an aggregate fetched from the
primary and secondary masters rather than an atomic snapshot of all of them.
Tablet-action changes can become visible between the individual master
responses, adding short-lived jumps around action completion. Confirm the cause
by inspecting actions that reference the cell:

```bash
yt list //sys/tablet_actions --attributes kind,state,tablet_ids,cell_ids,error
yt get '#<cell-id>/@tablet_ids'
yt get '#<cell-id>/@multicell_status'
```

Wait for pre-existing move and reshard actions to finish, then evaluate the
trend across several decommission checks. A temporary increase followed by
progressing move actions is expected convergence. A count that remains flat or
keeps increasing warrants checking for repeatedly created balancer actions,
orphaned moves, unhealthy destination capacity, or a lagging secondary master.

#### Decommissioner controls and throttlers

The dynamic master configuration is under
`//sys/@config/tablet_manager/tablet_cell_decommissioner`. The principal
settings are:

| Setting | Purpose | Default |
| --- | --- | --- |
| `enable_tablet_cell_decommission` | Enables creation of tablet-move actions for retiring cells and transition to node-side decommission. | `%true` |
| `enable_tablet_cell_removal` | Enables automatic removal after decommission completes. | `%true` |
| `decommission_check_period` | Periodic scan interval for retiring cells. | 10 seconds |
| `orphans_check_period` | Periodic scan interval for retrying orphaned tablet actions. | 10 seconds |
| `decommission_throttler` | Limits tablet-move actions created by decommissioning; one acquired unit corresponds to one tablet action. | limit 200 |
| `kick_orphans_throttler` | Limits orphaned actions restarted after their bundle becomes healthy; one acquired unit corresponds to one action. | limit 200 |

For example, inspect the live configuration before changing it:

```bash
yt get //sys/@config/tablet_manager/tablet_cell_decommissioner
yt get //sys/@config/tablet_manager/tablet_cell_decommissioner/decommission_throttler
yt get //sys/@config/tablet_manager/tablet_cell_decommissioner/kick_orphans_throttler
```

A tight `decommission_throttler` makes `@tablet_count` fall slowly because
only a limited number of moves can be submitted. A tight
`kick_orphans_throttler` limits recovery progress after move actions become
orphaned; it does not limit the initial moves. Before raising either limit,
check bundle health, pending tablet actions, tablet-node capacity, and the
load caused by moves. Throttling protects the cluster from a burst of work;
disabling it or shortening scan periods does not repair an unhealthy bundle.

If `enable_tablet_cell_decommission` is false, a requested removal can remain
at `decommissioning_on_master`. If `enable_tablet_cell_removal` is false, a
fully decommissioned object is deliberately retained. These switches are
useful for controlled testing and incident response, but operators should
record the override and restore it after resolving the issue.

Importantly, `enable_tablet_cell_decommission` controls more than automatic
move-action creation. The same guarded scan performs the transition from
`decommissioning_on_master` to `decommissioning_on_node`. If an operator
disables the setting and manually moves every tablet away, the empty cell's
local and multicell status can converge to decommissioned, but the graceful
protocol still remains at `decommissioning_on_master`: the node-side request is
not sent. A safe manual-evacuation workflow is:

1. Disable automatic cell decommission and create explicit move actions with
   healthy targets.
2. Wait until the actions finish, the source `@tablet_count` is zero, and
   multicell status has converged.
3. Re-enable `enable_tablet_cell_decommission` so the master can request the
   node-side transaction and lease drain and finish removal.

Alternatively, move the tablets while the cell is still `running`, then issue
the remove with the decommissioner enabled. Ensure that balancers cannot place
new tablets back into the source during this interval. Leaving
`enable_tablet_cell_removal` enabled does not compensate for a disabled
decommissioner: automatic removal requires a cell whose lifecycle has already
reached `decommissioned`. Avoid using force removal merely to bypass this
stalled phase, since force marks the master-side cell complete without waiting
for the tablet node's normal drain acknowledgement.

#### What limits concurrent cell decommissioning

There is no `max_concurrent_tablet_cell_decommissions` setting. On every
`decommission_check_period`, the master scans all eligible retiring cells. The
amount of useful parallel drain work is nevertheless bounded by the following
gates:

1. **The shared move-action budget.** All retiring cells consume the same
   `decommission_throttler`; each tablet costs one unit when its move action is
   created. With many cells draining at once, they compete for this admission
   budget. The configured limit therefore controls aggregate action creation
   rate, not a guaranteed per-cell share or a fixed count of concurrent cells.
2. **Existing tablet actions.** If an unfinished action contains a mounted
   tablet from a retiring cell, or names the retiring cell as a target, the
   decommissioner postpones that cell. It does not create competing actions for
   those tablets. Large or stuck actions can consequently serialize progress
   for an entire cell.
3. **Bundle placement capacity.** Every tablet still needs a healthy
   destination in its bundle. Too few healthy cells, tablet slots, or node
   resources turns moved tablets into orphaned actions or makes mount phases
   slow. Orphan recovery is additionally paced by `kick_orphans_throttler`.
4. **Multicell convergence.** The primary master waits until all masters report
   `decommissioned` and the aggregate `@tablet_count` is zero before requesting
   node-side decommission. A lagging secondary therefore holds the cell at
   `decommissioning_on_master` even after local tablets appear drained.
5. **Node-side completion.** After the master-side gates pass, the tablet node
   must finish its own decommission protocol and acknowledge it. Bundle dynamic
   option `suppress_tablet_cell_decommission` deliberately prevents this
   completion and leaves the cell at `decommissioning_on_node`.
6. **Automatic removal.** `enable_tablet_cell_removal` controls cleanup after
   completion. Disabling it retains decommissioned objects, but does not by
   itself reduce the concurrency of earlier tablet moves.

When several cells make uneven progress, compare their remaining
`@tablet_count`, unfinished actions and action states, `@multicell_status`, and
peer/node health. Increasing `decommission_throttler` helps only when action
admission is the limiting gate; it can worsen flush, mount, network, or master
pressure when execution is already saturated.

#### Removing every cell in a bundle

Removing the last healthy cell is different from moving tablets between two
available cells. The decommissioner still creates destination-free `move`
actions. Each action freezes and unmounts its tablet, but at the assignment
stage it finds no healthy destination and changes to `orphaned`. The tablet is
therefore unmounted and temporarily unavailable; YTsaurus does not keep the
last cell running solely to preserve availability.

An orphaned action is pending recovery work, not a finished historical object.
Only `completed` and `failed` actions are eligible for expiration cleanup, so
an orphan remains visible in `//sys/tablet_actions` even though internally
created decommission actions normally have immediate expiration. Once a new
cell is healthy in the same bundle, the periodic orphan scan changes the action
back to `unmounted`; destination selection and mounting resume. After the
action reaches `completed`, it is normally removed immediately. The orphan
scan is independent of `enable_tablet_cell_decommission`, although its retry
rate is controlled by `orphans_check_period` and `kick_orphans_throttler`.

The orphan does not keep the old cell alive after its tablet is unmounted. The
decommissioner treats an unfinished action as blocking while its tablet is
still mounted in the retiring cell, or while the action explicitly names that
cell as a target. Once a destination-free action has unmounted the tablet, the
source cell can reach zero tablets and finish decommissioning while the action
waits for replacement capacity. Operationally, before removing every cell,
either accept this unavailability or create replacement healthy cells first.

#### Who removes finished tablet actions

The primary master's tablet action manager owns action cleanup. When an action
enters `completed` or `failed`, it is unbound from tablets and cells and no
longer counts as active. If its expiration time has already passed (the normal
case for an internally created decommission action), the manager immediately
unreferences it through the master object manager. Retained actions are found
by the periodic cleanup executor after `expiration_time` or
`expiration_timeout`; the executor commits a destroy-actions Hydra mutation,
which unbinds and unreferences each object. Consequently, an action that is
`completed` but intentionally has `keep_finished=%true` is observability data,
whereas an `orphaned` action is still live work and must not be deleted merely
to tidy Cypress.

### Corner cases to account for in design

- A committed transaction does not always imply immediate visibility of writes to all readers (depends on table type, whether the commit is local or distributed across multiple tablet cells, and read mode).
- Reads by old timestamps can fail if retention/TTL removed required versions.
- In-memory tables may temporarily reject reads after mount or tablet movement until preload finishes.
- Tablet movement/balancing can produce transient errors such as stale tablet routing; clients must retry idempotently.
- Forced unmount may lead to data loss and should only be used for emergency operations.

### Performance analysis workflow

When performance degrades, use this order:
1. **Confirm query shape**: ensure key-prefix filtering is used and full scans are not accidental.
2. **Check tablet structure**: store overlap/count and partition statistics.
3. **Check memory pressure**: tablet memory, lookup cache, and in-memory preload behavior.
4. **Check storage/network cost**: disk reads, cache hit rates, transmitted bytes.
5. **Apply a targeted change**: resharding, retention tuning, cache/filter tuning, compaction intervention, then re-measure.

For detailed knobs and metrics, see:
- [Sorted dynamic tables](../user-guide/dynamic-tables/sorted-dynamic-tables.md)
- [Ordered dynamic tables](../user-guide/dynamic-tables/ordered-dynamic-tables.md)
- [Compaction](../user-guide/dynamic-tables/compaction.md)
- [Tablet balancing](../user-guide/dynamic-tables/tablet-balancing.md)
- [FAQ](../user-guide/dynamic-tables/faq.md)
