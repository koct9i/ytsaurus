---
type: Draft Article
title: "Draft-7: Dynamic tables capacity planning and scaling"
last_modified: 2026-05-27T00:00:00Z
tags: [dynamic-tables, capacity-planning, scaling]
status: draft
---

# Dynamic tables capacity planning and scaling

This article describes how to plan capacity for dynamic tables and how to scale them without creating new bottlenecks. The focus is on administration, background-process headroom, and predictable growth of read and write throughput.

For the data model and storage internals, see [Dynamic tables](../user-guide/dynamic-tables/overview.md), [Automatic sharding and dynamic table balancing](../user-guide/dynamic-tables/tablet-balancing.md), and [Background compaction](../user-guide/dynamic-tables/compaction.md).

## What exactly scales

Dynamic tables do not scale by one universal knob. Different problems require different actions:

- **Write throughput** scales with better key distribution, more tablets, and more bundle resources for flush.
- **Read throughput** scales with better query shape, enough tablets for parallelism, and enough memory and cache efficiency.
- **Queue throughput** scales by sharding ordered tables and by spreading producers and consumers across tablets.
- **Data volume** scales only if the bundle also has headroom for compaction, partitioning, preload, and balancing.

A tablet is not only a logical shard, but also a unit of background work. If you increase traffic without keeping enough room for flush and compaction, performance degrades long before physical storage is exhausted.

## Initial sizing checklist

Before opening production traffic on a new table:

1. Choose the correct table type:
   - [Sorted tables](../user-guide/dynamic-tables/sorted-dynamic-tables.md) for key-value and range workloads.
   - [Ordered tables](../user-guide/dynamic-tables/ordered-dynamic-tables.md) for queue-like append workloads.
2. Estimate the expected write rate, read rate, working-set size, and retention period.
3. Choose an initial tablet count that spreads the hottest traffic, not only the total data size.
4. Configure balancing and resharding limits for the expected growth.
5. Leave headroom for background work and remount/preload operations.

If the workload is bursty, size for the burst, not for the average.

## Dynamic table memory management

Tablet-node RAM is not a single interchangeable pool. It is divided between dynamic
stores, in-memory tables, read caches, versioned chunk metadata, the lookup row
cache, queries, and memory reserved for the process itself. Capacity planning must
therefore set both the amount of RAM available to a tablet-node instance and the
limits of the consumers inside that amount. Adding RAM to an instance without
changing these limits does not give the corresponding consumer more memory.

Choose one owner for these limits:

- For a bundle managed by the Bundle controller, configure
  `bundle_controller_target_config.memory_limits`. The controller propagates the
  limits to the assigned tablet nodes.
- Without the Bundle controller, configure the tablet nodes themselves and keep
  the configuration identical on every node that can host the bundle.

Do not configure both mechanisms independently. Otherwise a controller update can
replace a manually selected limit.

### Defaults that must not be used as production sizing

Several built-in defaults only make a node start successfully; they are not a
memory allocation policy:

- The cluster-node `resource_limits.total_memory` default is **5 GB** and is
  intentionally very low. Set it to the memory actually available to the process.
- `tablet_node.resource_limits.tablet_static_memory` and
  `tablet_node.resource_limits.tablet_dynamic_memory` both default to the maximum
  64-bit integer. Set finite limits when the Bundle controller does not manage the
  node.
- `tablet_node.versioned_chunk_meta_cache.capacity` defaults to **10 GB**. This is
  an upper bound for the cache, not an instruction to reserve 10 GB of physical
  memory. Nevertheless, leaving it unchanged on a smaller node permits the cache
  to compete with dynamic stores and the process reserve, so set it explicitly.
- The per-bundle memory categories (`tablet_dynamic`, `tablet_static`, block
  caches, `versioned_chunk_meta`, and `lookup_row_cache`) are optional in the
  low-level configuration. An omitted category is therefore not a safe production
  default; give each enabled consumer an explicit budget.

### Bundle Controller configuration

For managed bundles, the following options must be set:

1. `tablet_node_resource_guarantee.memory`: physical RAM assigned to each tablet
   node instance.
2. `bundle_controller_target_config.memory_limits`: the per-node distribution of
   that RAM.
3. The bundle's `resource_limits.memory`: the total memory quota for all instances
   assigned to the bundle.

Set at least these memory categories in `memory_limits` (all values are bytes):

