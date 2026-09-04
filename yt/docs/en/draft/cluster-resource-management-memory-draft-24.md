---
type: Draft Article
title: "Draft-24: Cluster node and proxy memory management"
last_modified: 2026-09-04T00:00:00Z
tags: [administration, cluster-node, proxy, memory]
status: draft
---

# Cluster node and proxy memory management

{% note warning "Draft" %}

This article describes the current memory tracker and the configuration model used by cluster nodes, HTTP proxies, and RPC proxies. It is an operator-oriented draft: verify defaults and option availability against the version deployed in your cluster before changing production limits.

{% endnote %}

## Scope and mental model

Memory management has two distinct parts:

1. **Accounting** assigns tracked allocations to a memory category and, where applicable, to a pool such as a tablet-cell bundle or an authenticated user.
2. **Admission and reaction** compare accounted usage with the effective total, category, and pool limits. A component can reject an allocation or request, defer work, evict cache entries, throttle activity, or raise an alert depending on the call site.

A limit is therefore not an operating-system hard limit. The process may contain temporarily overcommitted allocations, untracked memory, allocator fragmentation, memory-mapped pages, and external memory such as user jobs. Keep headroom between the tracker limit and the container or host limit; otherwise the kernel or container runtime can terminate the process before YTsaurus can react.

The effective free memory for an allocation is the minimum of all applicable scopes:

```text
total process/node budget
  └── category budget (for example, block_cache or heavy_request)
        └── optional pool budget (for example, a bundle or user)
```

In other words, increasing a category limit cannot make more memory available after the total limit is exhausted. Likewise, a request tagged with a pool must fit both the category-wide and pool-specific budgets. Not every component or category uses pool limits.

## Memory categories

Categories are shared by the node and proxy memory-tracker implementation, but each daemon normally uses only a subset. Names in configuration and monitoring are the snake-case forms shown below.

| Group | Categories | What they account for |
|---|---|---|
| Process overhead | `footprint`, `alloc_fragmentation`, `profiling`, `logging`, `unknown`, `mixed`, `huge_page` | Unattributed allocator usage, allocator slack, profiling/logging data, allocations with no single category, and huge pages. `footprint` and `alloc_fragmentation` are periodically reconciled with allocator statistics. |
| Caches and metadata | `block_cache`, `chunk_meta`, `chunk_block_meta`, `chunk_blocks_ext`, `chunk_journal_index`, `versioned_chunk_meta`, `master_cache`, `lookup_rows_cache`, `job_input_block_cache`, `job_input_chunk_meta_cache`, `chunk_replica_cache` | Cached blocks and the various metadata/index structures used to locate and interpret chunks. |
| RPC and queries | `rpc`, `heavy_request`, `query`, `lookup`, `read_table`, `fetch_table_rows` | Transport buffers and request/query execution. Proxies principally constrain `rpc` and/or `heavy_request`. |
| Tablet workloads | `tablet_static`, `tablet_dynamic`, `tablet_background`, `tablet_footprint`, `tablet_row_merger` | In-memory stores, preload/static tablet data, tablet metadata, and background processing. Tablet categories may additionally be split by bundle. |
| Jobs | `user_jobs`, `system_jobs`, `tmpfs_layers` | Memory assigned to user and system jobs and job tmpfs layers. User-job memory is external to the node daemon heap but participates in the node budget. |
| I/O and replication | `pending_disk_read`, `pending_disk_write`, `p2p`, `table_replication`, `chaos_replication_incoming`, `chaos_replication_outgoing` | In-flight disk I/O, peer-to-peer distribution, and replication buffers. |
| Compatibility/sentinel | `blob_session`, `unrecognized` | `blob_session` is a legacy alias migrated to `pending_disk_write`; `unrecognized` preserves unknown enum values. Do not use either for new configuration. |

Category accounting is not identical to RSS:

- `footprint` is the allocator's currently allocated bytes minus explicitly tracked in-process categories; it is clamped at zero;
- `alloc_fragmentation` is the nonnegative difference between allocator heap size and allocated bytes;
- `profiling` and `logging` are refreshed from their dedicated trackers;
- `user_jobs` and `tmpfs_layers` describe memory outside ordinary node heap allocations;
- shared buffers can be retagged, and buffers attributed to several meaningful categories are reported as `mixed`.

Consequently, use category counters to explain and control consumption, but use process/container RSS and the container memory limit to assess OOM risk.

## Cluster-node limits

### Static configuration

