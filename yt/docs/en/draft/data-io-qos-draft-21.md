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

## Mental model

Data I/O in {{product-name}} passes through several shared resources:

1. **Client-side concurrency**: request fan-out, retries, timeouts, and operation job counts.
2. **Proxy and RPC queues**: HTTP/RPC proxy request queues and tablet/data-node RPC queues.
3. **Tablet nodes**: lookup/select/write thread pools, tablet cells, dynamic-store memory, preload memory, and compaction/flush execution.
4. **Data nodes and media**: per-node and per-location disk queues, network throttlers, chunk readers/writers, repair and replication jobs.
5. **Scheduler pools**: operation CPU, memory, user slots, and operation-level I/O intensity.
6. **Background work**: flush, compaction, partitioning, tablet balancing, chunk replication, repair, merge, and cleanup.

QoS is achieved by combining three techniques:

* **Isolation** where contention is too risky: separate tablet cell bundles, media, scheduler pools, accounts, or even node tags for critical workloads.
* **Shaping** where sharing is acceptable: throttlers, fair-share pool guarantees, operation limits, client concurrency limits, and request timeouts.
* **Feedback** from monitoring: tune based on saturation, queueing, and latency signals rather than on average bandwidth alone.

## Table tuning

### Choose the storage layout for the access pattern

For latency-sensitive tables, optimize the table for the dominant request shape.

* Use sorted dynamic tables for key-value and bounded range reads.
* Keep hot lookup tables in `optimize_for=lookup` unless scan efficiency is more important than point-read latency.
* Use `optimize_for=scan` for wide analytical tables where batch reads select large ranges or many rows.
* Avoid oversized blocks in dynamic tables. Dynamic-table reads pay the cost of reading and decompressing blocks; blocks that are too large amplify single-row reads.
* Prefer faster compression codecs for hot serving data. Stronger compression may save disk and network, but it can move the bottleneck to tablet-node or job CPU.
* Use erasure coding primarily for colder or throughput-oriented data. Hot low-latency tables usually benefit from ordinary replication because reads have fewer reconstruction and placement constraints.

For dynamic tables converted from static tables, rewrite or compact them with dynamic-table-friendly chunk and block sizes before opening serving traffic. Large static-table chunks can produce poor tablet distribution, excessive read amplification, and slow preloads.

### Keep hot and cold data apart

Mixing hot serving rows and cold history in the same physical layout makes every maintenance process more expensive.

* Partition by time, tenant, or another natural dimension if hot and cold data have different latency requirements.
* Move old partitions to a colder medium or stronger compression policy.
* Use TTL and trimming so that obsolete versions and old ordered-table rows stop consuming disk, chunk count, compaction, and lookup cost.
* Avoid running large analytical scans directly over the newest serving table when a periodically frozen or exported snapshot is sufficient.

### Shard for load, not only for size

A table with enough bytes per tablet can still be under-sharded for request rate.

* Choose tablet count from peak read/write request rate, hot-key distribution, and expected background work.
* For sorted tables, verify that pivot keys spread hot ranges across tablets.
* For ordered tables, create enough tablets for producer and consumer parallelism.
* Use tablet balancing groups when several tables in the same bundle have different load profiles.
* Do not rely on adding nodes to fix a single hot tablet. A hot tablet remains hot until the key range or queue shard is split.

### Reserve maintenance headroom

Low-latency reads depend on background work keeping up.

* Keep dynamic-store memory below sustained pressure so writes can flush smoothly.
* Leave disk and network headroom for flush, compaction, partitioning, and replication.
* Watch write amplification: large write bursts can create delayed compaction debt that hurts reads later.
* Tune compaction and partitioning conservatively for serving tables; an aggressive setting can reduce debt faster but may steal the very I/O you are trying to protect.
* For read-mostly in-memory tables, mount frozen when possible to reduce maintenance overhead.

### Put critical tables in the right bundle and medium

Use a dedicated tablet cell bundle for online serving when the workload has strict SLOs. A bundle gives you a separate set of tablet cells and configurable tablet-node resources. With Bundle controller, you can allocate tablet nodes, memory categories, and lookup/select/write thread-pool sizes per bundle.

