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

A single `read_table` or `write_table` call is not a single data-node RPC. The client may read several input tables, several ranges per table, and many chunks per range at the same time. Each logical chunk reader then fetches blocks in bounded groups; each writer keeps several sessions and sends groups of blocks to one or more target data nodes.

### 1. API request, path expansion, and data-slice planning

The client first expands rich YPaths, table indexes, column filters, row/key ranges, transaction/prerequisite options, and workload descriptors into data slices and chunk specs. This stage decides how many chunks can be active at once and whether the next stages see a few large sequential reads or many small random reads.

- **Batching/merging:** adjacent ranges over the same table/chunk should be merged before they become separate block requests; otherwise the bus sees excessive RPS.
- **Isolation:** set the correct workload category for user-interactive, batch, system replication/repair, tablet, and artifact-cache traffic; data-node request queues and fair throttlers depend on it.
- **Metrics:** locate/fetch-chunk-spec latency, chunk count per request, unavailable/repairing chunk count, table-range count, reader wait/idle time.
- **Options to inspect:** `suppress_access_tracking`, `suppress_expiration_timeout_renewal`, `unavailable_chunk_strategy`, `chunk_availability_policy`, `keep_in_memory`, workload descriptor/category.

### 2. Multi-reader scheduling across tables, ranges, and chunks

For multi-table or multi-range reads, the multi-reader manager owns many per-chunk/per-slice reader factories. It can open readers sequentially or in parallel and caps active readers by `max_parallel_readers` (default 512). It also obeys a global multi-reader memory manager, so high `max_parallel_readers` does not guarantee high throughput if memory is exhausted.

- **Concurrency:** increase `max_parallel_readers` only when active readers are below the limit and bus/storage queues have headroom.
- **Queues/buffers:** `max_buffer_size` bounds buffered data across child readers and must be at least twice `window_size`; with many tables/chunks, memory becomes the real concurrency limit.
- **Isolation:** parallel reads are useful for batch scans; latency-sensitive point/range reads often need lower fan-out and stricter workload category.
- **Metrics:** active reader count, reader creation/open latency, multi-reader wait time, buffered bytes, failed chunk IDs, per-reader data/decompression statistics.

### 3. Per-chunk block fetch window and request grouping

A chunk reader hands block descriptors to the block fetcher. The fetcher keeps a byte prefetch **`window_size`** (default 20 MiB) but does not send the whole window as one RPC. It acquires and sends at most **`group_size`** bytes per fetch group (default 15 MiB, and it cannot exceed `window_size`). Thus the effective outstanding read memory is window-sized, while the individual `GetBlockSet`/`GetBlockRange` RPC payloads are group-sized. Duplicate block requests are de-duplicated in the fetch window; out-of-order blocks can optionally be grouped.

- **Batching/merging:** `group_size` is the RPC grouping knob; `group_out_of_order_blocks` may reduce request count for non-sequential reads but can increase latency for the next row batch.
- **Buffers:** `window_size + group_size` contributes to memory estimates per chunk reader; multiply by active readers, tables, and chunks.
- **Concurrency:** for high-BDP links, raise `window_size` and possibly `group_size`; for small RAM/1 Gbps hosts, reduce active readers before increasing window.
- **Cache path:** `use_uncompressed_block_cache`, `use_block_cache`, and `use_async_block_cache` can remove bus traffic but add local CPU/memory pressure.
- **Metrics:** block count, prefetched block count, max block size, bytes read from cache, block fetch wait time, decompression time, read data size.

### 4. Replica selection, probing, hedging, and read batching

Replication readers locate seeds/replicas, optionally probe peer queue size, choose local data-center/rack/host replicas, and issue block RPCs. Hedging can send backup RPCs after a delay; batching can combine block requests before sending them to data nodes.

- **Batching/merging:** `use_read_blocks_batcher` and `block_set_subrequest_threshold` reduce RPC count for many small block reads.
- **Concurrency:** hedging increases offered load and may double traffic during tail-latency incidents; enable only when the cluster has spare bandwidth.
- **Isolation:** `enable_workload_fifo_scheduling` annotates reads so fair scheduling preserves request order within a workload.
- **Options:** `block_rpc_timeout`, `block_rpc_hedging_delay`, `cancel_primary_block_rpc_request_on_hedging`, `probe_rpc_timeout`, `probe_peer_count`, `pass_count`, `retry_count`, `retry_timeout`, `session_timeout`, `prefer_local_*`, `fetch_from_peers`, `enable_p2p`, `use_proxying_data_node_service`.
- **Metrics:** probe latency, peer queue size, retry/pass count, hedged request count, banned peers, local-vs-remote replica choice, block RPC latency and failures.

### 5. Writer buffering, chunk sessions, and block formation