The static node configuration establishes the baseline under `resource_limits`:

```yson
resource_limits = {
    total_memory = 120000000000;
    free_memory_watermark = 8000000000;
    memory_limits = {
        user_jobs = {type = dynamic;};
        tablet_static = {type = static; value = 24000000000;};
        tablet_dynamic = {type = static; value = 32000000000;};
        block_cache = {type = static; value = 12000000000;};
    };
};
```

Byte counts are used in these examples. The effective total tracker limit is:

```text
effective total = detected/configured instance memory - free_memory_watermark
```

`total_memory` is the static fallback, but an instance-limit tracker can replace the current total with the container/runtime limit. Normally, `free_memory_watermark` is subtracted before the total is passed to the memory tracker. If the watermark is greater than the detected total, the node logs a warning and does not subtract it.

Each entry in `memory_limits` has one of three types:

- `static`: use the explicit `value` as the category ceiling; `value` is mandatory, and the value is deducted when calculating the dynamic remainder;
- `dynamic`: take an equal share of the memory left after the watermark and all non-dynamic category amounts;
- `none`: do not actively set a limit for this category. For dynamic-budget calculation, its existing explicit tracker limit is used, or current usage if no explicit limit exists.

Do not specify `value` for `dynamic` or `none`. If several categories are `dynamic`, they split the remainder equally; the split is not proportional to current demand. If fixed and observed amounts consume the entire budget, every dynamic category receives zero.

For compatibility, the top-level `resource_limits.user_jobs`, `tablet_static`, and `tablet_dynamic` fields are accepted and overwrite entries with the same names in `memory_limits`. New configurations should use the map. If omitted, compatibility defaults currently make `user_jobs` dynamic, derive tablet limits from legacy tablet-node settings, and set `lookup_rows_cache` to a static zero limit.

### Dynamic configuration and precedence

The node dynamic configuration has a matching `resource_limits.memory_limits` map and an optional `free_memory_watermark`:

```yson
resource_limits = {
    free_memory_watermark = 12000000000;
    memory_limits = {
        user_jobs = {type = static; value = 48000000000;};
        block_cache = {type = static; value = 8000000000;};
    };
};
```

For each category independently, the effective source is, from highest to lowest priority:

1. bundle dynamic memory limit, when the bundle configuration supplies one for `tablet_static`, `tablet_dynamic`, `lookup_rows_cache`, or `query`;
2. cluster-node dynamic `resource_limits.memory_limits[category]`;
3. cluster-node static `resource_limits.memory_limits[category]`;
4. the `none` behavior when no usable entry exists.

Bundle dynamic configuration uses plain byte values, which become `static` category limits. The bundle keys are `tablet_static`, `tablet_dynamic`, `lookup_row_cache`, and `query`; note that `lookup_row_cache` is singular in the bundle configuration but maps to the `lookup_rows_cache` tracker category. Cache-capacity settings such as `compressed_block_cache` also exist in the bundle configuration, but they reconfigure the corresponding caches instead of adding category entries to this precedence chain.

This per-category fallback matters: providing one dynamic entry does not erase all static entries. The dynamic `free_memory_watermark`, when present, replaces the static watermark.

Resource-limit overrides are applied after the category calculation. In particular, an externally supplied node override has priority over dynamic-config `resource_limits.overrides`; `system_memory` and `user_memory` replace the calculated `system_jobs` and `user_jobs` values respectively.

The node recalculates limits periodically (`resource_limits_update_period`, one second by default). Changes smaller than `memory_accounting_tolerance` are not applied to category trackers. Reducing a limit below current usage does not reclaim all memory synchronously: it makes the scope exceeded and lets the owning subsystem react.

### Worked node example

Assume 128 GB of detected instance memory, an 8 GB watermark, 20 GB of static categories, 4 GB accounted for categories with type `none`, and two dynamic categories:

```text
tracker total       = 128 - 8 = 120 GB
dynamic remainder  = 120 - 20 - 4 = 96 GB
each dynamic limit = 96 / 2 = 48 GB
```

These category limits are not separate physical reservations: total usage across all categories still cannot exceed 120 GB. The calculation can also be conservative because a `none` category's current usage is subtracted from the dynamic remainder.

## Proxy limits

HTTP and RPC proxies use the same tracker but expose different configuration models.

### RPC proxy

RPC proxy static and dynamic configurations both contain `memory_limits` with `total`, `heavy_request`, and `rpc`:

```yson
memory_limits = {
    total = 20000000000;
    heavy_request = 12000000000;
    rpc = 8000000000;
};
```

The static `total` defaults to 20 GB. A present dynamic `total` replaces it. For `heavy_request` and `rpc`, a dynamic value wins, then a static value, then the effective total is used. Category limits remain subordinate to the total limit, so configuring both categories up to the total does not double the process budget.

### HTTP proxy

HTTP proxy static and dynamic configurations expose `memory_limits.total`. Dynamic `total` replaces static `total`; if both are absent, the base is effectively unlimited. The API configuration converts that base into limits by ratio:

```yson
memory_limits = {total = 20000000000;};
api = {
    default_memory_limit_ratios = {
        total_memory_limit_ratio = 0.90;
        heavy_request_memory_limit_ratio = 0.75;
        default_user_memory_limit_ratio = 0.20;
        user_to_memory_limit_ratio = {
            analytics = 0.35;
        };
    };
};
```

The tracker total is `base × total_memory_limit_ratio`, while the `heavy_request` category limit is `base × heavy_request_memory_limit_ratio`. Ratios are in `[0, 1]`. If `role_to_memory_limit_ratios` has an entry for the proxy's role, that whole ratio object is selected instead of `default_memory_limit_ratios` for the total and heavy-request calculations; it is not a field-by-field patch. Missing fields in that object receive their schema defaults.

User-pool selection is slightly different. The proxy first reads the default ratio object and then applies a matching role object's `default_user_memory_limit_ratio` and `user_to_memory_limit_ratio` entries. A user-specific value wins over the default-user value at each layer. Thus an individual heavy request must fit the tracker total, the heavy-request category, and its user's pool.

### Tracker behavior shared by nodes and proxies

Dynamic tracker settings tune the common implementation. The field is named `node_memory_tracker` in cluster-node dynamic configuration and `memory_tracker` in HTTP/RPC proxy dynamic configuration:

```yson
node_memory_tracker = {
    check_per_category_limit_overcommit = false;
    system_categories_update_period = 1s;
};
```

For a proxy, use the same map under the `memory_tracker` key. `system_categories_update_period` controls reconciliation of system categories. For unconditional `Acquire` calls, `check_per_category_limit_overcommit` makes the tracker report a category overcommit (return `false` and log a warning) after accounting the allocation, even when total memory remains; it does not roll the allocation back. Conditional `TryAcquire` calls check the applicable total, category, and pool limits before accounting memory regardless of this option. Leave the option at the version's default unless callers are prepared to react to the overcommit result. On a cluster node, the option is forced off unless the job-resource manager is also configured to check the user-jobs category during resource updates.

## Observing and changing limits safely

Before a change, record all of the following at the same time:

1. container/host memory limit and process RSS;
2. tracker total used and total limit;
3. usage, explicit limit, and effective limit by category;
4. pool usage and limits for bundles or proxy users;
5. allocator fragmentation and footprint;
6. rejected-memory errors, cache eviction, job scheduling, and OOM events.

The node resource manager exposes its calculated `total_memory`, `memory_demand`, and `memory_limit_per_category` through Orchid. Tracker and component-specific Orchid trees and metrics provide actual usage; exact paths and metric names may differ by release.

Apply changes in this order:

1. Confirm that the runtime/container limit is the intended physical ceiling.
2. Choose a watermark that covers untracked memory, transient peaks, and allocator behavior.
3. Set explicit limits for workloads that require isolation; use dynamic limits only where equal sharing is appropriate.
4. Roll out to a small node or proxy group and verify the effective values, not merely the stored dynamic configuration.
5. Reduce limits gradually. Wait for caches and workload owners to react before the next reduction.
6. Check that job capacity, tablet write/compaction behavior, lookup latency, proxy rejection rate, and RSS remain healthy.

Common mistakes are treating category limits as additive reservations, setting their sum equal to physical memory without a watermark, assuming `none` consumes no dynamic budget, and changing only the proxy category limit while leaving the total or user-pool limit lower.

## Current limitations and follow-up topics

- Accounting coverage depends on allocations being explicitly tracked; `footprint` is the residual rather than a perfect subsystem attribution.
- Enforcement behavior belongs to each subsystem, so an exceeded limit does not imply a uniform response.
- Bundle and user pools add another hierarchy that deserves a separate operational guide.
- CPU, network, disk-space, slot, and job-resource management are outside this first draft.
- A follow-up should document stable Orchid paths, metrics, alerts, and concrete rollout procedures for each daemon type.
