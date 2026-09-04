---
type: Draft Article
title: "Draft-18: Data storage failure handling and recovery"
last_modified: 2026-07-11T00:00:00Z
tags: [data-storage, failure-recovery, operations]
status: draft
---

# Data storage failure handling and recovery

This draft describes how {{product-name}} stores data, detects storage failures, restores lost redundancy, and where operators can observe or control the recovery path.

## Bottom-up view of the storage layers { #layers }

### Chunk locations

A **chunk location** is a directory on a data node disk that belongs to a medium. Locations are the smallest operational unit for storing chunk replicas: they have capacity, disk health, read/write queues, and per-location throttlers. If a location becomes unhealthy or unavailable, all chunk replicas stored in it become unavailable until the location returns or the master schedules recovery elsewhere.

A location has its own lifecycle, independent of the node lifecycle:

* **Enabled**: the location is initialized, accepts new chunk write sessions, serves reads, and can participate in replication and repair jobs.
* **Enabling**: the location is being initialized or resurrected. It is not yet a stable placement target.
* **Disabling**: the data node is draining local activity, cancelling location sessions, removing local chunk registrations from memory, stopping the health checker, and preparing a master heartbeat that makes replicas on this location unavailable.
* **Disabled**: the location is intentionally out of service. The node writes a disabled lock file in the location path so the state survives restart. Reads and new writes from that location are not expected to be served.
* **Destroying**: the operator or hot-swap workflow has requested permanent destruction after the location is disabled.
* **Destroyed**: destruction has finished and the disk can be replaced or recovered by the disk manager workflow.
* **Crashed**: the location failed during initialization and requires operator investigation.

The location health checker can schedule disablement automatically when disk checks fail, including cases where the underlying drive becomes effectively read-only for {{product-name}} writes. Operators can do the same manually by location UUID:

```bash
yt execute disable_chunk_locations '{node_address="<node-address>";location_uuids=["<location-uuid>"]}'
```

To undo an intentional disable after the disk is healthy again, resurrect the location:

```bash
yt execute resurrect_chunk_locations '{node_address="<node-address>";location_uuids=["<location-uuid>"]}'
```

For a disk that must be retired rather than returned to service, destroy the already disabled location:

```bash
yt execute destroy_chunk_locations '{node_address="<node-address>";location_uuids=["<location-uuid>"]}'
```

`destroy_chunk_locations` is irreversible for the local replicas on that location; use `resurrect_chunk_locations` only when the original data is still expected to be readable and the disable was temporary.

The disabled state is persisted by a lock file inside the location directory. On startup the data node checks location lock files before enabling locations; if the disabled lock file is present, the location remains disabled and the reason stored in the file is used for diagnostics. `resurrect_chunk_locations` removes this disabled lock file and reinitializes the location.

Data node locations are exposed in Cypress in several places:

* `//sys/chunk_locations` is the global virtual map of chunk location objects. Each object has state attributes such as `@uuid`, `@state`, `@node_address`, `@statistics`, and optional writable `@medium_override`.
* `//sys/chunk_location_shards/<shard>` contains the same chunk location objects partitioned by location UUID shard; this is useful when the global map is large.
* `//sys/cluster_nodes/<node-address>/@chunk_locations` is a data-node attribute keyed by location UUID. It contains the current master-side view of the node's chunk locations and their statistics. Use it when you already know the node address and want all its locations in one map.
* `//sys/cluster_nodes/<node-address>/@statistics/chunk_locations` contains the latest per-location statistics reported by that node in data node heartbeats. The legacy alias `@statistics/locations` may also be present.
* The data node Orchid exposes local runtime state under its location manager, including `store_locations`, but Orchid is node-local runtime diagnostics rather than master-owned Cypress metadata.

Typical inspection commands are:

```bash
yt get //sys/cluster_nodes/<node-address>/@chunk_locations
yt get //sys/cluster_nodes/<node-address>/@statistics/chunk_locations
yt get //sys/chunk_locations/<chunk-location-id>/@statistics
```