Use storage media deliberately:

* Put latency-sensitive chunks on SSD/NVMe media if the cluster has multiple media.
* Keep large batch and archival tables on HDD or lower-priority media when possible.
* Set `primary_medium` at a directory boundary so new chunks inherit the intended placement.
* Ensure accounts have enough quota on the chosen medium; quota failures often appear as application errors during peak load.

## Client setup

### Classify traffic explicitly

Every producer, consumer, and batch pipeline should know its workload class. At minimum, separate configuration profiles for:

* `serving`: small requests, short deadlines, bounded retries, limited concurrency.
* `interactive`: human-triggered queries and dashboards, moderate deadlines, moderate concurrency.
* `batch`: scans, backfills, exports, and large writes, long deadlines, throttled concurrency.
* `maintenance`: compaction backfills, migrations, verification, repair-oriented user jobs.

Give each profile different proxy addresses, scheduler pools, operation specs, retry budgets, and observability labels where the client library supports them.

### Bound concurrency at the source

A shared cluster cannot protect low latency if every client treats timeouts as a signal to multiply traffic.

For serving clients:

* Use small connection pools and explicit in-flight request limits per process.
* Prefer batching that reduces overhead without creating large tail-latency outliers.
* Set request deadlines close to the user-facing SLO and fail fast when the answer is no longer useful.
* Use exponential backoff with jitter and a small retry budget.
* Avoid unbounded parallel lookups over many tablets from one request path.

For batch clients:

* Limit reader and writer parallelism before submitting work.
* Split large backfills into resumable ranges with pauses between waves.
* Use scheduler pools with low weight or no strong guarantees for opportunistic work.
* Use operation-level job counts, data-size-per-job settings, and pool limits to keep aggregate I/O below the reserved batch budget.

### Make scans cooperative

Batch scans are the most common cause of serving-latency regressions on shared storage.

* Prefer reading snapshots, frozen tables, or replicated copies instead of mounted hot tables.
* Read only required columns and key ranges.
* Avoid small random reads from many jobs when a sequential scan or pre-aggregation would do.
* Use sampling for exploratory queries.
* Schedule large scans in windows where serving traffic has known headroom.
* For remote reads, enable and respect inter-cluster bandwidth throttling.

### Separate writes by durability and freshness needs

Low-latency writes and batch ingestion have different optimal shapes.

* Keep serving writes small, evenly distributed, and deadline-bound.
* Buffer batch ingestion outside {{product-name}} or in staging tables when possible, then merge or publish in controlled waves.
* Avoid large append storms to the same ordered-table tablet.
* For dynamic tables, monitor flush debt before increasing batch write concurrency.

## Cluster tuning

### Use scheduler pools as the first line of defense

Batch operations should be assigned to explicit scheduler pools. Configure pool guarantees and limits so that online-adjacent work has enough CPU and user slots even during large backfills.

Recommended pool pattern:

* `prod_serving`: strong guarantees, strict admission control, used only by latency-sensitive operations.
* `prod_interactive`: moderate guarantees, bounded concurrency.
* `prod_batch`: large but capped share, preemptible/opportunistic where possible.
* `prod_maintenance`: controlled by administrators, often low weight but allowed during planned windows.

Do not put emergency backfills or one-off analytical jobs into the same pool as serving operations just because they are owned by the same team.

### Configure data-node and network throttlers

Cluster nodes expose network and data-node throttlers for different traffic categories. Use them to cap background and batch categories below physical disk and network capacity.

A practical starting point is to reserve a fixed percentage of per-node bandwidth for online traffic and recovery, then give batch the remaining budget. The exact ratio depends on hardware, but the important rule is that throttler limits must be lower than the point where disk queues and RPC queues grow without bound.

Pay special attention to:

* total in/out network throttlers;
* per-category data-node throttlers for replication, repair, merge, tablet compaction/partitioning, tablet store flush, local jobs, and cache traffic;
* per-location throttlers when one medium is much slower than another;
* request-queue size limits that reject instead of letting latency grow indefinitely.

