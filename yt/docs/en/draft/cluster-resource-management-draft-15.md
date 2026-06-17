# Cluster resource management

{% note warning "Draft" %}

This is a draft overview. It intentionally collects several resource-control mechanisms in one place; individual clusters may use different defaults, disabled subsystems, or additional admission-control rules.

{% endnote %}

This article describes the main {{product-name}} resources that have limited capacity or explicit control knobs, and explains how to configure limits, control consumption, monitor usage, and react to shortage or overcommit.

## Resource-control planes

Resource management in {{product-name}} is split between several planes:

1. **Operation scheduler**: controls resources consumed by jobs and operations: CPU, memory, GPU, user slots, network scheduling weight, local job disk requests, and operation concurrency. Scheduler state is organized as pool trees under `//sys/pool_trees`.
2. **Accounts**: control persistent storage and master metadata resources: disk space per medium, Cypress node count, chunk count, master memory, tablets, and tablet static memory. Account state is under `//sys/accounts` and `//sys/account_tree`.
3. **Tablet cell bundles**: control dynamic-table serving capacity: tablet nodes, tablet count, tablet static memory, CPU, memory, thread pools, and memory categories. Bundle state is under `//sys/tablet_cell_bundles`.
4. **Masters and proxies**: control request rates, request queues, request complexity, and RPC/service concurrency. User request limits live on `//sys/users/<user>`; master-side global throttlers are configured in master dynamic config.
5. **Data nodes and exec nodes**: enforce local physical limits such as disk location space, write sessions, read/write buffers, job sandbox disk space, memory cgroups, tmpfs, per-node job CPU/memory/GPU capacity, and per-node network/disk pressure. In Kubernetes deployments, the exec-node pod specification and the `jobs` sidecar/container resource limits define the physical capacity that the scheduler can expose for user jobs.
6. **Distributed throttlers**: control cluster-wide bandwidth or RPS for selected traffic classes, especially inter-cluster reads.

A useful operational rule is to classify every symptom by the plane that owns it: a pending operation is usually a scheduler issue; a failed write due to quota is usually an account or bundle issue; a slow `get/list/exists` workload is often a master/proxy request-pressure issue; a mounted dynamic table rejecting writes is usually a tablet-bundle memory or flush-throughput issue.

Capacity and request/limit knobs are different layers. For example, `jobResources.limits.cpu` in the Kubernetes spec defines how much CPU an exec node may offer for user jobs, while `mapper.cpu_limit` defines how much of that capacity one job requests. Increasing a per-job limit cannot create capacity if the exec-node `jobs` container, GPU devices, or slots volume are already the bottleneck.

## Quick inventory