The table writer buffers rows into blocks and chunks before remote upload. Small blocks increase RPC RPS and metadata overhead; large blocks reduce RPS but increase write latency, memory, and tail amplification. Replicated writers use a send window; erasure writers have separate erasure and writer windows.

- **Batching/buffers:** table `block_size` and `max_buffer_size` shape block formation; replicated writer `send_window_size` (default 32 MiB) bounds in-flight block data and writer `group_size` (default 10 MiB) bounds one send group.
- **Concurrency:** keep enough open chunks/sessions to hide RTT, but cap by data-node write memory, target count, and disk queues.
- **Merging:** larger groups merge more blocks into `PutBlocks`/`SendBlocks`; too large groups can monopolize queue slots and hurt interactive traffic.
- **Options:** `upload_replication_factor`, `min_upload_replication_factor`, `direct_upload_node_count`, `node_rpc_timeout`, `probe_put_blocks_timeout`, `populate_cache`, `sync_on_close`, `enable_direct_io`, `use_probe_put_blocks`, `preallocate_disk_space`, erasure `writer_window_size`, `writer_group_size`, `erasure_window_size`.
- **Metrics:** writer buffered bytes, open/close/flush latency, in-flight sessions, blocks per chunk, `PutBlocks`/`FlushBlocks` latency, retry/reallocation count.

### 6. Client RPC channel, codecs, retries, and local queues

The native client serializes request headers and attachments, applies request/response codecs, chooses a channel, and manages retries/timeouts. A single application process can create many channels when reading many tables/chunks or when many user threads share one client.

- **Queues/buffers:** request attachments are queued before bus send; response attachments are buffered until consumed by table reader code.
- **Concurrency:** client-side request fan-out should be sized from `active_readers * window_size / RTT`, not from CPU count alone.
- **Isolation:** use different clients or workload descriptors for latency-sensitive and batch flows if they otherwise share channels and retry budgets.
- **Options:** `rpc_timeout`, `rpc_acknowledgement_timeout`, `default_total_streaming_timeout`, `default_streaming_stall_timeout`, `request_codec`, `response_codec`, `enable_retries`, `retrying_channel`, `bus_client`, `idle_channel_ttl`.
- **Metrics:** client request count, retries, timeout/cancel count, request/response attachment bytes, channel count, queue wait before send.

### 7. Bus encode, multiplexing, and outgoing packet queues

Bus encodes messages, optionally generates checksums and encryption records, assigns a multiplexing band, and appends packets to per-connection outgoing queues. This stage is the first place where a healthy client can still accumulate bytes because the peer, kernel, or NIC cannot drain fast enough.

- **Batching:** fewer, larger attachments reduce per-packet overhead but can increase head-of-line blocking; `group_size` and writer send group size are the upstream knobs.
- **Queues:** watch pending out packets/bytes; they should be short-lived. Persistent growth means downstream congestion.
- **Concurrency:** bus dispatcher `thread_pool_size` should scale with NIC queues/sockets on 25/100 Gbps hosts; the historical default is usually too small for very large nodes.
- **Isolation/prioritization:** `multiplexing_bands`, `tos_level`, and `network_to_tos_level` only help if the network honors them end-to-end.
- **Options:** `thread_pool_size`, `thread_pool_polling_period`, dispatcher `network_bandwidth`, `min_multiplexing_parallelism`, `max_multiplexing_parallelism`, `verify_checksums`, `generate_checksums`, encryption and verification mode.
- **Metrics:** `/bus/out_bytes`, `/bus/out_packets`, `/bus/pending_out_packets`, `/bus/pending_out_bytes`, encoder errors, stalled writes, per-band/per-network counters.

### 8. TCP, kernel, NIC, and fabric

TCP and NIC queues convert bus writes into packets. A misconfigured host can show low data-node CPU and low disk usage while bus pending bytes, retransmits, or softirq backlog grow.

- **Buffers/queues:** socket send/receive buffers, NIC rings, qdisc queues, and `netdev_max_backlog` must fit the bandwidth-delay product and burst size.
- **Concurrency:** RSS/RPS/XPS and IRQ placement must distribute packet work across enough CPUs, especially on multi-socket 100 Gbps nodes.
- **Isolation:** DSCP/TOS and qdisc classes must match bus multiplexing bands; otherwise RPC priority is lost below user space.
- **Options:** bus `enable_no_delay`, `enable_quick_ack`, `min_rto`, `max_rto`, `rto_scale`, `connect_timeout`; Linux TCP buffers, backlog, congestion control, qdisc, MTU, offloads.
- **Metrics:** `ss -ti`, retransmits, drops, NIC `ethtool -S`, softirq CPU, `/proc/net/softnet_stat`, `nstat`, per-queue interrupt rates.

### 9. Data-node bus receive and RPC dispatch

