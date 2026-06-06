# Tuning YT bus RPC service performance for data-node table I/O

> **Draft metadata**
>
> - **Draft number:** 14
> - **Author:** AI agent (OpenAI GPT-5.5)
> - **Created:** 2026-06-06
> - **Status:** In progress; requires review by YT storage/RPC maintainers.

This page is a short checklist for tuning high-RPS native bus RPC traffic, with emphasis on **data node** read/write paths used by C++ table readers and writers. It is intentionally not exhaustive: use it to find the bottleneck stage and the first set of knobs to inspect.

## Scope

Applies mostly to static table/file chunk traffic over native RPC:

- reads: C++ table reader -> chunk reader -> RPC/bus -> data node `GetBlockSet`, `GetBlockRange`, `GetChunkMeta`, `LookupRows`;
- writes: C++ table writer -> chunk writer -> RPC/bus -> data node `StartChunk`, `PutBlocks`, `SendBlocks`, `FlushBlocks`, `FinishChunk`;
- cluster-local traffic where client, RPC proxy, job proxy, exec node, and data node may be separate processes.

Do not use this page as a replacement for workload-level design. If rows are too small, chunks are tiny, schemas are inefficient, or replication/erasure settings are inappropriate, bus tuning only hides the problem.

## End-to-end request pipeline

For one high-level table read/write, the hot path usually looks like this:

1. **Client-side batching and chunk planning.** The C++ reader locates chunks, chooses replicas, batches block requests, and uses a chunk-reader pool. The writer buffers rows into blocks/chunks before opening sessions on target data nodes.
2. **RPC channel and request construction.** Request/response codecs, retry policy, timeouts, streaming timeouts, and bus client TCP options are applied.
3. **Bus send path.** Messages and attachments are encoded, checksummed/encrypted if enabled, placed into bus outgoing queues, multiplexed by band, and flushed by bus dispatcher threads.
4. **Kernel/network/NIC.** TCP pacing, socket buffers, congestion control, IRQ/RSS/RPS/XPS, NIC queues, and packet drops determine whether user-space queues drain smoothly.
5. **Data-node bus receive path.** Bus dispatcher threads decode packets and hand RPC messages to the RPC server.
6. **RPC service queueing.** Data node methods enforce per-method queue limits, byte limits, concurrency limits, request-byte throttlers, request-weight throttlers, authentication queues, and per-workload request queues.
7. **Data-node storage path.** Reads pass through block cache, disk throttlers, memory tracking, and location I/O. Writes pass through session memory, `PutBlocks`/`FlushBlocks`, replication/repair/merge throttlers, location watermarks, and disk writeback.
8. **Reply path.** Blocks or acknowledgements return through RPC and bus queues, where response attachments can be larger than request bodies for reads.

Tune from the outside in: first verify that client concurrency and batching can generate enough load, then check bus/RPC queues, then storage throttlers and disk/NIC/kernel counters.

## Main bottleneck classes and first checks

| Symptom | Likely stage | First metrics/checks | First knobs |
|---|---|---|---|
| High latency with low CPU/network | client under-parallelism | reader pool utilization, low bus `out_bytes`, low RPC concurrency | reader/writer concurrency, chunk-reader pool, block size, batch size |
| RPC queue grows; bus is idle | service limits or prioritization | `/rpc/server/.../request_queue_size`, `/concurrency`, `/local_wait_time` | method `queue_size_limit`, `concurrency_limit`, request queue provider, workload category |
| Bus pending bytes/packets grow | TCP/NIC or peer not reading | `/bus/pending_out_packets`, `/bus/pending_out_bytes`, retransmits, stalled writes | dispatcher threads, socket buffers, NIC queues, kernel TCP settings |
| Data-node throttled read/write counters grow | node/network/disk throttlers | `/data_node/throttlers`, `/location/disk_throttler`, throttled read/write counters | node `network_bandwidth`, in/out/fair throttlers, location throttlers |
| Disk busy but network below target | media bottleneck | iostat, disk queue, location read/write latencies, block cache hit rate | location weights, read coalescing, block size, compression, storage layout |
| CPU saturated in user space | serialization/compression/checksum/encryption | CPU profiles, bus encoder/decoder errors, codec counters | disable unnecessary codecs, tune block size, add bus/RPC threads |
| Retransmits/drops under load | kernel/NIC | `ss -ti`, `nstat`, `ethtool -S`, softirq CPU | RSS/RPS/XPS, backlog, congestion control, buffers, MTU |