| Resource | Primary owner | Main configuration points | Main observability points | Shortage or overcommit behavior |
|---|---|---|---|---|
| Exec-node job capacity | Kubernetes/operator spec, exec node, scheduler | Exec-node pod `resources`; CRI `jobResources.limits.cpu`, `jobResources.limits.memory`, GPU limits; slots-location volume/PVC/`emptyDir.sizeLimit`; exec-node `@resource_limits` | Node UI; `//sys/exec_nodes/<node>/@resource_limits`; Kubernetes pod/container resources; slot-location disk metrics | Scheduler cannot run more jobs than node-advertised CPU, memory, GPU, user slots, and disk capacity; jobs may stay pending or fail on node-local exhaustion |
| Per-exec-node job distribution | Scheduler, exec node, administrators | Node tags and pool-tree `node_tag_filter`; operation `scheduling_tag_filter`; node `resource_limits_overrides`; `disable_scheduler_jobs`; job resource vector and data locality | Scheduler orchid per-node scheduling attributes; node `@resource_usage`; operation job distribution by address; unutilized-resource reasons | Jobs are not round-robin; they are placed where a heartbeat has feasible free resources in the relevant tree, so heterogeneous nodes, tags, locality, and dominant resources shape distribution |
| Job CPU | Scheduler, exec node | Operation task `cpu_limit`; pool `strong_guarantee_resources` and `resource_limits`; optional container CPU limit | Operation UI, scheduler orchid, job statistics, CPU metrics | Jobs are throttled or scheduled less often; starving operations may trigger preemption |
| Job memory | Scheduler, exec node | Operation task `memory_limit`; memory reserve settings; pool memory guarantees/limits | Operation UI, job statistics, memory digest, aborted job reasons | Job may be aborted on memory limit or resource overdraft; scheduler reduces concurrency |
| GPUs | Scheduler, exec node | Operation task `gpu_limit`; GPU pool tree/node tags; pool guarantees/limits | Operation UI, scheduler resource usage, node GPU health | Jobs remain pending; failed GPU jobs may be restarted or operation may fail |
| User slots / job count | Scheduler | Pool `resource_limits.user_slots`; operation `resource_limits.user_slots`; tree operation-count limits | Scheduling page, operation progress | New jobs are not scheduled; operations remain pending |
| Operation count | Scheduler | Pool/tree `max_operation_count`, `max_running_operation_count` | Scheduling page | Operations stay pending in pools |
| Scheduler network resource | Scheduler | Automatic network resource for shuffle-heavy operations; pool limits where enabled | Operation/scheduler resource usage | Fewer network-heavy jobs are scheduled; preemption may occur |
| Slots location capacity | Kubernetes/operator spec, exec node | Slots-root volume/PVC or `emptyDir.sizeLimit`; exec-node location configuration; user-slot count | Kubernetes volume usage; exec-node location free space/inodes; node alerts | Jobs fail or cannot start when sandboxes, temporary files, or stderr exhaust the slots location |
| Job sandbox disk | Scheduler, exec node | Task `disk_request` with `disk_space`, `inode_count`, `account`, `medium_name` | Job statistics; node disk usage; job stderr/errors | Job interruption or failure if disk fills; scheduler avoids overcommitting requested space |
| Persistent disk space | Accounts, data nodes | Account `resource_limits.disk_space_per_medium`; table `primary_medium`; replication/erasure/compression settings | Account UI; `@resource_usage`; Prometheus account metrics | New writes or creates under the account may be denied; usage may temporarily exceed limits because accounting is asynchronous |
| Chunk count | Accounts, masters | Account `resource_limits.chunk_count`; writer chunk-size settings; merge/compaction policy | Account UI; `@resource_usage.chunk_count`; `yt_accounts_chunk_count` | New data-producing writes may be denied; masters spend more memory/CPU on metadata |
| Cypress node count | Accounts, masters | Account `resource_limits.node_count`; application object layout | Account UI; `@resource_usage.node_count` | Creating new Cypress nodes may be denied |
| Master memory | Accounts, masters | Account `resource_limits.master_memory`; object/chunk/table design | Account UI; `@resource_usage.master_memory`; master RSS metrics | Creates/writes may be denied; master latency and snapshot pressure increase |
| Tablet count | Tablet cell bundles, accounts | Bundle/account `resource_limits.tablet_count`; table resharding | Bundle UI; `@resource_usage.tablet_count`; tablet statistics | Creating or resharding tablets may fail; balancing options become constrained |
| Tablet static memory | Tablet cell bundles, accounts | Bundle/account `resource_limits.tablet_static_memory`; in-memory table settings | Bundle UI; `@resource_usage.tablet_static_memory`; tablet node memory | Mount or writes may fail; in-memory reads may be unavailable until preload succeeds |
| Tablet serving CPU and memory | Bundle controller/tablet bundles | Bundle `@resource_limits.cpu`, `@resource_limits.memory`; bundle node count; thread pools and memory categories | Bundle UI; tablet node metrics; dynamic-table profiling | Reads/writes slow down, stores accumulate, flush/compaction lag grows; bundle may need more nodes/resources |
| Dynamic-table flush/compaction IO | Tablet nodes, data nodes | Bundle resources; table/store/compaction settings; media choice | Tablet errors, store counts, flush/compaction metrics, disk bandwidth | Writes may be rejected due to tablet memory; read amplification and latency grow |
| Data-node disk IO bandwidth | Data nodes | Location/media configuration; operation IO settings; compaction/repair throttlers where configured | Data-node disk metrics, per-location queues, operation throughput | Read/write latency grows; background jobs slow; write watermarks may disable writes |
| Intra-cluster network bandwidth | Scheduler, data nodes, RPC | Scheduler network resource; job concurrency; read/write window sizes; RPC limits | Node network metrics, RPC metrics, operation throughput | Jobs slow down; retries/timeouts may rise; scheduler may limit network-heavy jobs |
| Exec-node network bandwidth | Exec node, scheduler, RPC/bus | Cluster-node `network_bandwidth`; in/out throttlers; scheduler `network` resource for network-heavy jobs; job concurrency and tags | Per-node NIC throughput/drops/retransmits; bus/RPC bytes and queues; scheduler network-resource usage | Network-heavy jobs can concentrate on a node unless constrained by tags/resources; throttlers delay traffic, while scheduler network resource reduces placement density for known network-heavy phases |
| Inter-cluster network bandwidth and RPS | Distributed throttlers | `//sys/cluster_throttlers` `cluster_limits.<remote>.bandwidth.limit` and `.rps.limit` | Discovery `local_throttlers`, throttler queue size/rate, dashboards | Reads wait for quota; queues grow; remote-copy/remote-read throughput falls |
| Master read/write request rate | Users, masters, proxies | User `read_request_rate_limit`, `write_request_rate_limit`, `request_queue_size_limit`; global master throttlers | User attributes, request-rate metrics, proxy/master request queues | Requests are throttled or fail with rate/queue-limit errors |
| Master read complexity | Masters | `enable_read_request_complexity_limits`; default/max node-count and result-size limits | Request errors, master/proxy logs, client errors | Oversized subtree reads fail; batched subrequests can fail independently |
| RPC queues and service concurrency | Proxies, masters, nodes | Service configs: request queue size limits, thread pools, timeouts | RPC request queue size/limit metrics, latency histograms | Requests queue, time out, or fail with queue-size errors |
| Transactions and locks | Masters | Transaction timeouts; application transaction discipline | `@locks`, transaction counts, master memory/CPU, request latency | Lock conflicts, higher master memory usage, failed commits, queue buildup |
| Caches | Clients, nodes, masters/tablet nodes | Cache sizes and TTLs in component configs; table in-memory mode | Cache hit rate, memory usage, latency | More backend reads, higher disk/network/master load; memory pressure may evict useful data |
| Job stderr/debug artifacts | Controller agent, exec node, operations archive | Operation `max_stderr_count`; task `max_stderr_size`; `stderr_table_path`; `job_node_account`; task `archive_ttl`; operations-archive retention | Jobs tab, `get_job_stderr`, `stderr_size`, archive tables, account usage for debug artifacts | Stderr is truncated, not collected after count limits, expired from archive, or written to a dedicated stderr table if configured |
| Logs and tracing buffers | Components | Logging categories, rate limits, log rotation/retention | Disk usage, dropped/rate-limited log counters | Logs may be dropped or disks may fill, causing component instability |

## Scheduler-managed job resources

### Exec-node job capacity from deployment configuration

Before an operation-level `cpu_limit` or `memory_limit` can be scheduled, the cluster must advertise enough per-node job capacity. In Kubernetes deployments using CRI, the exec-node pod typically contains the main `ytserver` container and a separate `jobs` container. Job containers are launched inside the `jobs` container, so the `jobs` container limits are the hard physical envelope for user jobs on that exec node.

A typical split in the cluster specification is:

```yaml
execNodes:
  - resources:
      limits:
        cpu: 2
        memory: 10Gi
    jobResources:
      limits:
        cpu: 8
        memory: 40Gi
        nvidia.com/gpu: 1
```

Control layers:

