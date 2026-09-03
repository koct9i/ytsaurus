---
type: Draft Article
title: "Draft-24: Cluster node and proxy resource management: memory tracking"
last_modified: 2026-09-01T00:00:00Z
tags: [cluster-node, proxy, memory, configuration]
status: draft
---

# Cluster node and proxy resource management: memory tracking

{% note info "Draft" %}

This article is an initial description of resource management in cluster nodes, HTTP proxies, and RPC proxies. It currently covers memory tracking and memory limits. Configuration names and operational recommendations may change while the article is being reviewed.

{% endnote %}

## Memory tracking model

Each server process has a memory usage tracker. Code that owns a buffer charges its size to a **memory category** and releases the charge when the buffer is destroyed. The tracker maintains:

* usage for each category;
* a limit for each category;
* total tracked usage and a total limit;
* optional finer-grained pools, for example per-user or per-tablet-cell-bundle usage.

A category is an accounting and back-pressure boundary, not an operating-system isolation boundary. Exceeding a tracker limit does not automatically make the kernel reclaim memory. The component that reserves memory decides whether to reject a request, wait, evict cached data, throttle work, or raise an alert. A process can still be killed by its container or host limit before a poorly sized or incompletely tracked workload is stopped.

Tracked usage is also not identical to RSS. RSS can contain allocator fragmentation, stacks, executable pages, memory mappings, and allocations that have not been attributed to a category. Conversely, tracked buffers can include memory that is not currently resident. Use category counters to find the owner of memory and use RSS, cgroup statistics, and the OOM log to verify the process-level safety margin.

## Memory categories

Category names in YSON use `snake_case`. Not every component uses every category. The common categories are grouped below; the exact set may grow as new subsystems acquire explicit tracking.

| Group | Categories | What they account for |
| --- | --- | --- |
| Process overhead | `footprint`, `tablet_footprint`, `alloc_fragmentation`, `profiling`, `logging`, `huge_page` | Baseline process/tablet overhead, allocator slack, diagnostics, logs, and explicitly tracked huge pages. `alloc_fragmentation` represents free space retained inside the allocator and is diagnostic rather than additional live allocations. |
| Data-node caches and metadata | `block_cache`, `chunk_meta`, `chunk_block_meta`, `chunk_blocks_ext`, `chunk_journal_index`, `versioned_chunk_meta`, `master_cache`, `chunk_replica_cache`, `p2p` | Cached blocks, chunk metadata and indexes, master metadata, replica data, and peer-to-peer cache traffic. |
| Data-node I/O | `pending_disk_read`, `pending_disk_write` | Buffers held by disk read and write requests. `blob_session` is a legacy compatibility category; configure `pending_disk_write` for new deployments. |
| Jobs | `user_jobs`, `system_jobs`, `tmpfs_layers`, `job_input_block_cache`, `job_input_chunk_meta_cache` | User and system job memory, tmpfs-layer accounting, and exec-node input caches. Job-level limits remain a separate layer from the node-wide `user_jobs` budget. |
| Tablets | `tablet_static`, `tablet_dynamic`, `lookup_rows_cache`, `tablet_background`, `tablet_row_merger`, `table_replication`, `chaos_replication_incoming`, `chaos_replication_outgoing` | In-memory stores, dynamic stores, lookup cache, background tablet work, row merging, and replication buffers. Some tablet categories can additionally be divided by bundle. |
| Queries and reads | `query`, `lookup`, `fetch_table_rows`, `read_table` | Query execution and row/table read request buffers. |
| Transport and requests | `rpc`, `heavy_request` | RPC transport buffers and memory attributed to heavy proxy requests. |
| Fallback | `unknown`, `mixed`, `unrecognized` | Memory with no more specific owner, memory shared by several owners, and categories sent by a newer component that the receiver does not recognize. Persistent growth here is a signal to investigate tracking coverage or version skew. |

Limits should be assigned to the categories that can grow with workload. A category limit is not a reservation: unused capacity is not allocated up front. On a cluster node, however, categories configured with a `dynamic` limit divide the remaining memory budget among themselves, as described below.

