<!--
Draft number: 8
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: user-guide/dynamic-tables
-->

# Dynamic tables — operating in production (topic draft)

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