- **Exec-node pod resources**: reserve CPU and memory for the node process itself, JobProxy overhead, container runtime overhead, log writing, and system work.
- **`jobResources.limits`**: define the aggregate CPU, memory, and GPU capacity available to user job containers on this exec node. These values are reflected in scheduler-visible node resource limits after the node registers.
- **Slots location volume**: the volume or PVC mounted as the slots root (for example `/yt/node-data/slots`) defines local disk capacity for job sandboxes, artifacts, temporary files, and job stderr before upload. For `emptyDir`, `sizeLimit` is the local capacity guard; for PVCs, the claim size is the guard.
- **User slots**: the advertised `user_slots` value controls how many allocations can be placed on the node independently of CPU and memory. It must be sized together with `jobResources` and slots disk capacity; many small slots on a small volume can fail through local disk exhaustion.
- **Exec-node `@resource_limits`**: verify what the scheduler actually sees with `yt get //sys/exec_nodes/<node-address>/@resource_limits`.

Monitoring:

- Kubernetes pod/container requested and limited CPU, memory, and GPU.
- `//sys/exec_nodes/<node-address>/@resource_limits`, `@state`, and `@alerts`.
- Slot-location free bytes/inodes and per-node job sandbox usage.
- Scheduler view of free node resources and unutilized-resource reasons.

Shortage and overcommit:

- If `jobResources` is smaller than expected, the scheduler has less CPU, memory, or GPU capacity to place jobs, even if pool guarantees are larger.
- If the main exec-node container is underprovisioned, node heartbeats, job setup, layer preparation, and log upload may become bottlenecks although user job limits look healthy.
- If the slots volume is too small, jobs can fail locally even when account disk quota is available; increase the slots volume/PVC or reduce job concurrency and temporary data.
- GPU limits in `jobResources` and GPU device discovery must match the physical node; otherwise GPU jobs remain pending or fail during container startup.

### Per-exec-node job distribution and balancing

The scheduler does not distribute jobs by a simple round-robin rule. Scheduling is heartbeat-driven: an exec node reports completed/running jobs and currently available resources, and the scheduler tries to place new allocations that fit this node, the node's pool tree, operation constraints, and the global fair-share state. This means job distribution across nodes is a result of several constraints rather than one balancing switch.

Controls that shape per-node distribution:

- **Advertised node resources**: `//sys/exec_nodes/<node>/@resource_limits` defines the CPU, memory, GPU, and user-slot capacity seen by the scheduler. `resource_limits_overrides` can reduce or override CPU/memory/GPU-style resources for maintenance or heterogeneity tests, but does not change `user_slots`.
- **Node eligibility**: pool-tree `node_tag_filter` selects which nodes belong to a tree; operation `scheduling_tag_filter` further restricts where one operation may run; node `user_tags` can be used to mark hosts for these filters.
- **Administrative drain controls**: `disable_scheduler_jobs=%true` stops new jobs from being scheduled on a node and interrupts existing jobs after the configured timeout; banning or taking a node offline removes it from scheduling entirely.
- **Operation concurrency and size**: operation/pool `resource_limits.user_slots`, task `cpu_limit`, `memory_limit`, `gpu_limit`, and `disk_request` determine how many jobs can fit on one node at the same time.
- **Data locality and job type**: reads, shuffle/sort phases, GPU jobs, and operations with special tag filters may naturally concentrate on a subset of nodes.

Monitoring:

- Node attributes: `@resource_limits`, `@resource_usage`, `@tags`, `@alerts`, and `@state`.
- Scheduler orchid/scheduling UI per-node scheduling attributes and unutilized-resource reasons.
- Operation job list grouped by node address to detect hot nodes or skewed placement.
- Per-node CPU, memory, GPU, slots-location, and network metrics.

Shortage and overcommit:

- If one resource dimension is exhausted on a node, jobs that need that dimension cannot be placed there even if other dimensions are free; this is the common source of fragmentation.
- If nodes are heterogeneous but placed in the same tree without tags or adjusted resource limits, large jobs may queue behind a small feasible subset of nodes.
- If a node is network- or disk-hot but still has CPU and memory, the scheduler may continue placing CPU-fitting jobs unless network/disk pressure is represented by tags, throttlers, the scheduler `network` resource, or administrative drain.

### CPU

A job requests CPU with the task-level `cpu_limit` option. One CPU corresponds to one HyperThreading core in scheduler accounting. By default, custom jobs request one CPU. For multi-threaded jobs, increase `cpu_limit`; otherwise the job can consume more wall-clock time than expected while being throttled or weighted as a one-core job.

Example:

```yson
{
  mapper = {
    command = "python3 mapper.py";
    cpu_limit = 4;
  };
  pool_trees = [default];
}
```

Control layers:

- **Per job**: `cpu_limit` in the relevant task section (`mapper`, `reducer`, `tasks.<name>`, and so on).
- **Per operation**: root `resource_limits`, for example `{cpu=100; user_slots=200}` where supported by the scheduler.
- **Per pool**: `//sys/pool_trees/<tree>/<pool>/@strong_guarantee_resources` and `@resource_limits`.
- **Per tree**: node tag filters, main resource, and preemption settings in `//sys/pool_trees/<tree>/@config`.
- **Container enforcement**: `set_container_cpu_limit` may set a hard container CPU cap instead of only proportional CPU weight.

Monitoring:

- Operation UI: requested CPU, running job count, fair share, starvation/preemption status.
- Scheduler orchid and scheduling page: pool fair share, usage share, limits, guarantees.
- Job statistics: user CPU/system CPU and elapsed time.
- Node metrics: CPU saturation, run queue, throttling, and system CPU/softirq when network-heavy jobs are involved.

Shortage and overcommit:

- If free CPU is unavailable or pool share is exhausted, jobs remain pending.
- If another operation is below fair share long enough, the scheduler may preempt newer/preemptible allocations.
- If a job consumes more CPU than requested and a hard cap is enabled, it is throttled by the container; otherwise it may get proportional weight but still distorts fairness and latency.

### Memory

A job requests memory with task-level `memory_limit`. The scheduler uses this value plus system overhead such as JobProxy buffers when deciding how many jobs can run. Memory is not only an execution limit; it is also a concurrency control knob.