`@chunk_locations` and `@statistics/chunk_locations` expose the same per-location statistics shape; the former is keyed by location UUID, while the latter is a heartbeat statistics list.

Chunk location statistics fields are:

* `medium_name`: resolved medium name for the location's medium index, or an unknown-medium placeholder if the medium is absent.
* `available_space`: bytes still available on the location after watermarks and local accounting.
* `used_space`: bytes currently used on the location.
* `low_watermark_space`: bytes reserved by the low-watermark policy; when free space approaches this value, placement and writes become constrained.
* `chunk_count`: number of stored chunks currently accounted to the location.
* `session_count`: number of active write sessions on the location.
* `full`: whether the location is considered full for new placement.
* `enabled`: whether the location is enabled from the data node's point of view.
* `throttling_reads` and `throttling_writes`: whether local throttlers are currently limiting read or write traffic.
* `sick`: whether the location's I/O engine has observed sustained slow reads or writes. The flag is raised when read or write wait times stay above the configured `sick_read_time_threshold` or `sick_write_time_threshold` for the corresponding `sick_*_time_window`; it expires after `sickness_expiration_timeout`. A sick location is still present, but new/active write sessions are asked to close and operators should treat it as a degraded disk signal before full disablement.
* `disk_family`: disk family reported by the node, for example an HDD/SSD/NVMe class used by medium rules.
* `io_statistics`: nested I/O rates and capacities. Rates are byte-per-second deltas between counter samples; the data node refreshes them lazily no more often than `io_statistics_update_timeout`, which defaults to 10 seconds. The fields are:
  * `filesystem_read_rate` and `filesystem_write_rate`: logical bytes per second read/written through the location I/O engine.
  * `disk_read_rate` and `disk_write_rate`: physical block-device bytes per second from OS disk counters for the location device.
  * `disk_read_capacity` and `disk_write_capacity`: measured read/write throughput capacity from the data node I/O throughput meter. These are not current utilization; they are probe results used to estimate how much the disk can sustain.

### Media

A **medium** is a logical pool of chunk locations, usually mapping to a disk class such as HDD or SSD. Quotas, balancing, and replication requirements are computed independently per medium. Each chunk owner has a primary medium for new writes and may request additional replicas on other media through the `media` attribute. Erasure-coded objects may also use `data_parts_only` media to place only data parts on a faster or cheaper tier.

### Data nodes

A **data node** owns one or more chunk locations and serves reads, writes, replication traffic, and erasure repair traffic. It periodically reports its state to masters through data node heartbeats. Heartbeats include the node and location state, chunk counts, write-session-disablement flag, and incremental changes such as added, removed, or medium-changed chunks. Data nodes also execute chunk replica jobs: copying blocks to another node, reading blocks for repair, removing obsolete replicas, and honoring local disk and network throttlers.

### Node states and node maintenance flags

Node lifecycle state answers whether the master currently sees the process. Node maintenance flags answer what the cluster should do with that process while it is alive or temporarily absent. Keep these concepts separate from per-location state.

Important node states and flags for storage recovery are:

* **online**: the node is registered and heartbeating. It can still be unsuitable for writes if `disable_write_sessions` or `decommissioned` is set.
* **offline**: the node is not heartbeating. Replicas on it become unavailable unless the node is covered by `pending_restart`.
* **pending_restart**: a short expected outage. The master extends the node lease and treats replicas as temporarily unavailable to avoid excessive repair during rolling restarts.
* **disable_write_sessions**: the node refuses new chunk write sessions. Existing replicas remain readable, so this is a low-impact way to stop placing fresh data on a suspicious node or to drain writes before disk maintenance.
* **decommissioned**: the node is being evacuated. The master schedules work to move chunk replicas away and, for multi-role nodes, other subsystems may move their ownership away as well. Decommission is the right state for long maintenance or permanent removal.
* **banned**: the node is excluded more aggressively and should not be used for normal service; prefer narrower flags when only storage writes or only scheduler jobs must be drained.