The data node receives packets, decodes bus messages, authenticates RPCs, accounts request size, and dispatches each method into an RPC request queue. Large read responses stress the reply path; write requests stress incoming attachment buffers.

- **Queues:** authentication queue, pending payloads, method request queue, and byte queue are separate bottlenecks.
- **Concurrency:** method `concurrency_limit` and `concurrency_byte_limit` govern executing requests; queue limits include waiting and executing requests.
- **Isolation:** data-node `GetBlockSet` and `GetBlockRange` use per-workload-category request queues, so workload descriptors directly affect fairness.
- **Options:** service/method `queue_size_limit`, `queue_byte_size_limit`, `concurrency_limit`, `concurrency_byte_limit`, `request_bytes_throttler`, `request_weight_throttler`, `authentication_queue_size_limit`, `pending_payloads_timeout`, `pooled`, `heavy`.
- **Metrics:** `/rpc/server/.../request_queue_size`, `/request_queue_byte_size`, `/concurrency`, `/concurrency_byte`, `/local_wait_time`, `/remote_wait_time`, `/execution_time`, `/total_time`, request/response body and attachment bytes.

### 10. Data-node read execution and disk/cache queues

Read methods probe chunk availability and throttling, check block cache, acquire read memory, optionally coalesce nearby disk reads, read/decompress blocks, and build response attachments. `ProbeChunkSet` can expose disk and network throttling before heavy block reads are sent.

- **Batching/merging:** `GetBlockSet`/`GetBlockRange` groups block IDs from the client; location `coalesced_read_max_gap_size` can merge nearby disk I/O at the cost of read amplification.
- **Queues/buffers:** read memory tracker and disk throttler queues gate sessions before disk I/O starts.
- **Concurrency:** high RPC concurrency without enough disk queues or memory increases wait time; use per-location throttlers and weights.
- **Isolation:** fair-share workload category weights protect interactive reads from scans, repair, and replication.
- **Options:** location `throttlers`, `enable_uncategorized_throttler`, `uncategorized_throttler`, `fair_share_workload_category_weights`, `memory_limit_fraction_for_starting_new_sessions`, `coalesced_read_max_gap_size`, block-cache capacities.
- **Metrics:** `/location/disk_throttler`, `/location/blob_block_read_latency`, `/blob_block_read_time`, `/blob_block_read_size`, `/blob_block_read_bytes`, `/blob_chunk_meta_read_time`, `/throttled_reads`, `/throttled_probing_reads`, block-cache hit bytes.

### 11. Data-node write execution, replication, and flush/merge pressure

Writes open chunk sessions, accept `PutBlocks`, optionally forward `SendBlocks` to other replicas, flush data, close chunks, and update metadata. Replication, repair, merge, tablet flush/compaction, and artifact-cache traffic share node/network/disk throttlers unless isolated.

- **Batching/merging:** writer group size controls how many blocks arrive in one `PutBlocks`; data-node sessions may flush groups and the storage layer may merge writes through disk queues.
- **Queues/buffers:** session write memory, probe-write queue, disk writeback, trash cleanup, and location watermarks can block new writes before bus/RPC queues look full.
- **Concurrency:** `SendBlocks`, `FlushBlocks`, and `PutBlocks` have their own method queues; replication factor multiplies downstream bus traffic.
- **Isolation:** separate user writes from replication/repair/merge/tablet workloads with node in/out throttlers and location fair-share weights.
- **Options:** data-node `throttlers`, cluster-node `network_bandwidth`, `in_throttler`, `out_throttler`, `in_throttlers`, `out_throttlers`, `throttler_free_bandwidth_ratio`, location watermarks, `io_weight`, `max_write_rate_by_dwpd`.
- **Metrics:** `/location/put_blocks_wall_time`, `/throttled_writes`, `/throttled_probing_writes`, `/probe_writes/queue_size`, `/probe_writes/requested_memory`, per-location used write memory, disk write latency, replication/repair throttler rates.

### 12. Reply delivery, row materialization, and downstream backpressure

The final reply path can bottleneck even when data-node execution is fast. Read responses contain large attachments that travel through bus queues, client RPC queues, decompression, table-format decoding, and user-code consumption. If user code reads rows slowly, buffers fill and upstream concurrency naturally collapses.

- **Buffers:** response attachments, decompressed blocks, table-reader row buffers, and multi-reader `max_buffer_size` together determine memory pressure.
- **Concurrency:** increasing data-node or client concurrency cannot help if row materialization or the consumer thread is saturated.
- **Merging:** larger block groups reduce RPC overhead but delay first rows and cancellation; smaller groups improve streaming latency but raise RPS.
- **Metrics:** response attachment bytes, client idle/wait/read time, decompression CPU, row decode CPU, application consumption rate, memory tracker usage.

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