Example:

```yson
{
  mapper = {
    command = "./mapper";
    memory_limit = 2147483648;  // 2 GiB
  };
}
```

Control layers:

- **Per job**: `memory_limit` in the user-job spec.
- **Job IO buffers**: `job_io.table_reader.window_size`, writer buffers, sort/partition buffers, and other operation-specific buffers increase actual scheduler memory needs.
- **Memory reserve**: the controller can choose a reserve from historical job statistics. This improves packing but can produce `resource_overdraft` aborts if jobs are underestimated.
- **tmpfs**: `tmpfs_path`, `tmpfs_size`, and `tmpfs_volumes` consume memory-backed storage. Without an explicit `tmpfs_size`, tmpfs defaults to `memory_limit` and the reserve is forced conservatively.
- **Pool limits**: pool and operation `resource_limits.memory` cap aggregate memory.

Monitoring:

- Operation UI and job statistics: maximum memory, cumulative memory reserve, job abort reason.
- Scheduler: allocated memory vs fair share and limits.
- Node metrics: RSS, cgroup memory, OOM events, swap/PSI if available.

Shortage and overcommit:

- A job exceeding its memory cgroup limit is aborted.
- A job whose actual memory exceeds the scheduler reserve can be aborted as `resource_overdraft` to protect node stability.
- If cluster memory is fragmented by small allocations, large-memory jobs may wait until preemption can free a sufficiently large node slot.

### GPUs and other accelerators

GPU jobs request devices with `gpu_limit`. In practice GPU scheduling also depends on pool trees and node tags that select GPU-capable exec nodes.

Control layers:

- **Per job**: `gpu_limit`.
- **Per tree/pool**: GPU-specific pool tree, `strong_guarantee_resources.gpu`, `resource_limits.gpu` where enabled, and node tag filters.
- **Runtime environment**: CUDA/toolkit layers and driver compatibility.

Monitoring:

- Scheduler GPU usage and pending jobs.
- Node-level GPU health/utilization/exporter metrics.
- Job stderr and fail context for CUDA initialization errors.

Shortage and overcommit:

- Jobs remain pending if no matching GPU node has free CPU, memory, user slot, and GPU resources simultaneously.
- A bad or unhealthy GPU can cause repeated job failures; the scheduler may restart jobs until operation failure thresholds are reached.

### User slots and operation concurrency

`user_slots` control the number of concurrent custom jobs. Each custom job consumes one slot independently of CPU and memory size.

Control layers:

- Pool `resource_limits.user_slots`.
- Operation `resource_limits.user_slots` to cap parallelism of one operation.
- Pool/tree `max_operation_count` and `max_running_operation_count` to cap concurrently running or queued operations.
- Operation-specific job-count settings such as `job_count`, `map_job_count`, `partition_job_count`, and data-size-per-job parameters.

Monitoring:

- Scheduling page: running and pending operations, running job count, pending job count.
- Operation UI: planned/running/completed jobs and pool/tree placement.

Shortage and overcommit:

- If user slots are exhausted, the scheduler does not start more jobs even if CPU and memory are available.
- Too many tiny jobs waste scheduler, master, and node overhead; too few jobs underutilize the cluster.

### Scheduler network resource

The scheduler has a `network` resource used mainly for operations that can generate heavy internal traffic, such as sort and shuffle phases. Its unit is abstract: it is not bytes per second, but a scheduling weight chosen so co-located jobs do not overload node network channels.

Control layers:

- Mostly automatic for operation types that need it.
- Indirect control through job counts, partition counts, operation type, pool limits, and data locality settings.
- Global control through pool tree configuration if the cluster exposes network as a limited resource.

Monitoring:

- Operation throughput and stage-specific counters.
- Node NIC throughput, retransmits, socket queues, and RPC latency.
- Scheduler resource usage for the `network` resource when exposed.

Shortage and overcommit:

- The scheduler starts fewer network-heavy jobs per node.
- If traffic exceeds physical NIC or ToR capacity despite scheduling, operation latency rises and retries/timeouts can appear.

### Slots location and local job disk

A job uses local sandbox disk for input artifacts, temporary files, stderr, core files, and user-created files. The aggregate local capacity comes from the exec-node slots-location volume configured in the cluster deployment; per-job reservation and limiting are controlled with `disk_request`. Treat the slots location as a finite per-exec-node resource: its size bounds the total local disk footprint of all concurrent jobs on the node, regardless of persistent account disk quota. By default disk space may not be strictly accounted to a user account, but `disk_request` can both reserve and limit sandbox disk resources.

Example:

```yson
{
  mapper = {
    command = "./mapper";
    disk_request = [
      {
        disk_space = 10737418240;  // 10 GiB
        inode_count = 100000;
        account = "analytics";
        medium_name = "default";
      }
    ];
  };
}
```

Control layers:

- Task `disk_request.disk_space` and `disk_request.inode_count`.
- `medium_name` to choose HDD/SSD-like job media where configured.
- `account` to charge guaranteed requested space on non-default media.
- Artifact size limits and `copy_files`/`tmpfs` settings.

Monitoring:

- Job statistics and failure reason.
- Exec-node disk-free and inode-free metrics by slots location.
- Kubernetes volume/PVC usage for the slots location.
- Node alerts for low disk watermarks.

Shortage and overcommit:

- If the job fills its requested disk limit, it fails or is interrupted.
- If a node location fills unexpectedly, node-level watermarks may stop writes or interrupt jobs to protect the node.

## Account-managed storage and metadata resources

Accounts manage persistent storage and metadata quota. Account attributes are located at `//sys/accounts/<account>`; the most important are `@resource_limits`, `@resource_usage`, `@recursive_resource_usage`, `@violated_resource_limits`, and recursive violation counters.

### Disk space per medium

Disk quota is configured per medium in `resource_limits.disk_space_per_medium`. The aggregate `disk_space` field is derived/read-only. Actual physical usage depends on replication factor, erasure coding, compression, and medium placement.