Use maintenance requests instead of setting raw boolean attributes when possible, because requests leave an auditable reason and several requests can coexist:

```bash
yt add-maintenance --component cluster_node --type disable_write_sessions --address '<node-address>' --comment 'disk diagnostics'
yt add-maintenance --component cluster_node --type decommission --address '<node-address>' --comment 'host evacuation'
yt add-maintenance --component cluster_node --type pending_restart --address '<node-address>' --comment 'rolling restart'
```

Undo the requested state by removing the maintenance request, for example:

```bash
yt remove-maintenance --component cluster_node --address '<node-address>' --type disable_write_sessions --mine
```

For short planned restarts, use `pending_restart` rather than `decommissioned`. It lets the master distinguish temporarily unavailable replicas from truly lost capacity and avoids unnecessary replication storms, provided that maintenance is limited to a safe failure domain, typically one rack at a time.

### Health checks and state propagation

The storage control plane treats failures as a sequence of visibility changes:

* A disk or location health checker marks local storage unhealthy, failed, waiting for replacement, or effectively read-only; the data node then schedules location disablement.
* A data node may stop reporting because of process, host, network, or Kubernetes failure.
* The master node tracker eventually marks the node offline unless maintenance such as `pending_restart` intentionally extends its lease.
* Chunk replicas on offline nodes, disabled locations, destroyed locations, or unhealthy locations are excluded from the set of available replicas used by the chunk replicator.

### Master chunk host and chunk metadata

Masters store chunk metadata rather than chunk payloads. The chunk server and chunk host know which chunk owners reference a chunk, which media and replication factors are required, where replicas or erasure parts were last seen, and whether the chunk is vital. This metadata is the source of truth for deciding whether a chunk is healthy, underreplicated, overreplicated, data-missing, parity-missing, or lost vital.

### Replication queues and jobs

The master chunk replicator periodically compares the required chunk state with the currently available replicas. It then fills per-node replication queues and assigns repair jobs:

* **Push replication**: a source data node sends an existing replica to a target location.
* **Pull replication**: a target data node fetches a replica from a source node.
* **Erasure repair**: enough surviving parts are read to reconstruct a missing data or parity part, then the rebuilt part is written to a target location.
* **Removal jobs**: obsolete or overreplicated replicas are removed after metadata says they are no longer needed.

### Throttlers

Recovery uses the same disks and networks as user traffic. Data nodes therefore apply throttlers to reads, writes, replication, repair, and removal traffic. Cluster-level or inter-cluster throttlers may also limit background traffic. Throttlers protect foreground workloads, but overly tight limits increase mean time to repair and make the cluster more vulnerable to a second failure.

## Failure detection and recovery action flow { #action-flow }

The typical recovery path for a replicated chunk is:

1. **Replica becomes unavailable.** A location fails, a data node stops heartbeating, or a chunk disappears from an incremental heartbeat.
2. **Master refreshes chunk state.** The chunk server recomputes the set of stored, available, and last-seen replicas.
3. **Chunk enters a bad-state set.** The chunk is classified into one or more bad-state sets such as underreplicated or lost vital.
4. **Placement is selected.** The master chooses a target node and location in the required medium, respecting rack awareness, per-medium capacity, decommissioning/maintenance state, and replica-per-rack limits.
5. **Replication job is issued.** The source and target data nodes copy blocks. The job competes for data node CPU, network, disk bandwidth, and throttler tokens.
6. **Heartbeat confirms completion.** The target reports the new replica to the master. The chunk leaves the bad-state set after metadata is refreshed.
7. **Cleanup follows.** If the source later returns and the chunk has too many replicas, obsolete replicas enter removal queues and are deleted in the background.

Possible chunk states include:

* **Healthy**: the chunk has the required replicas or erasure parts on acceptable nodes, locations, racks, and media.
* **Underreplicated**: a replicated chunk has fewer available replicas than requested; it appears in `//sys/underreplicated_chunks`.
* **Overreplicated**: more replicas exist than required; extra replicas are removed in the background.
* **Unexpectedly overreplicated**: the master sees replicas that should not exist according to current metadata or placement decisions.
* **Parity-missing**: an erasure-coded chunk has lost a parity part. Data is usually readable and repairable, but redundancy is reduced.
* **Data-missing**: an erasure-coded chunk has lost a data part. Reads may still succeed if enough parts remain for decoding, but repair is urgent.
* **Quorum-missing or unrecoverable**: too many erasure parts are missing to reconstruct the chunk until a last-seen part returns.
* **Lost vital**: all available copies or enough erasure parts of vital data are missing; the chunk appears in `//sys/lost_vital_chunks` and requires incident response.
* **Inconsistently placed**: the chunk has enough replicas or parts, but they violate placement rules such as rack or medium constraints.

For erasure-coded chunks, the flow is similar to replication, but repair reads surviving parts, reconstructs the missing data or parity part, and writes it to a new target location.

### Repair priority and queue ordering { #repair-priority }

The master does not repair chunks in a single unordered list. It uses several queues and resource checks:

* **Replicated chunks** enter per-node push or pull replication queues when they are underreplicated, unsafely placed, or inconsistently placed. Queues are split by priority and scanned from the smallest priority number first. The priority is derived from the number of remaining replicas: chunks with fewer available replicas get lower priority numbers and are scheduled before chunks that still have more redundancy.
* **Erasure-coded chunks with missing parts** enter the `Missing` repair queue. This covers data-missing and parity-missing chunks where a part must be reconstructed from surviving parts.
* **Erasure-coded chunks with decommissioned parts** enter the `Decommissioned` repair queue. These are less urgent because the part still exists on a node being drained.
* The `Missing` queue is always considered before the `Decommissioned` queue, so real part loss is repaired before decommission-driven movement.
* Inside each repair-queue kind, work is split per medium. A decaying max-min balancer chooses the next medium so one medium cannot permanently starve others; scheduled repair data size is added as balancer weight and decays over time.
* A node receives repair jobs only while it has spare `repair_slots` and `repair_data_size` resources. Replication jobs similarly respect replication slots, replication data-size limits, pull-replication per-target limits, and misschedule limits.

This means the most urgent recovery path is: chunks with real missing data/parity and the least remaining redundancy first, then placement/decommission cleanup, all constrained by node resources, medium availability, and throttlers.

### Chunk object state attributes { #chunk-state-attributes }

For an individual chunk, inspect `//sys/chunks/<chunk-id>/@...`. The most useful state attributes are:

* `@stored_replicas`: currently known alive replicas. Replica entries include node address and attributes such as `medium`, `location_uuid`, `index` for erasure parts, `state` for journal replicas, `decommissioned` when the hosting node is decommissioned, and `offshore` for offshore media.
* `@stored_master_replicas`: replicas known from master metadata.
* `@stored_sequoia_replicas`: replicas known from Sequoia metadata when Sequoia chunk replica storage is used.
* `@last_seen_replicas`: last nodes where replicas or erasure parts were seen; use this during lost-vital incidents to identify hosts or disks worth recovering.
* `@unapproved_sequoia_replicas`: Sequoia replicas that are not yet approved in the regular replica view.
* `@replication_status`: aggregated status computed by the chunk replicator. This is the fastest way to see whether the chunk is considered underreplicated, overreplicated, lost, data-missing, parity-missing, or otherwise deficient.
* `@local_replication_status`: the local status on the chunk-replicator master peer.
* `@scan_flags` and `@local_scan_flags`: whether the chunk is present in leader/replicator scan queues that maintain bad-state maps and schedule work.
* `@jobs` and `@local_jobs`: active and recently finished jobs for the chunk, including job type, target node address, state, epoch, start time, and origin master.
* `@part_loss_time` and `@local_part_loss_time`: time when an erasure part loss was detected, if any.
* `@available`: whether the chunk is currently considered available for reads.
* `@confirmed`: whether the chunk upload has been confirmed and committed into master metadata.
* `@vital` and `@historically_non_vital`: whether loss of this chunk should trigger vital-data incident handling.
* `@movable`: whether the system can move or replicate the chunk as part of balancing and repair.
* `@requisition`, `@local_requisition`, and `@external_requisitions`: effective per-medium replication requirements derived from chunk owners and accounts.

