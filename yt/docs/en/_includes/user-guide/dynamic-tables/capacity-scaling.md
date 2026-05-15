# Capacity planning and scaling

This article describes how to plan capacity for dynamic tables and how to scale them without creating new bottlenecks. The focus is on administration, background-process headroom, and predictable growth of read and write throughput.

For the data model and storage internals, see [Dynamic tables](../../../user-guide/dynamic-tables/overview.md), [Automatic sharding and dynamic table balancing](../../../user-guide/dynamic-tables/tablet-balancing.md), and [Background compaction](../../../user-guide/dynamic-tables/compaction.md).

## What exactly scales

Dynamic tables do not scale by one universal knob. Different problems require different actions:

- **Write throughput** scales with better key distribution, more tablets, and more bundle resources for flush.
- **Read throughput** scales with better query shape, enough tablets for parallelism, and enough memory and cache efficiency.
- **Queue throughput** scales by sharding ordered tables and by spreading producers and consumers across tablets.
- **Data volume** scales only if the bundle also has headroom for compaction, partitioning, preload, and balancing.

The raw meeting logs highlight a useful mental model: a tablet is not only a logical shard, but also a unit of background work. If you increase traffic without keeping enough room for flush and compaction, performance degrades long before physical storage is exhausted.

## Initial sizing checklist

Before opening production traffic on a new table:

1. Choose the correct table type:
   - [Sorted tables](../../../user-guide/dynamic-tables/sorted-dynamic-tables.md) for key-value and range workloads.
   - [Ordered tables](../../../user-guide/dynamic-tables/ordered-dynamic-tables.md) for queue-like append workloads.
2. Estimate the expected write rate, read rate, working-set size, and retention period.
3. Choose an initial tablet count that spreads the hottest traffic, not only the total data size.
4. Configure balancing and resharding limits for the expected growth.
5. Leave headroom for background work and remount/preload operations.

If the workload is bursty, size for the burst, not for the average.

## Scaling writes

Write scaling is usually limited by one of three things:

- Too few tablets receive most writes.
- Flush cannot turn dynamic stores into chunks fast enough.
- Compaction later becomes too expensive because the table layout is unhealthy.

Use the following rules:

### Distribute keys before adding hardware

If new writes fall into a narrow key range, one or several tablets become hot. In that case:

- change the key design;
- add more tablets;
- or reshard using better pivot keys.

Adding nodes without fixing key distribution often just creates more idle nodes.

### Keep enough flush headroom

Writes first live in memory. The bundle must have enough CPU, memory, and disk throughput to flush passive stores in time.

If you frequently observe memory-pressure errors, do not immediately increase flush aggressiveness. First answer the more important question: is the write rate sustainable for the current bundle?

### Do not forget write amplification

For sorted tables, higher write throughput also means more work for compaction and partitioning. If you push write traffic to the limit with no reserve, read performance usually degrades later.

When increasing write traffic, re-check:

- tablet count;
- overlap-related alerts;
- retention and TTL settings;
- balance between write cost now and compaction cost later.

## Scaling reads

Read scaling begins with query analysis, not with hardware.

### Sorted tables

For sorted tables:

- prefer lookups by key or by key prefix;
- verify that the query avoids accidental full scans;
- choose `optimize_for` according to the dominant access pattern.

If the workload is lookup-heavy and the data fits in memory, in-memory mode can reduce latency. But it also reduces effective memory headroom for the rest of the bundle.

### Ordered tables and queues

For ordered tables:

- increase the number of tablets if readers or writers are concentrated on too few partitions;
- spread consumers across tablets;
- treat queue lag as a throughput symptom, not only as a consumer issue.

If strict ordering is enabled for semantic reasons, include its cost in the throughput budget.

## Scaling by tablet count

Tablet count is the main operational knob for horizontal scaling.

Too few tablets lead to hot shards and poor parallelism. Too many tablets increase metadata overhead, balancing work, and operational complexity. The right value depends on traffic distribution, not on one universal formula.

Use the following approach:

1. Start from the hottest expected traffic path.
2. Choose the minimum number of tablets that spreads that traffic safely.
3. Verify that the bundle balancer can move and reshard tablets with enough freedom.
4. Revisit the tablet count after major changes in traffic shape.

Example:

```bash
yt reshard-table //path/to/table --tablet-count 64
```

If the table is already under pressure, resharding is often more effective than changing low-level mount parameters.

## Scaling the bundle

Bundle scaling is needed when multiple healthy tables are simultaneously close to the same resource limit.

Typical signals:

- several tables in the same bundle show memory-pressure symptoms;
- flush and compaction fall behind across the bundle, not only for one table;
- balancing keeps moving tablets, but no placement removes the saturation.

In that case, review:

- number of tablet cells;
- number of tablet nodes in the bundle;
- RAM headroom for dynamic stores and in-memory tables;
- disk and network headroom for flush, compaction, and preload.

Use balancing configuration as a placement tool, not as a substitute for missing hardware.

## Headroom for background work

Capacity planning for dynamic tables must reserve room for:

- flush;
- compaction and partitioning;
- remounts;
- tablet movement and balancing;
- in-memory preload after restarts or relocations.

Ignoring this reserve creates a common failure mode: average traffic seems acceptable, but maintenance actions or normal background work cause long latency spikes.

As a practical rule, leave enough unused capacity so that background work can continue while foreground traffic remains within SLO.

## Choosing between tuning and scaling

Choose **tuning** when:

- one table is misconfigured;
- the schema or key layout is the real cause;
- the bundle as a whole is healthy.

Choose **scaling** when:

- several tables show the same saturation pattern;
- the same bundle resource is exhausted across many tablets;
- retries, balancing, and remounts make the bundle unstable under normal traffic.

## Examples

### Check balancing settings before increasing tablet count

```bash
yt get //sys/tablet_cell_bundles/<bundle_name>/@tablet_balancer_config
```

If balancing is disabled or too constrained, new tablets may not spread as expected.

### Inspect current tablet layout

```bash
yt get //path/to/table/@tablets
```

Use this to check whether current tablets already show obvious skew before you add more of them.

## Related topics

- [Performance profiling and bottleneck analysis](../../../user-guide/dynamic-tables/profiling.md)
- [Automatic sharding and dynamic table balancing](../../../user-guide/dynamic-tables/tablet-balancing.md)
- [Background compaction](../../../user-guide/dynamic-tables/compaction.md)
- [Ordered tables](../../../user-guide/dynamic-tables/ordered-dynamic-tables.md)
- [Sorted tables](../../../user-guide/dynamic-tables/sorted-dynamic-tables.md)