Example:

```bash
yt set //sys/accounts/analytics/@resource_limits/disk_space_per_medium/default 109951162777600
```

Consumption controls:

- Pick the table/file medium with `primary_medium`.
- Reduce replication factor only where reliability policy permits.
- Use compression and erasure coding appropriately.
- Remove stale data and temporary operation outputs.
- Merge small chunks to reduce metadata pressure, but remember that merging consumes IO and temporary storage.

Monitoring:

```bash
yt get //sys/accounts/analytics/@resource_usage
yt get //sys/accounts/analytics/@violated_resource_limits
```

Also use the Accounts UI and metrics such as account disk-space and chunk-count gauges.

Shortage and overcommit:

- New writes under the account can be rejected when the limit is violated.
- Usage may temporarily exceed the limit because accounting is asynchronous or because limits were lowered below current usage.
- Replication repair, chunk movement, and merge jobs may need additional temporary headroom.

### Chunk count

Each table and file consists of chunks. Chunk count is a master-memory and scheduling scalability resource, not just a storage-detail counter.

Control layers:

- Account `resource_limits.chunk_count`.
- Writer `desired_chunk_size`, block sizes, and operation job granularity.
- Periodic `merge --mode auto` for tables with too many small chunks.
- Dynamic-table flush and compaction policy.

Monitoring:

- `//sys/accounts/<account>/@resource_usage/chunk_count`.
- Account dashboards and metrics such as `yt_accounts_chunk_count`.
- Table attributes such as `@chunk_count` for hot spots.

Shortage and overcommit:

- New chunk-producing writes can fail.
- Master CPU and memory grow with high chunk count, increasing snapshot and mutation pressure.

### Cypress node count

Cypress nodes include tables, files, directories, links, locks, and other metadata-tree objects.

Control layers:

- Account `resource_limits.node_count`.
- Application data model: avoid creating millions of tiny tables or directories where one partitioned table is enough.
- Retention for temporary paths, logs, and per-run output directories.

Monitoring:

- `//sys/accounts/<account>/@resource_usage/node_count`.
- Recursive account usage for subtrees.
- Cypress listing latency and master read complexity errors.

Shortage and overcommit:

- Creating new nodes can fail.
- Large directory listings can hit request complexity limits or cause master pressure.

### Master memory

`master_memory` accounts for metadata stored by masters: node metadata, chunk metadata, attributes, pivot keys, table metadata, and other persistent state.

Control layers:

- Account `resource_limits.master_memory`.
- Reduce node count, chunk count, attribute payloads, wide schemas with excessive metadata, and oversized pivot-key sets.
- Split very large clusters across cells when single-cell master limits are reached.

Monitoring:

- Account `@resource_usage.master_memory`.
- Master process RSS and automaton CPU.
- Snapshot duration, snapshot size, and changelog backlog.

Shortage and overcommit:

- Account-level metadata operations can be denied.
- Cluster-wide master memory pressure increases snapshot risk; snapshot creation may need substantial headroom because of fork/copy-on-write behavior.

## Dynamic-table and bundle resources

Dynamic tables consume both account quotas and tablet-bundle capacity. The account owns persistent data and metadata resources; the bundle owns serving resources.

### Tablet count

Tablet count controls sharding and parallelism. More tablets can improve parallel reads/writes but increase master and tablet-node overhead.

Control layers:

- Bundle/account `resource_limits.tablet_count`.
- Resharding commands and automatic/tablet balancers.
- Table design: key distribution, ordered-table partitioning, queue partition count.

Monitoring:

- Bundle UI and table tablet view.
- `//sys/tablet_cell_bundles/<bundle>/@resource_limits` and usage.
- Per-table tablet statistics and tablet errors.

Shortage and overcommit:

- Resharding or creating more tablets fails if the tablet-count quota is exhausted.
- Too few tablets causes hot tablets; too many tablets increases overhead and can hurt latency.

### Tablet static memory and in-memory tables

Tablet static memory limits data loaded into memory, especially in-memory dynamic tables. It is configured on bundles and represented in account resource structures.

Control layers:

- Bundle/account `resource_limits.tablet_static_memory`.
- Table `in_memory_mode` and preload behavior.
- Bundle node count and memory category distribution.

Monitoring:

- Bundle UI memory usage.
- Tablet node memory metrics.
- Table mount/preload state and `@tablet_errors`.

Shortage and overcommit:

- Mounting or preloading in-memory tables can fail or remain incomplete.
- Reads from in-memory tables may be rejected or fall back depending on mode and readiness.
- Writes can be disabled if tablet memory pressure prevents stores from being flushed fast enough.

### Bundle CPU, memory, and thread pools

Tablet bundles isolate dynamic-table serving. When the Bundle controller is used, bundle `@resource_limits` contains at least `tablet_count`, `tablet_static_memory`, `cpu`, and `memory`. CPU and memory limits determine how many tablet-node instances the controller can allocate to the bundle.

Control layers:

- `//sys/tablet_cell_bundles/<bundle>/@resource_limits/cpu`.
- `//sys/tablet_cell_bundles/<bundle>/@resource_limits/memory`.
- Bundle node count, spare-node policy, thread pools for lookup/select/write paths, and memory category distribution.
- Table-level compaction/partitioning and store-rotation settings.

Monitoring:

- Bundle UI: health, allocated nodes, cell state, resource usage.
- Tablet-node CPU, memory, thread-pool queues, lookup/select/write latency.
- Store counts, flush lag, compaction backlog, and tablet errors.

Shortage and overcommit:

- Lookup/select latency grows when serving CPU or read threads saturate.
- Writes first accumulate in dynamic stores; if flush cannot keep up, memory pressure grows and writes may be disabled.
- Compaction lag increases read amplification and can later turn into write failures.
- If nodes fail and no spare capacity is available, the Bundle controller may be unable to restore target capacity automatically.

## IO bandwidth resources

### Persistent data-node disk IO

