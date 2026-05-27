<!--
Draft number: 9
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: user-guide/dynamic-tables
-->

# Dynamic tables (sorted) — operational corner cases and administration notes

## Operational corner cases and administration notes { #ops_corner_cases }

### Consistency and timestamp pitfalls

- `sync_last_committed` and `async_last_committed` are useful for freshness, but they do not provide a globally consistent snapshot across all rows/tables. Use an explicit timestamp for that.
- Read APIs and SQL queries in a transaction operate on a snapshot taken at transaction start, so they do not read writes performed by the same transaction.
- Reads by explicit old timestamp may fail when the requested history has already been removed by TTL/retention (`retained timestamp` class of errors).

### Write-path and lock-related pitfalls

- `atomicity=none` removes row-level conflict protection. This can significantly increase write throughput but may create very high version counts for hot keys and non-deterministic final values for concurrent writes to the same key.
- Frequent updates of the same key can cause row-version explosion. Plan retention/compaction settings and schema (including aggregation columns) for this pattern from the beginning.
- Forced unmount is an emergency tool. It may cause data loss and two-phase commit side effects.

### Performance troubleshooting flow

1. Confirm the query plan and key-prefix filtering first (avoid accidental full scans).
2. Check tablet structure (`overlapping_store_count`, store count, partition count) and whether background compaction/partitioning keeps up.
3. Investigate lookup path costs:
   - Disk and cache bytes read.
   - Network bytes transferred.
   - Lookup cache hit/miss/outdated rates.
4. If many lookups are for missing keys, enable and tune key filter.
5. If read latency is still dominated by disk/network, evaluate `in_memory_mode`, row cache size, and table format (`lookup` vs `scan`) for the actual workload.

### Minimal incident checklist

For recurring write failures (`out of tablet memory`, `active store is overflown`, `too many stores/overlaps`):
1. Verify table/tablet errors (`@tablet_errors`) and memory pressure.
2. Reshard to increase parallelism where needed.
3. Recheck balancing and bundle resource limits.
4. Only then use forced compaction or other heavy interventions.