### Isolate tablet-node resources

For dynamic-table serving, tablet-node isolation is often more important than raw disk bandwidth.

* Create dedicated bundles for strict SLO tables.
* Allocate enough lookup, select, and write threads for the online bundle.
* Keep memory limits for dynamic stores and in-memory tables realistic.
* Maintain spare tablet nodes so failover does not collapse isolation.
* Avoid mounting experimental or bulk-ingestion tables into the same bundle as critical online tables.

If Bundle controller is enabled, manage these settings through bundle target configs instead of hand-editing individual nodes.

### Protect inter-cluster links

Remote reads and `RemoteCopy` can saturate links in ways that local per-node throttlers do not fully express. Configure cluster throttlers for remote-source traffic and keep their state visible to operators. Batch pipelines that read from another cluster should be designed to slow down when the remote-bandwidth budget is exhausted.

### Keep recovery bandwidth available

Do not allocate 100% of disk and network capacity to foreground work. Replication and repair are part of the cluster's safety margin. If repair cannot keep up after a disk or node failure, the cluster may remain exposed to a second failure for too long.

Reserve enough background bandwidth for:

* chunk replication and repair;
* tablet store flush;
* tablet compaction and partitioning;
* tablet snapshots and changelogs;
* decommission and medium balancing.

## Monitoring

### SLO dashboards

For each critical table or service, maintain a dashboard with:

* request rate by command and workload class;
* p50/p95/p99 latency and timeout rate;
* error rate by error code, especially throttling, queue overflow, unavailable tablet, memory pressure, and timeout errors;
* in-flight request count and client-side retry count;
* top tables, tablets, nodes, and users by traffic.

The dashboard should show both user-visible latency and internal wait time. If p99 latency rises while CPU and bandwidth averages look normal, look for queueing, hot tablets, overloaded locations, or retry amplification.

### Tablet and bundle health

For dynamic tables, watch:

* tablet count and distribution across cells;
* per-tablet read/write request rate and data weight;
* dynamic-store memory usage and flush backlog;
* compaction backlog, partitioning backlog, and overlapping-store indicators;
* lookup/select/write thread-pool saturation;
* tablet cell health, failed cells, and tablet moves;
* preload state for in-memory tables.

Alert on sustained imbalance, not only on hard failures. A single hot tablet can dominate p99 latency long before the bundle looks globally saturated.

### Data-node and medium saturation

For data I/O QoS, average node bandwidth is less important than queues and tail latency. Monitor:

* disk read/write bandwidth by medium and location;
* disk queue size and read/write latency;
* network in/out bandwidth and throttler overdraft duration;
* per-category throttler rates and queue sizes;
* chunk reader and writer errors;
* replication and repair backlog;
* cache hit rate for hot reads.

When a batch job starts, online latency should remain flat or degrade within the agreed budget. If latency rises with disk queues, lower batch I/O limits or move the batch data to another medium. If latency rises without disk queues, inspect tablet-node CPU, thread pools, and request queues.

### Scheduler and operation visibility

Batch owners need their own feedback loop:

* operation pool, weight, and fair-share usage;
* runnable and pending job counts;
* input and output bytes per second;
* job failures caused by throttling or unavailable bandwidth;
* remote-cluster bandwidth availability;
* top operations by read/write volume.

Operators should be able to answer: "Which operation consumed the I/O budget when serving latency regressed?" If this requires manual log archaeology, QoS incidents will last too long.

### Alerting strategy

Use layered alerts:

1. **User SLO alerts**: p99 latency, timeout rate, and error rate.
2. **Contention alerts**: disk queues, throttler overdraft, request queues, thread-pool saturation.
3. **Debt alerts**: compaction backlog, flush backlog, replication/repair backlog, unbalanced tablets.
4. **Capacity alerts**: medium free space, account quotas, node count, spare tablet nodes.

Debt alerts should fire before user SLO alerts. If the first signal is user-facing latency, the system is already operating without enough headroom.

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
