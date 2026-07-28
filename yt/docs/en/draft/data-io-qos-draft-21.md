<!--
Draft number: 21
Author: AI agent (GitHub Copilot)
Created: 2026-07-21
Status: In progress
Target: user-guide/data-io-qos
-->

# Data I/O QoS for mixed low-latency and batch workloads

This draft describes how to run latency-sensitive services and heavy batch processing on the same {{product-name}} cluster without letting one class of traffic starve the other. The focus is data I/O quality of service: table layout, client behavior, cluster-side throttling, and monitoring.

The article intentionally treats QoS as an end-to-end discipline. No single switch can guarantee low tail latency if a table is badly shaped, clients retry aggressively, background compaction has no headroom, and batch jobs read the same disks at full speed.

## Target scenario

A typical shared cluster has at least two workload classes:

* **Low-latency serving**: point lookups, short range reads, small writes, queue consumers, and online dashboards. The goal is stable p95/p99 latency and predictable error rate.
* **Batch and analytical load**: scans, large writes, compaction-heavy ingestion, exports, `Map`, `Reduce`, `Sort`, `RemoteCopy`, CHYT/SPYT/YQL queries, and backfills. The goal is high aggregate throughput, but these jobs can usually tolerate queuing and lower priority.

The QoS goal is not to make batch invisible. The practical goal is to reserve enough disk, network, CPU, memory, request-queue, and background-maintenance capacity so that batch work slows down before online traffic becomes unstable.

## Concrete control planes

Use the following levers first; they map directly to resource contention.

| Lever | What it isolates or shapes | Concrete use in a mixed cluster |
| --- | --- | --- |
| `replication_factor` / erasure codec | Read fan-out options, recovery traffic, disk usage | Use `replication_factor = 3` for hot tables when possible: each chunk has three independent read sources, so random reads are spread across more disks and one slow replica hurts less. `replication_factor = 2` saves space but gives fewer placement choices and less read load balancing. Erasure coding saves space for cold scans, but a degraded or reconstructed read may touch more nodes and add CPU/network cost. |
| `primary_medium` and `media` | Disk class and per-medium queues | Put serving chunks on SSD/NVMe media; keep backfills, archival tables, and scan-heavy snapshots on cheaper media. Set the attribute on a directory so new chunks inherit it. |
| `tablet_cell_bundle` | Tablet nodes, tablet cells, memory, lookup/select/write thread pools | Put strict-SLO dynamic tables into a serving bundle. Do not mount experimental ingestion tables in that bundle. |
| RPC proxy `role` | Proxy CPU, connection pools, request queues, and proxy-level monitoring | Use separate proxy roles such as `serving`, `interactive`, and `batch`; configure clients to discover or connect to the matching role. This prevents batch scans from filling the same proxy queues used by point lookups. |
| Workload category | Data-node request queues and fair throttler buckets | Mark interactive and batch reads differently where the client/API supports workload descriptors. Per-workload queues and fair throttlers can then protect interactive reads from scan traffic. |
| User and account | ACLs, quotas, attribution, emergency kill switch | Use separate users/accounts for serving services, batch pipelines, and maintenance robots. This gives separate disk quotas, clearer dashboards, and a way to suspend or throttle one class without blocking another. |
| Scheduler pool | Job CPU, memory, user slots, and operation admission | Put batch operations into capped pools. Keep serving-adjacent operations in a small protected pool with strict admission control. |
| Data-node / location throttlers | Disk and network bandwidth by traffic kind | Cap batch, local-job, replication, repair, compaction, and flush categories below the point where disk queues hurt p99. |
| Inter-cluster throttlers | Remote read and `RemoteCopy` bandwidth | Limit cross-cluster batch reads so remote traffic slows down instead of saturating links used by online replication or serving. |

## Table tuning

### Replication, erasure, and read bandwidth

Replication factor is a QoS setting, not only a durability setting.

* With replicated chunks, a read can be served from any available replica. Increasing `replication_factor` from 2 to 3 increases disk usage, write traffic, and repair work, but it also gives the system more choices for placing reads and balancing load across data nodes and racks.
* For hot small reads, `replication_factor = 3` is usually preferable to erasure coding: the system can choose one nearby/healthy replica, and the read path is simple.
* For cold batch scans, erasure coding is often acceptable because storage efficiency matters more than per-request latency. Keep in mind that degraded erasure reads and repair can consume extra CPU, network, and disk bandwidth.
* Do not put a hot serving table and a large erasure-coded scan table on the same slow medium unless throttlers and scheduler pools are already limiting the scan.
* If a table is hot and has only two replicas, first check whether p99 is caused by replica placement or overloaded locations before adding tablet nodes.

### Choose the storage layout for the request shape

* Use sorted dynamic tables for key-value and bounded range reads.
* Keep hot lookup tables in `optimize_for=lookup` unless scan efficiency is more important than point-read latency.
* Use `optimize_for=scan` for wide analytical tables and snapshots scanned by batch jobs.
* Keep dynamic-table blocks small enough for point reads. Oversized blocks make a single-row lookup read and decompress too much data.
* Prefer fast compression for serving tables; prefer stronger compression only when CPU is not on the critical path.
* Rewrite static tables before converting them to dynamic tables if their chunks or blocks are too large for dynamic-table reads.

