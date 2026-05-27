<!--
Draft number: 10
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: user-guide/dynamic-tables
-->

# Dynamic tables (ordered) — operational corner cases and administration notes

## Operational corner cases and administration notes { #ops_corner_cases }

### Visibility and ordering corner cases

- For transactions involving multiple tablet cells, a successful commit response from the coordinator does not guarantee immediate row visibility in ordered tables.
- In `weak` `commit_ordering`, append order may differ from commit timestamp order.
- In `strong` `commit_ordering`, timestamp order is preserved within each tablet, but visibility may be delayed because rows become visible only after corresponding serialization progress (`barrier-ts` logic).

If consumers require deterministic replay order across producers, do not rely on `$timestamp` and `commit_ordering=strong` alone when reading from multiple tablets. Ordered dynamic tables do not provide a single global order across tablets, so deterministic replay requires either a single-tablet table or an explicit merge strategy by `$timestamp` together with tablet position (`$tablet_index`, `$row_index`). In this setup, `commit_ordering=strong` helps preserve timestamp order within each tablet, and SLAs should account for the additional visibility lag caused by `barrier-ts`.

### Trim, reshard, and conversion pitfalls

- `trim_rows` uses absolute `trimmed_row_count`, not incremental row count.
- Combining trim with resharding is prohibited.
- Reducing tablet count after deletions can be impossible in practice if deleted prefixes are not aligned as required.
- Converting through static tables may reintroduce previously trimmed rows.

Design queue retention and tablet topology with these constraints in mind to avoid emergency migrations later.

### Performance-analysis checklist

1. Check write distribution across tablets (`$tablet_index` usage and tablet-level load skew).
2. Track tablet memory pressure and flush lag indicators when append rate grows.
3. Check chunk growth and compaction progress if read-by-index latency or storage size degrades.
4. Validate retention settings (`min_data_versions`, TTLs) against expected queue depth and replay windows.
5. For overload events, scale by tablet count/bundle resources first; only then apply heavy maintenance actions.

### Operational playbook for queue-like workloads

1. Explicitly decide whether per-tablet ordering is sufficient; if consumers need deterministic replay across producers, use a single tablet or merge rows by `$timestamp` and tablet position.
2. Set and test trim policy together with consumer checkpoint/replay logic.
3. Use remount and staged rollout for mount-config changes.
4. Treat force unmount as emergency-only and plan client retry/idempotency behavior for transient routing or cell-move errors.