| Option | Purpose | Sizing guidance |
| --- | --- | --- |
| `tablet_dynamic` | Active and passive dynamic stores before flush. | Size from sustained and burst write rates and measured flush latency. This is the main write buffer. |
| `tablet_static` | Preloaded data for tables whose `in_memory_mode` is not `none`. | Use zero if there are no in-memory tables; otherwise cover the resident data plus relocation/preload headroom. |
| `compressed_block_cache` | Compressed blocks used by reads. | Prefer it when RAM efficiency matters. |
| `uncompressed_block_cache` | Decoded blocks used by reads. | Allocate it only when avoiding decompression is worth the larger footprint. |
| `versioned_chunk_meta` | Accounted memory for versioned chunk metadata. | Keep it consistent with `tablet_node.versioned_chunk_meta_cache.capacity`; see below. |
| `lookup_row_cache` | Row-level lookup cache. | Use zero unless tables enable and benefit from the lookup row cache. |
| `reserved` | Tablet-node process overhead and memory not covered by the categories above. | Always leave a non-zero reserve; do not distribute all instance RAM to caches and stores. |

The sum of the categories must not exceed
`tablet_node_resource_guarantee.memory`. It should normally be smaller, leaving a
margin for the operating system and transient allocations. If the zone supplies a
`default_config.memory_limits`, treat those values only as defaults for new
bundles and override them for the workload.

Example target configuration for a node with 64 GiB of RAM (illustrative rather
than universal):

```yson
{
    tablet_node_resource_guarantee = {
        memory = 68719476736;
        vcpu = 16000;
        net_bytes = 0;
    };
    memory_limits = {
        tablet_dynamic = 21474836480;
        tablet_static = 0;
        compressed_block_cache = 10737418240;
        uncompressed_block_cache = 4294967296;
        versioned_chunk_meta = 8589934592;
        lookup_row_cache = 0;
        reserved = 12884901888;
    };
}
```

Also set `//sys/tablet_cell_bundles/<bundle>/@resource_limits/memory` high enough
for the requested node count. See [Bundle controller](../admin-guide/bundle-controller.md)
for the complete target configuration, including CPU limits and node count.

### `versioned_chunk_meta_cache`

Sorted-table reads need parsed metadata for versioned chunks. The tablet node keeps
it in an SLRU cache configured at:

```yson
tablet_node = {
    versioned_chunk_meta_cache = {
        capacity = 8589934592;
    };
};
```

`capacity` is in bytes and can be changed through tablet-node dynamic
configuration. For a Bundle-controller-managed node, use the same value as the
bundle's `memory_limits.versioned_chunk_meta`. The former controls cache eviction;
the latter is the memory-accounting limit. Setting only one of them produces a
misleading budget: a smaller cache wastes the bundle allocation, while a larger
cache is constrained by memory accounting and can cause repeated eviction or
memory-pressure errors.

Increase the cache only when its hit/miss and weight metrics show that metadata is
being repeatedly reloaded and the node has reserve. Do not increase it merely
because a table contains many chunks: first reduce excessive chunk counts with
compaction. After changing it, inspect
`/orchid/tablet_node/effective_config/versioned_chunk_meta_cache/capacity` on each
tablet node to confirm that the dynamic configuration was applied.

### Operating without the Bundle controller

For manually managed nodes, explicitly set:

- `resource_limits.total_memory` in the cluster-node configuration;
- finite `tablet_node.resource_limits.tablet_dynamic_memory` and
  `tablet_node.resource_limits.tablet_static_memory` values;
- capacities for `tablet_node.versioned_chunk_meta_cache` and the compressed and
  uncompressed block caches used by the node;
- a process/OS reserve outside those limits.

Apply the same policy to every eligible tablet node. A bundle can move tablets to
any of them, so one node left at the built-in defaults makes bundle behavior depend
on placement. Roll out one category at a time, verify the effective configuration,
and watch memory pressure and flush latency before increasing traffic.

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
yt unmount-table //path/to/table --sync
yt reshard-table //path/to/table --tablet-count 64 --sync
yt mount-table //path/to/table --sync
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

- [Draft-6: Dynamic tables — performance profiling and bottleneck analysis](./dynamic-tables-profiling-draft-6.md)
- [Automatic sharding and dynamic table balancing](../user-guide/dynamic-tables/tablet-balancing.md)
- [Background compaction](../user-guide/dynamic-tables/compaction.md)
- [Ordered tables](../user-guide/dynamic-tables/ordered-dynamic-tables.md)
- [Sorted tables](../user-guide/dynamic-tables/sorted-dynamic-tables.md)