Disk IO is consumed by table reads/writes, dynamic-table flushes, compactions, replication repair, chunk balancing, merges, sorted operation shuffle, and background maintenance.

Configuration and controls:

- Media placement: choose `default`, SSD-like media, journal media, or in-memory media according to workload.
- Operation IO settings: reader `window_size`, writer `desired_chunk_size`, replication factor, compression codec, and job parallelism.
- Background-job throttlers for compaction, repair, replication, and chunk balancing if configured on the cluster.
- Disk location watermarks and per-location limits in node configuration.

Monitoring:

- Data-node disk utilization, await/queue depth, read/write bytes, throttler queues.
- Operation throughput and stalled jobs.
- Dynamic-table flush/compaction backlogs.
- Chunk repair and replication queues.

Shortage and overcommit:

- Latency rises before hard failures appear.
- Write paths may hit low-space or disable-write watermarks.
- Background maintenance slows down, which can later surface as under-replicated chunks or dynamic-table store buildup.

### Job input/output buffering

Job IO buffers can trade memory for throughput. Larger buffers and bigger chunks usually improve throughput but increase memory and tail latency; smaller buffers reduce per-job memory but increase RPC and chunk overhead.

Configuration and controls:

- `job_io.table_reader.window_size`.
- `job_io.table_writer.desired_chunk_size`.
- Operation-specific partition/sort buffer settings.
- Number of parallel jobs and input split size.

Monitoring:

- JobProxy memory statistics.
- Data read/write rate per job.
- RPC request rate and data-node read session metrics.

Shortage and overcommit:

- Too-small chunks create high chunk count and master overhead.
- Too-large buffers reduce scheduler concurrency and can trigger memory overdraft.

## Network bandwidth resources

### Intra-cluster network

Intra-cluster network is used for table reads from remote nodes, writes/replication, shuffle, sorted merge, tablet traffic, master-to-node heartbeats, and client/proxy traffic.

Configuration and controls:

- Scheduler `network` resource for shuffle-heavy workloads. The unit is abstract, but it lowers the density of jobs known to consume significant node network bandwidth.
- Cluster-node `network_bandwidth` should match usable host bandwidth; stale values make the scheduler and throttlers believe a node is larger or smaller than it is.
- Node-level `in_throttler`, `out_throttler`, `in_throttlers`, `out_throttlers`, and `throttler_free_bandwidth_ratio` can reserve headroom and split bandwidth between traffic classes where configured.
- Operation job count, partition count, and `resource_limits.user_slots` control how many jobs can produce network traffic at once.
- Pool-tree/node tags and operation `scheduling_tag_filter` can isolate network-heavy work on nodes or trees prepared for that traffic.
- Data locality settings and sorted-operation locality timeouts reduce unnecessary remote reads.
- RPC concurrency, timeouts, and request queue limits cap request fan-out and queue growth.
- Network project or network isolation settings where used.

Monitoring:

- NIC throughput, drops, retransmits, softirq CPU, and packet errors on exec/data/tablet nodes.
- Bus/RPC bytes, pending bytes, request queues, latency, and retry counters.
- Scheduler usage of the `network` resource and per-node unutilized resources.
- Operation read locality, remote-read volume, shuffle bytes, and per-node job distribution.
- Data-node and tablet-node request queues.

Shortage and overcommit:

- Network-heavy jobs slow down and may time out.
- Retries amplify traffic, so reducing concurrency can improve end-to-end throughput.
- If master/proxy traffic shares the same bottleneck, metadata operations can become slow even when data nodes are healthy.
- If one exec node is network-hot while CPU/memory still look available, reduce per-operation concurrency, isolate the workload with tags/pool trees, lower advertised bandwidth, or drain the node until traffic normalizes.

### Exec-node network bandwidth

Exec-node network bandwidth is consumed by remote chunk reads, table writes, shuffle, job artifact downloads, RPC proxy-in-job, task services, logs/stderr upload, and container image/layer traffic. It is partly visible to the scheduler through the abstract `network` resource for selected operation phases, but many application-level network calls made by user code are invisible unless you control them with concurrency, tags, or external throttling.

Control patterns:

- **Represent physical capacity**: configure host-level `network_bandwidth` and throttlers so the cluster does not assume every exec node can safely push unlimited traffic.
- **Limit placement density**: reduce operation or pool `resource_limits.user_slots`, increase job CPU/memory requests if they were under-requested, or use the scheduler `network` resource where the operation type supports it.
- **Separate noisy traffic**: use node tags, pool-tree `node_tag_filter`, and operation `scheduling_tag_filter` to place heavy shuffle, GPU training, remote-copy, or service jobs on nodes with enough NIC capacity.
- **Shape job behavior**: tune reader `window_size`, writer chunk sizes, partition counts, application-level parallelism, and client retry/backoff so each job does not open unbounded network concurrency.
- **Drain or downrate a hot node**: use `disable_scheduler_jobs` for maintenance/drain or `resource_limits_overrides` for scheduler-visible resources when a node must receive less work.

Monitoring:

- Per-exec-node NIC bytes, drops, retransmits, socket queues, and softirq CPU.
- Job statistics for read/write wait time and job_proxy network/RPC counters.
- Operation job distribution by node address together with per-node NIC usage.
- RPC/bus pending bytes and queue sizes on exec nodes and data nodes.

Shortage and overcommit:

- A node can be network-saturated while scheduler CPU and memory still show headroom; in that case adding `cpu_limit` or memory does not help.
- If network pressure is caused by user code talking to external services, {{product-name}} can mainly limit it indirectly through job count, tags, pools, or container/network policy.
- If network pressure is caused by storage reads/writes or shuffle, first reduce concurrency or improve locality; then adjust `network_bandwidth` and throttlers if physical capacity allows.

### Inter-cluster network bandwidth and RPS

Remote reads and some cross-cluster workflows are controlled by distributed cluster throttlers. The configuration is stored in `//sys/cluster_throttlers` and can specify per-remote-cluster bandwidth and RPS limits.

Example:

```yson
{
  enabled = %true;
  cluster_limits = {
    remote_cluster = {
      bandwidth = {limit = 4294967296;};  // 4 GiB/s
      rps = {limit = 10000;};
    };
  };
}
```

Monitoring:

- Discovery attributes under the `remote_cluster_throttlers_group`, especially `rate`, `limit`, `queue_byte_size`, `quota_exceeded`, and `period`.
- Cluster-throttler dashboards if installed.
- Remote-read operation throughput and waiting time.

Shortage and overcommit:

- Requests wait for throttler quota; queue size and estimated overrun grow.
- The correct reaction is usually to reduce remote-read concurrency, copy hot data locally, or increase the configured quota if the physical link has headroom.

## Master, proxy, and request-bandwidth resources

### Per-user request rates and queues

Metadata and control-plane requests are limited per user. Modern clusters distinguish read and write request rate limits; some documentation and older clusters expose a generic `request_rate_limit`. Queue size is controlled separately.

Common attributes:

```bash
yt get //sys/users/my_user/@read_request_rate_limit
yt get //sys/users/my_user/@write_request_rate_limit
yt get //sys/users/my_user/@request_queue_size_limit
```

Configuration:

```bash
yt set //sys/users/my_user/@read_request_rate_limit 300
yt set //sys/users/my_user/@write_request_rate_limit 200
yt set //sys/users/my_user/@request_queue_size_limit 1000
```

Consumption controls:

- Batch small metadata requests with `execute_batch`, but do not create unbounded batches.
- Avoid recursive listings of large subtrees on hot paths.
- Cache stable metadata in applications.
- Use fewer high-level polling loops and exponential backoff on rate-limit errors.
- Avoid creating thousands of small objects in tight loops.

Monitoring:

- User attributes: request rate, request-rate limits, request-queue limit.
- Master/proxy metrics for user request rate, queue size, queue limit, and rate-limit errors.
- Client errors such as request rate exceeded or request queue size exceeded.

Shortage and overcommit:

- Requests may be throttled, queued, retried by clients, or rejected.
- Raising limits without checking master CPU can move the bottleneck from clients to the master automaton.

### Master global throttlers and read complexity

Master object-service reads and writes can also be protected by global throttlers and complexity limits. Read complexity limits track at least two dimensions: nodes visited and total result bytes.

Configuration and controls:

- Global read/write request throttlers in master dynamic config.
- `enable_read_request_complexity_limits`.
- `default_read_request_complexity_limits.node_count`.
- `default_read_request_complexity_limits.result_size`.
- `max_read_request_complexity_limits` for caller-requested overrides.

Monitoring:

- Master request latency by method.
- Automaton CPU, mutation queue, read request queues.
- Errors indicating complexity limits, rate limits, or queue-size limits.

Shortage and overcommit:

- Oversized reads fail fast instead of consuming unbounded master CPU/memory.
- Mutations may queue behind automaton work; write latency grows.
- Snapshot and changelog IO/memory pressure can increase tail latency even when request RPS is unchanged.

### RPC service queues and thread pools

Every component has RPC services with queue size limits, worker thread pools, and timeouts. These are not user-facing quotas, but they are capacity limits that strongly affect overload behavior.

Control layers:

- Component static/dynamic config for RPC server queues, service capacities, thread pools, and timeouts.
- Proxy pool sizing and load balancing.
- Client-side timeouts, retries, and concurrency.

Monitoring:

- RPC request queue size and queue size limit.
- Request latency histograms by service/method.
- Error counters for queue-size limit exceeded and timeout.

Shortage and overcommit:

- Queues grow first, then requests time out or fail.
- Aggressive client retries can form a retry storm; use jittered backoff and respect retry-after intervals.

## Transactions, locks, and metadata lifetime

Transactions consume master memory, lock table entries, ping bandwidth, and automaton CPU. Long-lived transactions also keep uncommitted resource usage visible in account `@resource_usage`.

Controls:

- Transaction timeout and ping period.
- Application discipline: short transactions, small write sets, bounded nesting, cleanup on failures.
- Account quotas for resources consumed inside active transactions.

Monitoring:

- Active transaction count and age.
- Locked node count and lock conflicts.
- Account `resource_usage` versus `committed_resource_usage`.
- Master memory and automaton CPU.

Shortage and overcommit:

- Lock conflicts block writers or readers depending on lock mode.
- Resource usage inside transactions can cause account quota violations before commit.
- Very large transactions increase commit latency and recovery work.

## Caches and memory pools

Caches are capacity-limited resources even when they are not exposed as user quotas. Examples include block caches, chunk metadata caches, tablet lookup/row caches, schema/object caches, and client-side caches.

Controls:

- Component cache sizes and TTLs in dynamic or static config.
- Table design: in-memory mode, chunk size, compression, sorted-key locality.
- Workload behavior: repeated point lookups vs scans, hot-key distribution.

Monitoring:

- Cache hit/miss rate.
- Cache memory usage and eviction rate.
- Downstream disk/network/master load after cache misses.

Shortage and overcommit:

- Low hit rate increases backend IO and latency.
- Oversized caches can steal memory from active serving and trigger pressure elsewhere.

## Job stderr, logs, traces, and diagnostic output

Logs and traces consume disk, IO bandwidth, CPU, and sometimes network bandwidth. Diagnostic output from jobs can also fill local disks and operations-archive tables.

### Job stderr and debug artifacts

User job stderr is a controlled resource in three dimensions: how much is collected from one job, how many job stderrs are retained for an operation, and how long archive rows remain available. The operation root option `max_stderr_count` limits the number of saved stderrs per job type; the task-level `max_stderr_size` limits bytes collected from one job, and excess stderr is ignored. Debug chunks such as stored stderr and failed-job input context are charged to `job_node_account`.