## Cluster node limits

### Static configuration

Cluster-node memory settings live under `resource_limits` in the node's static configuration. `total_memory` is the process-wide budget used by the node resource manager. `free_memory_watermark` leaves headroom before category budgets are calculated. `memory_limits` maps category names to limit descriptors:

```yson
resource_limits = {
    total_memory = 64G;
    free_memory_watermark = 4G;
    memory_limits = {
        block_cache = {type = static; value = 8G;};
        pending_disk_read = {type = static; value = 2G;};
        pending_disk_write = {type = static; value = 2G;};
        user_jobs = {type = dynamic;};
        tablet_dynamic = {type = dynamic;};
        lookup_rows_cache = {type = none;};
    };
};
```

The descriptor types have the following meaning:

* `static` assigns the byte value from `value`. A static descriptor without `value` is invalid.
* `dynamic` receives an equal share of the budget left after the watermark and all non-dynamic categories have been accounted for. A dynamic descriptor must not contain `value`.
* `none`, a missing descriptor, or a descriptor without `type` does not create a configured category cap. For budget calculation, the node subtracts an explicit limit installed by the owning cache if one exists; otherwise it subtracts current usage. Thus `none` does **not** mean zero usage or unlimited memory that is invisible to the total limit.

For `N` dynamic categories, the effective limit of each is calculated approximately as:

```text
max((effective_total_memory - free_memory_watermark
     - sum(static and owner-provided limits)
     - sum(current usage of otherwise unbounded categories)) / N, 0)
```

This is an equal split, not a weighted split. Adding a dynamic category therefore reduces every other dynamic category's limit. Because usage of categories without an explicit limit participates in the calculation, their growth can also reduce the dynamic-category budget.

The older `user_jobs`, `tablet_static`, and `tablet_dynamic` fields directly under `resource_limits` are compatibility aliases represented by the same limit descriptor. Prefer the entries in `memory_limits` in new configurations. `lookup_rows_cache` defaults to a static limit of zero unless it is overridden.

`total_memory` must fit below the physical or container memory limit with room for the operating system, allocator and untracked process memory. In a container, ensure the runtime/cgroup limit is higher than the node's total budget; otherwise the kernel can kill the node before internal admission control reacts.

### Dynamic configuration

Change supported limits without restarting nodes through `//sys/cluster_nodes/@config`. This attribute is a map from node filter to a dynamic configuration patch. Put `resource_limits.memory_limits` inside the selected patch:

```bash
yt set //sys/cluster_nodes/@config '%true={resource_limits={
    free_memory_watermark=6G;
    memory_limits={
        block_cache={type=static;value=10G;};
        user_jobs={type=dynamic;};
        tablet_dynamic={type=dynamic;};
    };
}}'
```

Use a more selective node filter instead of `%true` in a heterogeneous cluster, and ensure filters do not overlap. A dynamic category entry overrides the corresponding static entry; omitted entries fall back to static configuration. To return a category to the static setting, remove its dynamic override rather than copying an old value into the dynamic document.

The effective total memory can also be constrained by the instance-limits tracker and resource-limit overrides. In particular, `system_memory` and `user_memory` overrides replace the calculated `system_jobs` and `user_jobs` values. Treat the effective values reported by the running node—not merely the source configuration—as authoritative.

### Verifying a node configuration

Inspect the applied dynamic config and the calculated limits before and after a change:

```bash
yt get //sys/cluster_nodes/@config
yt get //sys/cluster_nodes/<node-address>/orchid/dynamic_config_manager/effective_config/resource_limits
yt get //sys/cluster_nodes/<node-address>/orchid/node_resource_manager
```

The resource manager's `total_memory` and `memory_limit_per_category` fields show the effective budget. Also inspect the node's `/memory_usage` profiler counters and process/container RSS. Orchid layouts can differ between releases; if a path is absent, browse the instance's `orchid` tree for `resource_manager`, `memory_usage`, and `dynamic_config_manager`.

