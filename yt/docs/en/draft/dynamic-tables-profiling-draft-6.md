<!--
Draft number: 6
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: user-guide/dynamic-tables
-->

# Performance profiling and bottleneck analysis

This article describes how to investigate slow or unstable dynamic tables in production. It covers practical operational topics such as tablet layout, flush, compaction, queue semantics, and tablet-cell execution.

For background, see [Dynamic tables](../user-guide/dynamic-tables/overview.md), [Automatic sharding and dynamic table balancing](../user-guide/dynamic-tables/tablet-balancing.md), and [Background compaction](../user-guide/dynamic-tables/compaction.md).

## What to check first

Start with the simplest classification:

- **Writes are slow or rejected**: investigate dynamic-store pressure, flush progress, tablet skew, or insufficient bundle capacity.
- **Reads are slow**: investigate query shape, key distribution, chunk overlap, cache efficiency, or preload status for in-memory tables.
- **Latency is unstable**: investigate balancing, tablet-cell movement, overloaded nodes, or background processes competing with foreground traffic.
- **Queue lag is growing**: investigate ordered-table write rate, consumer parallelism, and whether one or several tablets receive most of the traffic.

Before changing configuration, collect the current state:

```bash
yt get //path/to/table/@tablet_state
yt get //path/to/table/@tablet_errors
yt get //path/to/table/@tablets
yt get //path/to/table/@mount_config
yt get //sys/tablet_cell_bundles/<bundle_name>/@tablet_balancer_config
```

Use dashboards from the [Monitoring](../admin-guide/monitoring.md) section together with table attributes. Table attributes show the local symptom. Dashboards show whether the entire bundle is saturated.

## Typical bottlenecks

### Query shape is worse than expected

Dynamic tables scale well only when requests are aligned with the key layout.

- For **sorted tables**, use reads by key or by a key-prefix range.
- For **ordered tables**, keep in mind that each tablet is an independent append-only partition.
- Avoid accidental full scans when the workload assumes point lookups.

If the query shape is wrong, adding nodes often does not help. First fix the access pattern or the schema.

### Hot tablets and uneven key distribution

One overloaded tablet can dominate tail latency even when the bundle looks underutilized.

Typical signs:

- Only a small subset of tablets receives most writes or reads.
- One tablet accumulates errors while others are healthy.
- Resharding or balancing temporarily improves the situation.

Actions:

1. Inspect `@tablets` and identify skew in row count, data weight, or traffic.
2. Check whether the key design sends recent traffic to a narrow key range.
3. Increase tablet count or choose better pivot keys.
4. Verify that automatic balancing is enabled where appropriate.

For queue-like workloads, the most common reason is insufficient sharding of producers and consumers. Ordered tables scale by adding tablets and spreading traffic across them.

## Flush cannot keep up with writes

Writes first enter the in-memory dynamic store. If passive stores do not flush to chunks fast enough, memory usage grows and writes start failing.

Common symptoms:

- `Node is out of tablet memory, all writes disabled`
- `Active store is overflown, all writes disabled`
- Long periods with growing in-memory data and no reduction

Actions:

1. Check `@tablet_errors`.
2. Verify that the table has enough tablets for the current write rate.
3. Check whether the bundle has enough CPU, disk bandwidth, and memory for flush traffic.
4. Review mount settings only after confirming the problem is structural rather than transient.

If the whole bundle is saturated, the right fix is usually adding capacity, not forcing flushes more often.

## Compaction and partitioning cannot keep up

For sorted tables, background compaction is responsible for removing overlap, cleaning old versions, and keeping reads efficient. If it falls behind, both reads and writes degrade.

Common symptoms:

- `Too many overlapping stores`
- `Too many stores in tablet, all writes disabled`
- Read latency grows over time while write rate stays roughly constant

Actions:

1. Check whether compaction is enabled.
2. Review table retention settings and write pattern.
3. Make sure tablets are not too large and tablet count is high enough.
4. Use periodic or forced compaction only as an explicit operational action.

For details and safe tuning limits, see [Background compaction](../user-guide/dynamic-tables/compaction.md).

## In-memory tables are not actually ready

In-memory mode reduces read latency, but only after preload completes on the current tablet nodes.

Typical symptoms:

- `Chunk data is not preloaded yet`
- Read latency spikes after mount, remount, restart, or tablet movement

Actions:

1. Check whether the table is mounted in an in-memory mode.
2. Confirm that preload has completed after the latest movement or restart.
3. Keep memory headroom in the bundle. In-memory mode turns RAM into a hard capacity limit.

## Queue-specific consistency and latency notes

Ordered tables are often used as queues. In this mode, performance analysis must separate two questions:

- **Can the system append rows fast enough?**
- **Can consumers observe and process rows fast enough?**

Ordered tables are append-only, and commit ordering may be weak or strong. Stronger ordering improves semantics, but it may reduce throughput and add sensitivity to internal delays. Use it only when the workload requires the guarantee.

If queue lag grows:

1. Check producer sharding.
2. Check consumer parallelism by tablet.
3. Check whether writes are blocked by flush or memory pressure.
4. Check whether balancing or tablet movement causes transient retries.

## Practical investigation sequence

Use the following order during an incident:

1. **Confirm the workload**: sorted table or ordered table, read-heavy or write-heavy, point lookup or scan-like access.
2. **Check errors and tablet state**: `@tablet_errors`, `@tablet_state`, mount status.
3. **Inspect tablet distribution**: look for skew and insufficient tablet count.
4. **Inspect background processes**: flush, compaction, partitioning.
5. **Inspect bundle saturation**: CPU, memory, disk, and network from dashboards.
6. **Apply one targeted change**: reshard, rebalance, add nodes, or adjust retention/tuning.
7. **Measure again** before making the next change.

## Examples of focused actions

### Identify whether the issue is local to one table

```bash
yt get //path/to/table/@tablet_errors
yt get //path/to/table/@tablets
```

If a single table is unhealthy while the bundle is otherwise stable, start with schema, sharding, and mount settings for that table.

### Inspect bundle balancing configuration

```bash
yt get //sys/tablet_cell_bundles/<bundle_name>/@tablet_balancer_config
```

If the load is uneven, verify that the current balancing policy matches the workload.

### Verify effective runtime settings

```bash
yt get //sys/tablets/<tablet_id>/orchid/config
```

Use this when a table was remounted recently or when you need to confirm that a mount-config change really reached the tablet.

## When to scale instead of tune

Tune configuration only after you identify the bottleneck class. If multiple healthy tables in the same bundle are simultaneously hitting memory, disk, or CPU limits, the bundle is out of headroom. In that case, the correct action is capacity scaling.

For sizing guidance, see [Draft-7: Dynamic tables — capacity planning and scaling](./dynamic-tables-capacity-scaling-draft-7.md).