The virtual bad-state maps under `//sys`, such as `//sys/underreplicated_chunks`, `//sys/lost_vital_chunks`, `//sys/data_missing_chunks`, `//sys/parity_missing_chunks`, `//sys/quorum_missing_chunks`, `//sys/inconsistently_placed_chunks`, and `//sys/replica_temporarily_unavailable_chunks`, are indexes over these per-chunk states. Use the maps for cluster-wide triage and chunk attributes for per-chunk diagnosis.

## Bottlenecks during recovery { #bottlenecks }

Common bottlenecks are:

* **Source scarcity.** If only one replica or a minimal erasure quorum remains, all repair traffic must read from a small set of disks and hosts.
* **Target scarcity.** A medium with low free space, unhealthy locations, or strict placement constraints may not have enough valid targets.
* **Rack constraints.** Rack-aware placement protects durability, but after a rack outage the master may intentionally avoid placing multiple replicas in the same remaining rack.
* **Disk bandwidth.** Repairs are large sequential transfers and can saturate failed-replacement disks or busy source disks.
* **Network bandwidth.** Cross-rack recovery may be limited by top-of-rack links or distributed network throttlers.
* **Master automaton load.** A large failure creates many metadata updates, queue changes, and heartbeat deltas. High master automaton CPU delays scheduling and confirmation.
* **Chunk count.** Many small chunks increase master memory use, queue sizes, and per-chunk scheduling overhead.
* **Removal backlog.** Cleanup of destroyed or obsolete replicas consumes node work queues and can mask useful capacity until it catches up.
* **Data node job slots.** Replication, repair, removal, seal, and merge jobs consume node resource slots. Defaults are intentionally conservative (`replication_slots = 16`, `removal_slots = 16`, `repair_slots = 4`, `seal_slots = 16`, `merge_slots = 4`) and may be overridden for production. Even with free disk and network bandwidth, recovery stops accelerating when replication or repair slots, or their data-size limits, are saturated.

## Metrics and inspection points { #metrics }

Use these indicators when diagnosing recovery:

| Area | What to watch | Why it matters |
| --- | --- | --- |
| Vital data loss | `//sys/lost_vital_chunks/@count`, Odin `lost_vital_chunks` | Any non-zero value is a serious incident for vital data. |
| Replicated recovery backlog | `yt_chunk_server_underreplicated_chunk_count`, `//sys/underreplicated_chunks/@count` | Shows replicated chunks waiting for more copies. |
| Erasure recovery backlog | `yt_chunk_server_data_missing_chunk_count`, `yt_chunk_server_parity_missing_chunk_count`, `//sys/data_missing_chunks/@count`, `//sys/parity_missing_chunks/@count` | Shows erasure-coded chunks missing data or parity parts. |
| Node and location health | data node alerts, node liveness, location free space, `chunk_count`, `trash_chunk_count` | Identifies failed disks, unavailable hosts, and cleanup pressure. |
| Master pressure | `yt_resource_tracker_total_cpu{service="yt-master", thread="Automaton"}`, master memory, changelog and snapshot free space | Recovery is metadata-heavy and can be delayed by master saturation. |
| Account pressure | `yt_accounts_chunk_count`, disk-space quota metrics | Quota exhaustion can prevent new data and complicate movement to other media. |
| Queue pressure | node push/pull replication queue sizes, pull replication chunk count, `active_job_count`, replication/repair/removal slot usage | Shows whether jobs are scheduled faster than nodes complete them or whether job slots are saturated. |

For a specific chunk, inspect its attributes such as stored replicas, last-seen replicas, requisition, replication, and replication status. `last_seen_replicas` is especially useful for deciding which hosts or disks might restore an otherwise lost chunk.