### Keep hot and cold data physically separate

* Put hot rows and recent versions into serving tables or recent partitions.
* Move history to snapshot/static tables on a batch medium.
* Use TTL and `trim_rows` for ordered tables so obsolete rows stop consuming chunks and compaction/scan work.
* Do not run large scans over a mounted hot table when a frozen table, snapshot, replica, or export is sufficient.

### Shard for request rate

* Pick tablet count from peak RPS, write rate, and hot-key distribution, not only from bytes.
* Split hot sorted-table key ranges; adding nodes does not fix a single hot tablet.
* Create enough ordered-table tablets for producer and consumer parallelism.
* Use tablet balancing groups when tables in one bundle have different load profiles.
* Monitor the hottest tablet, not only bundle totals.

### Reserve maintenance headroom

Serving p99 depends on background work staying current.

* Keep dynamic-store memory below sustained pressure so writes can flush without emergency behavior.
* Leave bandwidth for flush, compaction, partitioning, replication, and repair.
* Treat compaction backlog as future read latency. Large write bursts can hurt reads later even if the write path looked healthy.
* For read-mostly in-memory tables, mount frozen where possible to reduce maintenance overhead.

### Use bundles and media deliberately

* Put strict-SLO dynamic tables into a dedicated `tablet_cell_bundle`.
* Size lookup/select/write thread pools for the actual serving mix.
* Keep enough spare tablet nodes for failover; otherwise a single node loss can collapse isolation.
* Put serving chunks on SSD/NVMe media and batch/archive chunks on cheaper media.
* Set `primary_medium` on directories used by each workload class.

## Client setup

### Route clients to separate proxy roles

Use separate RPC proxy roles for different traffic classes:

```text
serving clients      -> RPC proxies with role = serving
interactive queries  -> RPC proxies with role = interactive
batch pipelines      -> RPC proxies with role = batch
maintenance robots   -> RPC proxies with role = maintenance
```

This protects proxy CPU, connection pools, request queues, and dashboards. If a batch client opens thousands of long scan requests, it should fill the `batch` proxy role first, not the serving proxies. Also restrict who may use serving proxy roles so accidental batch jobs cannot bypass the split.

### Set workload category on requests

Where the client or API exposes workload descriptors/categories, set them consistently:

* point lookups and user-facing range reads: interactive/user category;
* scheduled scans and backfills: batch category;
* compaction-style user maintenance: maintenance/background category.

This matters because data-node request queues and fair throttlers can be split by workload category. If all clients use the default/uncategorized category, the cluster cannot distinguish a dashboard lookup from a backfill scan.

### Use separate users and accounts

Do not run all pipelines under one robot.

* Use one user/account for serving writes and serving-owned tables.
* Use separate users/accounts for batch ingestion, ad-hoc analytics, exports, and maintenance.
* Give batch accounts explicit disk quotas on batch media; do not let them consume serving SSD quota.
* Use user/account tags in dashboards and alerts.
* In an incident, suspend or reduce limits for the batch user/account instead of disrupting serving.

### Bound concurrency and retries at the source

For serving clients:

* set per-process in-flight request limits;
* use short deadlines close to the user SLO;
* use a small retry budget with exponential backoff and jitter;
* avoid fan-out to many tablets in one synchronous user request.

For batch clients:

* cap scan readers and table writers;
* split backfills into resumable ranges;
* add pauses between waves;
* keep operation job counts and bytes-per-job consistent with the batch I/O budget.

Timeouts must not create retry storms. If p99 rises, multiplying requests usually makes every shared queue worse.

### Make scans cooperative

* Read only needed columns and key ranges.
* Prefer snapshots, frozen tables, or replicas over mounted hot tables.
* Use sampling for exploration.
* Schedule known heavy scans outside peak serving windows.
* Respect inter-cluster bandwidth throttlers for remote reads.

## Cluster tuning

### Scheduler pools

Use pools as admission control for jobs:

* `prod_serving`: small, protected, strict admission, only serving-adjacent operations.
* `prod_interactive`: bounded query/ad-hoc work.
* `prod_batch`: large but capped share for scans, backfills, exports.
* `prod_maintenance`: admin-controlled repair/migration work.

Set caps so `prod_batch` cannot consume all user slots or CPU even when it has unlimited pending work. Keep emergency backfills out of serving pools.

### Data-node, location, and network throttlers

Configure limits below the saturation point, not at theoretical device bandwidth. Watch disk queues while tuning.

Concrete reservations to decide per medium/node:

* minimum bandwidth left for serving reads;
* minimum bandwidth left for tablet store flush;
* minimum bandwidth left for compaction/partitioning;
* minimum bandwidth left for replication and repair;
* maximum bandwidth for local jobs / batch scans;
* maximum remote-read bandwidth per source cluster.

Use per-location throttlers when SSD and HDD are present in the same cluster: an HDD scan should not define the safe limit for SSD serving traffic, and SSD serving should not hide HDD queue growth.

### Tablet-node isolation