## RPC service limits and prioritization

Data-node methods have built-in defaults that are safe but not always optimal for very small or very large machines. The most relevant dynamic RPC method settings are:

- `queue_size_limit` / `queue_byte_size_limit`: protects memory and prevents unlimited waiting.
- `concurrency_limit` / `concurrency_byte_limit`: caps requests currently being handled.
- `request_bytes_throttler` and `request_weight_throttler`: rate-limit large or expensive calls independently of count-based concurrency.
- `heavy` and `pooled`: decide whether work is executed by the regular RPC invoker or a pooled/heavy path.
- `authentication_queue_size_limit` and `pending_payloads_timeout`: matter when clients open many channels or use streaming payloads.
- per-workload request queues: reads such as `GetBlockSet`/`GetBlockRange` are split by workload category, so interactive and batch traffic can be isolated.

Typical rule: increase concurrency only while `/local_wait_time` dominates and CPU, disk, memory, and bus queues have headroom. If `/total_time` grows together with bus pending bytes or disk queues, increasing RPC concurrency just moves the queue elsewhere.

## Data-node throttlers and storage queues

Check throttlers in this order:

1. **Cluster node network bandwidth.** `network_bandwidth` should match usable host bandwidth, not a historical default. Keep `throttler_free_bandwidth_ratio` as headroom for control traffic and bursts.
2. **Fair network throttlers.** Use in/out fair throttlers and per-bucket `in_throttlers`/`out_throttlers` when batch reads, replication, repair, and tablet traffic compete.
3. **Data-node raw throttlers.** Replication, repair, merge, autotomy, artifact-cache, tablet flush/compaction/logging/snapshot traffic should not starve user reads/writes.
4. **Location disk throttlers.** Each location has per-kind throttlers, optional uncategorized throttler, fair-share workload category weights, and `memory_limit_fraction_for_starting_new_sessions`.
5. **Disk watermarks.** Low/high/disable-write watermarks and trash cleanup thresholds can silently turn a throughput problem into an availability problem.
6. **Read coalescing and memory.** `coalesced_read_max_gap_size` can reduce IOPS for nearby blocks, but increases read amplification and memory use.
7. **Write pacing by drive endurance.** `max_write_rate_by_dwpd` is useful for mixed clusters where NVMe endurance differs by model.

For write-heavy tests, monitor session memory and `PutBlocks` wall time. For read-heavy tests, monitor block-cache hit rate, block read latency by workload category, and disk queue depth.

## Bus RPC and TCP knobs

Important bus options that are often left at unsuitable defaults:

- **Bus dispatcher `thread_pool_size`.** Default-sized pools are often enough for small nodes but can underfeed 25/100 Gbps hosts. Scale with NIC queues and CPU sockets, not with total cores blindly.
- **`thread_pool_polling_period`.** Long polling periods reduce CPU but can add latency at high RPS; validate p99 latency before and after changes.
- **`network_bandwidth` in bus dispatcher.** Set for profiling/alerts so bus saturation is visible.
- **Multiplexing bands and TOS.** Configure `multiplexing_bands`, `tos_level`, and `network_to_tos_level` only if the network fabric honors DSCP/TOS; otherwise it gives a false sense of prioritization.
- **`min_multiplexing_parallelism` / `max_multiplexing_parallelism`.** Higher values can improve high-BDP links but may reorder pressure and increase CPU.
- **TCP `enable_no_delay`.** Keep enabled for latency-sensitive small RPCs; test disabling only for bulk-only streams.
- **TCP `enable_quick_ack`.** Usually keep enabled for request/response traffic.
- **RTO settings (`min_rto`, `max_rto`, `rto_scale`) and `connect_timeout`.** Defaults are conservative; large clusters with fast failover may need shorter detection, but too-short RTO amplifies transient congestion.
- **Checksums/encryption.** `verify_checksums`, `generate_checksums`, and encryption/verification settings cost CPU. Do not disable integrity/security globally without an explicit risk decision.
- **Server backlog and connection limits.** `max_backlog_size` and `max_simultaneous_connections` must be compatible with kernel `somaxconn`, file descriptor limits, and client fan-out.
- **Client channel TTL.** Too-short idle channel TTL creates reconnect storms; too-long TTL can pin bad endpoints and stale balancing.