| Control | Scope | Effect |
|---|---|---|
| `max_stderr_size` | Task/user job spec | Limits bytes collected from one job's stderr; the default is 5 MB and the configured value is capped. |
| `max_stderr_count` | Operation root | Limits how many job stderrs are saved per job type; the usual default is 10 and the maximum accepted value is 150. |
| `stderr_table_path` | Operation root | Writes complete stderr for completed jobs to a dedicated table created by the user; use this when normal bounded retention is insufficient. |
| `job_node_account` | Operation root | Account that stores debug chunks such as stderr and failed-job input context. |
| `archive_ttl` | User job spec / archive path | Controls how long job rows, including stderr references, remain in the operations archive when TTL application is enabled. |

Retention is controlled separately from collection. The operation/job spec can carry `archive_ttl`, and controller-agent configuration can enable applying this TTL to job archive rows. After archive retention expires, `get_job_stderr` may no longer find the data in the operations archive, even if the operation itself is still visible elsewhere.

Monitoring:

- Jobs tab and `get_job_stderr` for sampled retained stderr.
- Job attributes such as `stderr_size` and filters for jobs with stderr.
- Operations archive size, cleanup lag, and account usage for `job_node_account`.
- Dedicated stderr table size when `stderr_table_path` is used.

Shortage and overcommit:

- A noisy job can have stderr truncated at `max_stderr_size`.
- If more jobs produce stderr than `max_stderr_count`, only a bounded subset is retained in the normal job stderr path.
- If the operations archive expires rows or cleanup catches up, historical stderr becomes unavailable through archive reads.
- Excessive stderr writing can slow jobs and consume local slots disk before upload; redirect or rate-limit application logs if stderr becomes a data stream.

### Component logs and traces

Controls:

- Logging category levels and category rate limits.
- Log rotation and retention.
- Component log disk quotas and retention.
- Sampling for tracing and profiling.

Monitoring:

- Log write rate and dropped/rate-limited log counters.
- Component disk usage.
- Job stderr size and artifact upload failures.

Shortage and overcommit:

- Logs can be dropped by rate limiters.
- Local disk exhaustion can destabilize nodes or cause job failures.
- Excessive debug logging can become a hidden CPU and IO bottleneck.

## How the system reacts to shortage

{{product-name}} uses different reactions depending on the resource and the layer:

1. **Admission refusal**: deny a new write, node creation, tablet creation, or request when a hard quota or complexity limit is exceeded.
2. **Throttling**: delay requests until bandwidth/RPS tokens are available.
3. **Reduced scheduling**: keep operations or jobs pending until fair-share resources are available.
4. **Preemption**: abort younger/preemptible allocations so starving operations or hard limits can be satisfied.
5. **Job abort/restart**: kill jobs that exceed memory, disk, or node resource constraints; restart if operation policy permits.
6. **Background slowdown**: reduce compaction, replication, repair, or balancing when their throttlers or physical IO are saturated.
7. **Write disabling**: stop accepting writes on a node, medium, tablet, or table path when memory/disk watermarks are unsafe.
8. **Degraded latency**: queues grow and tail latency rises before a hard failure is emitted.

## Overcommit checklist

When a resource is overcommitted, use this sequence:

1. **Identify the owner plane**: scheduler, account, bundle, master/proxy, data node, or external link.
2. **Compare usage and configured limits**: `@resource_usage` vs `@resource_limits`, scheduling usage vs fair share, throttler `rate` vs `limit`, queue size vs queue limit.
3. **Find the dominant consumer**: operation, account subtree, table, bundle, user, RPC method, node, or remote cluster.
4. **Choose a reaction**:
   - lower concurrency if queues or retries are growing;
   - increase quota only if physical capacity and upstream/downstream planes have headroom;
   - move workload to another pool/tree/medium/bundle;
   - compact/merge/clean up if metadata or chunk count is the issue;
   - shard/reshard if a tablet or queue partition is hot;
   - cache or batch requests if master request bandwidth is the issue.
5. **Watch for shifted bottlenecks**: after raising CPU, memory may dominate; after raising request RPS, master automaton CPU may dominate; after raising remote bandwidth, local disk writes may dominate.

## Useful commands

```bash
# Account limits and usage
yt get //sys/accounts/<account>/@resource_limits
yt get //sys/accounts/<account>/@resource_usage
yt get //sys/accounts/<account>/@recursive_resource_usage
yt get //sys/accounts/<account>/@violated_resource_limits

# Pool limits and guarantees
yt get //sys/pool_trees/<tree>/<pool>/@strong_guarantee_resources
yt get //sys/pool_trees/<tree>/<pool>/@resource_limits
yt get //sys/pool_trees/<tree>/<pool>/@max_running_operation_count

# Exec-node job capacity and distribution
yt get //sys/exec_nodes/<node-address>/@resource_limits
yt get //sys/exec_nodes/<node-address>/@resource_usage
yt get //sys/exec_nodes/<node-address>/@tags
yt get //sys/exec_nodes/<node-address>/@alerts

# User request limits
yt get //sys/users/<user>/@read_request_rate_limit
yt get //sys/users/<user>/@write_request_rate_limit
yt get //sys/users/<user>/@request_queue_size_limit

# Bundle resources
yt get //sys/tablet_cell_bundles/<bundle>/@resource_limits

# Inter-cluster throttlers
yt get //sys/cluster_throttlers
```

## See also

- [Scheduler and pools](../user-guide/data-processing/scheduler/scheduler-and-pools.md)
- [Setting up pool trees](../user-guide/data-processing/scheduler/pool-settings.md)
- [Preemption](../user-guide/data-processing/scheduler/preemption.md)
- [Operation options](../user-guide/data-processing/operations/operations-options.md)
- [Quotas](../user-guide/storage/quotas.md)
- [Accounts](../user-guide/storage/accounts.md)
- [Input/output settings](../user-guide/storage/io-configuration.md)
- [Bundle controller](../admin-guide/bundle-controller.md)
- [Inter-cluster network bandwidth throttling](../admin-guide/cluster-throttlers.md)
- [CRI for Job Container Runtime](../admin-guide/node-cri.md)
- [Getting the YTsaurus specification ready](../admin-guide/prepare-spec.md)
