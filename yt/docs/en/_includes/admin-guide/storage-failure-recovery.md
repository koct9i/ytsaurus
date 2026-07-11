# Data storage failure handling and recovery

This draft describes how {{product-name}} stores data, detects storage failures, restores lost redundancy, and where operators can observe or control the recovery path.

## Bottom-up view of the storage layers { #layers }

### Chunk locations

A **chunk location** is a directory on a data node disk that belongs to a medium. Locations are the smallest operational unit for storing chunk replicas: they have capacity, disk health, read/write queues, and per-location throttlers. If a location becomes unhealthy or unavailable, all chunk replicas stored in it become unavailable until the location returns or the master schedules recovery elsewhere.

### Media

A **medium** is a logical pool of chunk locations, usually mapping to a disk class such as HDD or SSD. Quotas, balancing, and replication requirements are computed independently per medium. Each chunk owner has a primary medium for new writes and may request additional replicas on other media through the `media` attribute. Erasure-coded objects may also use `data_parts_only` media to place only data parts on a faster or cheaper tier.

### Data nodes

A **data node** owns one or more chunk locations and serves reads, writes, replication traffic, and erasure repair traffic. It periodically reports its state to masters through data node heartbeats. Heartbeats include the node and location state, chunk counts, and incremental changes such as added, removed, or medium-changed chunks. Data nodes also execute chunk replica jobs: copying blocks to another node, reading blocks for repair, removing obsolete replicas, and honoring local disk and network throttlers.

### Health checks and node state

The storage control plane treats failures as a sequence of visibility changes:

* A disk or location health checker marks local storage unhealthy or read-only.
* A data node may stop reporting because of process, host, network, or Kubernetes failure.
* The master node tracker eventually marks the node offline unless maintenance such as `pending_restart` intentionally extends its lease.
* Chunk replicas on offline or unhealthy locations are excluded from the set of available replicas used by the chunk replicator.

For short planned restarts, use `pending_restart` maintenance. It lets the master distinguish temporarily unavailable replicas from truly lost capacity and avoids unnecessary replication storms, provided that maintenance is limited to a safe failure domain, typically one rack at a time.

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
3. **Chunk enters a bad-state set.** If available replicas are below the requested replication factor, the chunk appears in `//sys/underreplicated_chunks`. If all vital replicas are unavailable, it appears in `//sys/lost_vital_chunks`.
4. **Placement is selected.** The master chooses a target node and location in the required medium, respecting rack awareness, per-medium capacity, decommissioning/maintenance state, and replica-per-rack limits.
5. **Replication job is issued.** The source and target data nodes copy blocks. The job competes for data node CPU, network, disk bandwidth, and throttler tokens.
6. **Heartbeat confirms completion.** The target reports the new replica to the master. The chunk leaves the bad-state set after metadata is refreshed.
7. **Cleanup follows.** If the source later returns and the chunk has too many replicas, obsolete replicas enter removal queues and are deleted in the background.

For erasure-coded chunks, the flow is similar but the bad states and repair criteria differ:

1. A missing parity part makes the chunk **parity-missing** but still readable and repairable.
2. A missing data part makes the chunk **data-missing**; reads may still succeed if enough parts remain for decoding.
3. If too many parts are missing, the chunk becomes unrecoverable until at least one last-seen part returns.
4. Erasure repair reads surviving parts, reconstructs the missing part, and writes it to a new target location.

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
| Queue pressure | node push/pull replication queue sizes and pull replication chunk count | Shows whether jobs are scheduled faster than nodes complete them. |

For a specific chunk, inspect its attributes such as stored replicas, last-seen replicas, requisition, replication, and replication status. `last_seen_replicas` is especially useful for deciding which hosts or disks might restore an otherwise lost chunk.

## Operational controls { #controls }

Operators can influence recovery with the following controls:

* **Maintenance flags.** Use `yt add-maintenance --component cluster_node --type pending_restart ...` before planned node restarts; remove it after the node returns.
* **Media and primary medium settings.** Move new writes to a healthier medium by changing `primary_medium`; use `media` carefully because changing it may trigger large background movement.
* **Replication factor.** Increase replication for critical replicated data before risky maintenance; remember that increasing it creates immediate background work.
* **Erasure codec.** Choose erasure for large, cold data to reduce storage overhead while retaining tolerance to part loss; avoid erasure where repair latency or CPU cost is unacceptable.
* **Throttlers.** Raise recovery throttles during an incident if user traffic can tolerate it; lower them if repair traffic threatens cluster availability.
* **Decommissioning and disablement.** Drain or decommission nodes gradually so the replicator can keep up.
* **Chunk sizing and compaction.** Avoid excessive small chunks; merge or compact data to reduce metadata pressure and replication queue overhead.
* **Quotas and capacity.** Keep per-medium free space and account quotas high enough for repair headroom.

## Placement strategy and data-loss mitigation { #placement }

Data placement mitigates loss by reducing correlated replica failures:

* **Rack awareness.** Replicas or erasure parts are spread across racks when rack information is available. A single rack outage should not remove all copies of replicated data or too many parts of an erasure-coded chunk.
* **Per-medium isolation.** Media isolate capacity and performance tiers. A failure or exhaustion of one medium does not directly consume quota or placement slots in another medium.
* **Replica-per-rack limits.** The master avoids placing too many replicas of the same chunk in one failure domain. During maintenance, this is why one-rack-at-a-time procedures are safer.
* **Transient media marking.** RAM or other unreliable media can be marked transient so operators can identify data stored only on volatile locations as precarious.
* **Vitality.** Vital chunks receive stronger operational attention and alerts. Non-vital chunks, such as intermediate operation outputs, can be recomputed and therefore do not have to drive the same incident response.

## Replication versus erasure coding { #replication-vs-erasure }

**Replication** stores full copies of the chunk. With replication factor 3, any single replica can serve reads and repair only needs to copy one full replica to a new location. Replication is simple, fast to read, and fast to repair, but it consumes more disk space.

**Erasure coding** splits a chunk into data and parity parts. It reduces storage overhead for large datasets and can tolerate loss of some parts, but repair must read multiple surviving parts and reconstruct the missing one. This makes repair more CPU- and network-intensive, especially when many chunks lose parts at once.

A practical strategy is to use replication for hot, latency-sensitive, or small data, and erasure coding for large, colder data where storage efficiency matters. In both modes, the most important durability rule is the same: keep enough independent failure domains healthy and enough repair bandwidth available before the next failure occurs.