## C++ table reader and writer knobs

Reader side:

- Increase the chunk-reader pool only until data-node queues, bus pending bytes, or disk queues become the bottleneck.
- Use enough outstanding block requests to cover RTT * bandwidth; high-BDP 100 Gbps links need much more in flight than 1 Gbps links.
- Prefer larger reads and block batching for throughput; prefer smaller batches for tail latency and cancellation responsiveness.
- Check `unavailable_chunk_strategy`, chunk availability policy, P2P, and proxying data-node service settings before blaming bus.
- Dynamic-store reads have their own window (`window_size`) and row limits; do not tune them as static chunk reads.

Writer side:

- `block_size` and `max_buffer_size` control memory and RPS. Small blocks raise RPC RPS and metadata overhead; large blocks improve throughput but raise latency and memory.
- `max_row_weight`, `max_key_weight`, and `max_data_weight_between_blocks` protect pathological rows and oversized blocks.
- `sample_rate`, chunk indexes, key filters, columnar statistics, and compression features consume CPU and memory; enable only if downstream reads benefit.
- For high-throughput append/write, keep enough chunks/sessions open to hide RTT, but cap by data-node write memory and target disk queues.
- Replication factor, erasure codec, write quorum, and `enable_multiplexing` change both network fan-out and data-node CPU.

## Hardware scaling guidance

The following ranges are starting points, not prescriptions. Validate with production-like row sizes, compression, replication, and failure domains.

| Node class | Example hardware | Main risk | Starting approach |
|---|---:|---|---|
| Tiny | 4 CPU, 16 GiB RAM, 1 Gbps, 1 SSD | context switching, memory, fd limits | keep bus/RPC threads small; low reader concurrency; larger blocks to reduce RPS; conservative queues |
| Small | 8-16 CPU, 32-64 GiB, 10 Gbps, 2-4 SSD/NVMe | one hot disk or one hot CPU | match network bandwidth; separate batch vs interactive workloads; moderate queues; watch disk latency |
| Medium | 32-64 CPU, 128-256 GiB, 25 Gbps, 4-8 NVMe | bus/RPC queues and IRQ placement | raise bus dispatcher threads; tune RSS/RPS/XPS; increase reader pools and write sessions; enable fair throttlers |
| Large | 96-192 CPU, 512 GiB-1 TiB, 50/100 Gbps, 8-16 NVMe | NUMA, NIC queues, kernel buffers | pin/stripe IRQs; verify socket buffers and backlog; split traffic by workload; tune per-location weights |
| Very large | 256-512 CPU, 1-2 TiB, 100 Gbps, 16-32 NVMe | hidden serialization point, single queue, memory blow-up | avoid one global queue; raise byte limits carefully; shard clients; profile CPU; validate NUMA locality and per-drive saturation |

A common mistake on both ends of the range is copying configs from a different hardware class. On a 4-core node, a large queue only increases latency. On a 512-core node, a default eight-thread bus dispatcher or historical `network_bandwidth` can make the machine look artificially slow.

## Linux kernel and host tunings commonly missed

Validate these on every performance test host, not only on data nodes:

- **File descriptors and ephemeral ports:** `ulimit -n`, `fs.file-max`, `net.ipv4.ip_local_port_range`, `net.ipv4.tcp_tw_reuse` policy, and connection churn.
- **Listen backlog:** `net.core.somaxconn` and `net.ipv4.tcp_max_syn_backlog` should not be lower than bus server backlog during reconnect storms.
- **Socket buffers:** `net.core.rmem_max`, `net.core.wmem_max`, `net.ipv4.tcp_rmem`, `net.ipv4.tcp_wmem`; high-BDP 100 Gbps paths need larger buffers than defaults on many distributions.
- **Packet backlog:** `net.core.netdev_max_backlog` and NIC ring sizes (`ethtool -g/-G`) matter when softirq cannot keep up.
- **Congestion control and qdisc:** use a known policy (`cubic`/`bbr`, `fq`/`fq_codel`) and keep it consistent across benchmark hosts.
- **IRQ/RSS/RPS/XPS:** ensure NIC queues are spread across CPUs and NUMA nodes; avoid pinning all RX/TX interrupts to one socket.
- **MTU and offloads:** jumbo frames help only if the whole path supports them; verify TSO/GSO/GRO/LRO/checksum offloads instead of assuming defaults.
- **NUMA locality:** keep NIC IRQs, bus threads, and storage interrupts reasonably local on dual-/multi-socket hosts.
- **CPU frequency and power policy:** disable deep power-saving for latency tests; verify no thermal throttling.
- **Disk scheduler and writeback:** choose a scheduler suitable for NVMe/SATA, and watch dirty page limits for buffered writes.
- **Transparent huge pages and memory pressure:** avoid surprise compaction stalls; watch PSI, major faults, and OOM-killer logs.
- **Time sync:** skew affects tracing, timeout interpretation, and cross-host latency comparisons.

## Monitoring checklist

Collect at least these groups on client, RPC proxy/job proxy, and data node:

- **RPC server:** per-service/per-method request count, failed/timed-out/canceled requests, `/execution_time`, `/remote_wait_time`, `/local_wait_time`, `/total_time`, request/response body and attachment bytes, `/request_queue_size`, `/request_queue_byte_size`, `/concurrency`, and corresponding limits.
- **Bus:** `/bus/in_bytes`, `/bus/out_bytes`, packets, pending out packets/bytes, client/server connections, stalled reads/writes, read/write errors, retransmits, encoder/decoder errors; split by network and encryption tags if available.
- **Data node:** `/data_node/throttlers`, data-node overload controller, per-location `/used_memory`, `/blob_block_bytes`, throttled read/write counters, `/put_blocks_wall_time`, blob block read latency/size/time, chunk-meta read time, probe-write queue size and requested memory.
- **Chunk/table client:** chunk-reader pool utilization, retries, locate latency, unavailable/repairing chunk counters, writer open/close/flush latency, block size distribution, in-flight chunks/sessions.
- **Kernel/NIC:** `sar -n`, `ss -ti`, `nstat`, `softnet_stat`, `ethtool -S`, `iostat -x`, CPU softirq, run queue, PSI, page faults, fd usage, socket memory.

Useful quick interpretation:

- High `/remote_wait_time` with low `/local_wait_time`: client/proxy or network before data-node queue.
- High `/local_wait_time` with flat bus pending bytes: data-node RPC queue or throttler.
- High `/execution_time`: storage, CPU, or downstream calls inside method handling.
- High bus pending bytes plus retransmits: network/kernel/NIC, not RPC method concurrency.
- High throttled probing reads/writes: clients are correctly seeing throttling; tune throttlers or reduce offered load.

## Minimal tuning workflow

1. Pick the bottleneck metric and write down the target: RPS, GiB/s, p99 latency, or fairness share.
2. Run one client and one data node first; then scale clients; then scale data nodes.
3. Increase only one of: client concurrency, RPC concurrency, bus threads, disk/network throttler limit, or kernel buffers.
4. Stop increasing a knob once another queue starts growing faster than throughput.
5. Re-test with production failure modes: one slow disk, one unavailable replica, reconnect storm, mixed interactive and batch traffic.
6. Keep separate profiles for 1 Gbps, 10/25 Gbps, and 100 Gbps hosts. A single universal config is usually worse than three small profiles.

## Open review items

The following details should be confirmed by maintainers before graduation:

- recommended numeric defaults for bus dispatcher threads per NIC queue and CPU socket;
- current production metric paths in dashboards, including tags used for data-node method and workload category;
- whether any bus checksum/encryption settings are mandatory in managed deployments;
- known-good kernel profiles for 1, 10/25, and 100 Gbps environments.