For a serving bundle:

* allocate enough tablet nodes and keep spares;
* allocate lookup/select/write threads according to real traffic;
* keep `tablet_dynamic` memory large enough for write bursts;
* keep in-memory table limits separate from dynamic-store memory;
* move bulk ingestion and experiments to another bundle before load tests.

If Bundle controller is enabled, make these changes through bundle target config so node count, memory categories, and thread pools stay consistent.

### Inter-cluster links and recovery

Remote reads and `RemoteCopy` need explicit throttling. Without it, a backfill from another cluster can look like local batch work until the network link is saturated.

Reserve recovery bandwidth even on busy clusters. Replication and repair are not optional: if they fall behind after a disk/node failure, the cluster remains exposed to the next failure longer. Do not use all remaining bandwidth for foreground batch just because serving p99 is currently green.

## Monitoring

### Dashboards by isolation dimension

Every dashboard should be splittable by:

* table and tablet;
* tablet cell bundle;
* medium and location;
* RPC proxy role;
* workload category;
* user and account;
* scheduler pool and operation id;
* remote cluster for remote reads.

If a graph cannot answer "which class consumed the I/O budget?", add the missing tag or split.

### Signals to alert on

* serving p95/p99 latency, timeout rate, and throttling errors;
* proxy and RPC request queue size by proxy role;
* data-node read/write queue size and latency by medium/location;
* throttler rate, overdraft duration, and queue size by workload category;
* hottest tablets by read RPS, write RPS, and data weight;
* dynamic-store memory, flush backlog, compaction backlog, partitioning backlog;
* replication and repair backlog;
* scheduler pool fair-share usage, pending jobs, and top operations by bytes read/written;
* account quota usage by medium.

Alert on debt before SLO violation: compaction backlog, flush backlog, and repair backlog are often the warning that the next batch wave will hurt serving latency.

### Quick diagnosis matrix

| Symptom | Check first | Typical action |
| --- | --- | --- |
| Serving p99 rises; batch operation just started | proxy role queues, workload-category throttlers, disk queues | Pause/lower batch pool; lower batch/local-job throttler; move scan to snapshot. |
| One table has high p99; bundle totals look fine | hottest tablets and pivot ranges | Split/reshard hot tablets; fix key distribution. |
| Reads slow only on one medium | per-location disk queues and throttler overdraft | Lower scans on that medium; move hot table to SSD; increase replicas if read placement is too narrow. |
| Writes time out during ingestion | dynamic-store memory and flush backlog | Reduce writer concurrency; add serving-bundle memory/flush capacity; stage batch writes. |
| RemoteCopy hurts local users | inter-cluster throttler state and remote bandwidth | Reduce remote bandwidth limit or pause RemoteCopy pool. |
| Repair backlog grows during peak | repair throttlers and batch caps | Reserve more repair bandwidth; reduce foreground batch until redundancy is restored. |

## Rollout checklist

1. Inventory all major readers and writers and assign each to a workload class.
2. Move strict-SLO dynamic tables into dedicated bundles and appropriate media.
3. Put batch operations into explicit scheduler pools with capped fair-share and concurrency.
4. Set client-side concurrency, timeout, and retry profiles for serving and batch separately.
5. Configure data-node, network, per-location, and inter-cluster throttlers to reserve online and recovery headroom.
6. Build dashboards that join service latency with table, bundle, data-node, scheduler, and throttler metrics.
7. Run a controlled batch load test during normal serving traffic.
8. Tune limits until batch slows down before serving p99 exceeds the SLO.
9. Document emergency actions: pause pools, lower throttlers, disable a pipeline, move tablets, or stop remote reads.

## Emergency playbook

When online latency regresses during batch load:

1. Confirm the affected commands and tables.
2. Check whether one tablet, one bundle, one data-node location, or one scheduler pool is hot.
3. Reduce or pause the largest batch operations in the affected pool.
4. Lower relevant batch/data-node/inter-cluster throttler limits if queues keep growing.
5. Stop retry storms by tightening client retry budgets or shedding optional traffic.
6. If the issue is a hot tablet, split or reshard; if it is a hot bundle, move non-critical tables away or add bundle capacity.
7. After recovery, compare the incident timeline with dashboards and add the missing alert or limit.

## Common anti-patterns

* Using one default scheduler pool for everything.
* Allowing clients to retry indefinitely on timeouts.
* Running backfills directly against the newest hot dynamic table.
* Storing serving and archival data on the same medium with the same compression and replication policy.
* Increasing batch job counts when throttler queues are already growing.
* Treating compaction, flush, and repair as optional background work with no reserved resources.
* Measuring only average bandwidth and ignoring queues, p99 latency, and hot tablets.
* Sharing a tablet cell bundle between strict-SLO tables and experimental ingestion pipelines.

## Summary

Successful mixed-use clusters combine isolation for the workloads that cannot wait with throttling for the workloads that can. Start with table layout and client behavior, because cluster-side throttlers cannot fully compensate for hot tablets or retry storms. Then reserve capacity with bundles, media, scheduler pools, and data-node throttlers. Finally, monitor debt and queueing so that operators see contention before users see timeouts.