## Operational controls { #controls }

Operators can influence recovery with the following controls:

* **Maintenance flags.** Use `yt add-maintenance --component cluster_node --type pending_restart ...` before planned node restarts; remove it after the node returns.
* **Media and primary medium settings.** Move new writes to a healthier medium by changing `primary_medium`; use `media` carefully because changing it may trigger large background movement.
* **Replication factor.** Increase replication for critical replicated data before risky maintenance; remember that increasing it creates immediate background work.
* **Erasure codec.** Choose erasure for large, cold data to reduce storage overhead while retaining tolerance to part loss; avoid erasure where repair latency or CPU cost is unacceptable.
* **Throttlers.** Raise recovery throttles during an incident if user traffic can tolerate it; lower them if repair traffic threatens cluster availability.
* **Decommissioning and disablement.** Drain or decommission nodes gradually so the replicator can keep up.
* **Write-session disablement.** Use `disable_write_sessions` when the node should keep serving existing replicas but should not receive new chunk writes.
* **Chunk sizing and compaction.** Avoid excessive small chunks; merge or compact data to reduce metadata pressure and replication queue overhead.
* **Quotas and capacity.** Keep per-medium free space and account quotas high enough for repair headroom.

## Placement strategy and data-loss mitigation { #placement }

Data placement mitigates loss by reducing correlated replica failures:

* **Rack awareness.** Replicas or erasure parts are spread across racks when rack information is available. A single rack outage should not remove all copies of replicated data or too many parts of an erasure-coded chunk.
* **Per-medium isolation.** Media isolate capacity and performance tiers. A failure or exhaustion of one medium does not directly consume quota or placement slots in another medium.
* **Replica-per-rack limits.** The master avoids placing too many replicas of the same chunk in one failure domain. During maintenance, this is why one-rack-at-a-time procedures are safer.
* **Transient media marking.** RAM or other unreliable media can be marked transient so operators can identify data stored only on volatile locations as precarious.
* **Vitality.** Vital chunks receive stronger operational attention and alerts. Non-vital chunks, such as intermediate operation outputs, can be recomputed and therefore do not have to drive the same incident response.

## Disk hot-plug and hot-unplug support { #disk-hotplug }

When a data node is integrated with the disk manager and hot swap is enabled, storage can handle disk replacement without stopping the whole node:

1. The location health checker periodically reads disk-manager state.
2. If a disk is reported as failed, the corresponding location is marked failed and scheduled for disablement; alerts identify the disk id, model, path, device name, and state.
3. The master stops considering replicas on the disabled location available and schedules replication or erasure repair elsewhere.
4. The operator can destroy the disabled location when the disk is being removed.
5. After the replacement disk is connected and the disk manager reports it as healthy again, the location can be resurrected or recreated according to the node configuration.

This is hot-plug/hot-unplug at the disk/location level, not a guarantee that every filesystem, volume plugin, or Kubernetes storage class can be replaced online. If hot swap is not configured, disk replacement usually requires node-level maintenance: disable write sessions or decommission the node, stop it, replace the disk or volume, then start the node and let full heartbeats reconcile location state.

## Replication versus erasure coding { #replication-vs-erasure }

**Replication** stores full copies of the chunk. With replication factor 3, any single replica can serve reads and repair only needs to copy one full replica to a new location. Replication is simple, fast to read, and fast to repair, but it consumes more disk space.

**Erasure coding** splits a chunk into data and parity parts. It reduces storage overhead for large datasets and can tolerate loss of some parts, but repair must read multiple surviving parts and reconstruct the missing one. This makes repair more CPU- and network-intensive, especially when many chunks lose parts at once.

A practical strategy is to use replication for hot, latency-sensitive, or small data, and erasure coding for large, colder data where storage efficiency matters. In both modes, the most important durability rule is the same: keep enough independent failure domains healthy and enough repair bandwidth available before the next failure occurs.