Roll out changes to a small tagged group first. Lowering a limit below current usage does not make already allocated memory disappear; it can reject new work or trigger eviction and may produce a temporary alert. Watch request errors, cache hit rates, job scheduling, tablet health, RSS, and OOM events during convergence.

## HTTP proxy limits

The HTTP proxy has a total tracked-memory limit. Heavy requests are charged to `heavy_request`; other allocations can be attributed to categories such as `rpc`. In static proxy configuration, set:

```yson
memory_limits = {
    total = 16G;
};
```

The dynamic document is `//sys/http_proxies/@config`. Its `memory_limits.total` overrides the static total. HTTP proxy category limits are derived from role-aware ratios under `api`:

```bash
yt set //sys/http_proxies/@config '{
    memory_limits={total=14G;};
    api={
        default_memory_limit_ratios={
            total_memory_limit_ratio=0.90;
            heavy_request_memory_limit_ratio=0.70;
            default_user_memory_limit_ratio=0.10;
        };
    };
}'
```

`total_memory_limit_ratio` determines how much of `memory_limits.total` the tracker may use, and `heavy_request_memory_limit_ratio` sets the heavy-request category limit from the same base. Optional per-user ratios can isolate request memory further. Keep both the ratio headroom and container headroom: the first protects tracked proxy work, while the second covers untracked memory and process overhead.

## RPC proxy limits

RPC proxies expose direct total and category limits. Static configuration accepts:

```yson
memory_limits = {
    total = 16G;
    heavy_request = 10G;
    rpc = 4G;
};
```

Apply runtime overrides at `//sys/rpc_proxies/@config`:

```bash
yt set //sys/rpc_proxies/@config/memory_limits '{
    total=14G;
    heavy_request=8G;
    rpc=3G;
}'
```

An omitted dynamic `heavy_request` or `rpc` value falls back to its static value, or to the effective total when the static category value is also absent. A dynamic `total` value changes the total limit; if it is omitted, the currently configured total remains in effect. Avoid setting each category equal to the total unless that concurrency is intentional: category limits are guardrails within the total, not independent pools whose sizes are summed.

## Sizing workflow

1. Start with the host or container memory limit and reserve headroom for kernel-charged memory, allocator behavior, stacks, mappings, monitoring, and short peaks.
2. Set the component's total tracked-memory budget below that boundary.
3. Give hard static limits to caches and queues whose useful size is known.
4. Use dynamic node categories only for workloads that should share the remainder equally; use static limits when unequal service priorities are required.
5. Replay representative load and compare per-category usage, total tracked usage, RSS, and cgroup memory. The gap between tracked memory and RSS must remain bounded under sustained load.
6. Canary dynamic changes, verify the effective configuration, then expand the node filter or proxy rollout.

Do not solve persistent OOMs only by increasing one category limit. First determine whether the failing boundary is the category tracker, the component total, the job/container cgroup, or the host. Increasing an internal limit when the cgroup is already tight makes kernel OOM termination more likely.

## Troubleshooting checklist

| Symptom | Checks | Typical action |
| --- | --- | --- |
| Category-limit errors while RSS is low | Effective category limit, dynamic-category count, recent config changes | Rebalance static limits or remove an unintended dynamic category. |
| RSS is close to the cgroup limit but tracked total is low | Allocator fragmentation, `unknown`/`mixed`, stacks and mappings, tracking gaps | Capture heap/cgroup diagnostics and increase safety headroom; do not hide the gap by raising the tracked total. |
| Node limit differs from static config | Effective dynamic config, bundle/instance limits, resource overrides | Remove stale overrides or update the controlling source rather than repeatedly editing static config. |
| Cache hit rate collapses after a rollout | Cache category usage and limit, new dynamic split, eviction counters | Restore cache capacity or use an explicit static cache limit. |
| Proxy rejects large requests | Total, `heavy_request`, per-user pool limit, role-specific HTTP ratios | Adjust the narrowest applicable limit and retain process headroom. |
| Process is OOM-killed without a tracker error | Container/pod events, cgroup `memory.current` and `memory.events`, RSS versus tracked total | Increase external capacity or reduce the internal total; investigate untracked memory. |
