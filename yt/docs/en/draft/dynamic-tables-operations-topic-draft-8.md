<!--
Draft number: 8
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: user-guide/dynamic-tables
-->

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
