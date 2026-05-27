<!--
Draft number: 5
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: admin-guide/master-architecture.md
-->

# Master server architecture: performance and administration

Operational bottlenecks, monitoring, snapshots, and scaling guidance.

## Performance considerations { #performance }

### Automaton thread bottleneck

All persistent mutations on a single cell are applied serially on the automaton thread. The automaton thread is the primary throughput bottleneck. Monitor the metric `yt_resource_tracker_total_cpu{service="yt-master", thread="Automaton"}` — sustained load above 90% indicates the cell is under pressure.

Heavy single mutations (e.g. full heartbeats from nodes with hundreds of thousands of chunks) have been split into smaller batches to avoid blocking the automaton thread for too long.

### Mutation backlog and commit latency { #mutation-backlog }

Mutation latency for a single request is the sum of all pipeline stages:

```
latency ≈ serialization_wait + changelog_write + network_rtt_to_followers
          + automaton_queue_wait + automaton_apply_time
```

The most common sources of elevated latency are:

**1. Serialization wait (batching delay)**

Mutations accumulate in the draft queue until `SerializeMutations` runs. By default this executor fires every 5 ms (`mutation_serialization_period`). If the system is lightly loaded, a mutation submitted between two ticks simply waits up to 5 ms before it is even serialized. Enable `minimize_commit_latency: true` to trigger flush immediately after each serialization pass, trading slightly higher throughput for lower tail latency.

**2. Automaton queue depth**

After a mutation is committed (quorum reached) it is placed in a list to be applied by the automaton thread. If the automaton thread is already busy applying a large mutation, all subsequent mutations wait. The queue of committed-but-not-yet-applied mutations is visible in the Hydra monitoring endpoint as `last_offloaded_sequence_number - automaton_sequence_number`. A large gap here indicates automaton thread saturation.

**3. Mutation queue limits and restart**

The leader maintains a bounded in-memory queue of logged mutations that have not yet been confirmed as received by all peers (needed to retransmit to lagging followers). The queue is bounded by:

| Parameter | Default | Action when exceeded |
|-----------|---------|----------------------|
| `max_queued_mutation_count` | 100 000 | `LoggingFailed` — triggers quorum restart |
| `max_queued_mutation_data_size` | 2 GB | `LoggingFailed` — triggers quorum restart |

These limits protect against memory exhaustion when followers fall far behind. Hitting them causes the Hydra group to restart, which is disruptive. Monitor the metric `mutation_queue_size` and `mutation_queue_data_size` to detect growth before limits are reached.

**4. In-flight limits to followers**

To prevent network and memory overload, the leader caps the number of in-flight `AcceptMutations` requests to each follower:

| Parameter | Default | Effect |
|-----------|---------|--------|
| `max_in_flight_accept_mutations_request_count` | 10 | Maximum concurrent RPC calls per follower |
| `max_in_flight_mutations_count` | 100 000 | Maximum mutations in-flight per follower |
| `max_in_flight_mutation_data_size` | 2 GB | Maximum data in-flight per follower |

When a follower's in-flight limits are hit, the leader skips sending new mutations to that follower until acknowledgments arrive. This can delay quorum promotion if the follower is also a quorum member.

**5. Slow followers**

A follower in slow mode (after an RPC error or rejection) is sent only one request at a time. If that follower is needed for quorum, every commit waits for a full round-trip to that follower. Watch for log lines `Accept mutations mode is set to slow` — they indicate a follower recovery or network issue.

### Memory

The master keeps the entire Cypress tree, all chunk metadata, and all replicated global objects in RAM. Memory usage grows with:

- Number of nodes in Cypress.
- Number of chunks and replicas.
- Number of globally replicated objects (accounts, media, etc.) × number of cells.

Monitor `yt_resource_tracker_memory_usage_rss{service="yt-master"}`. Because snapshot creation uses `fork`, the master process must have at least **double** its working-set memory available on the host.

### Node registration and disposal

When a Data Node registers with the master (or its liveness transaction expires), the master must process a full heartbeat listing all chunks on that node. For nodes with many chunks, this is a heavy mutation. When a node is disposed (its liveness transaction aborted), the master must update the replica sets of all chunks on that node. Disposal of locations on a single node is processed sequentially. This is the primary reason why adding secondary chunk-host cells reduces the per-cell disposal cost: each cell handles only the chunks assigned to it.

### Changelog I/O latency

Mutation latency is directly tied to changelog write latency, because mutations are not committed until the write quorum has persisted the changelog entry. Use fast NVMe storage for changelogs. Monitor `yt_changelogs_available_space{service="yt-master"}` to ensure space does not run out.

### Snapshot I/O and forking

Snapshot creation causes the master process to fork. During the fork, copy-on-write pages may cause elevated memory usage. After the fork, the parent continues applying mutations while the child serializes state to disk. Slow snapshot writes extend the period of elevated memory pressure but do not block mutations. Monitor `yt_snapshots_available_space{service="yt-master"}`.

## Administration

### Snapshots and read-only mode

Use `yt-admin build-master-snapshots --read-only --wait-for-snapshot-completion` to create a clean snapshot with an empty subsequent changelog. This is required before major updates or before adding new master cells. In read-only mode the master accepts no mutations; this ensures the snapshot captures a fully quiesced state.

### Monitoring master health

Key checks:

- `quorum_health` (Odin) — verifies all masters are in quorum.
- `master_alerts` — reads `//sys/@master_alerts`; any non-empty value should be investigated.
- `yt_resource_tracker_total_cpu{thread="Automaton"}` — automaton thread CPU.
- `yt_resource_tracker_memory_usage_rss{service="yt-master"}` — master RSS.
- `yt_changelogs_available_space` / `yt_snapshots_available_space` — disk space for Hydra storage.
- `mutation_queue_size` / `mutation_queue_data_size` — leader in-memory mutation backlog. Sustained growth indicates followers falling behind or slow changelog I/O.

The `//sys` Cypress node exposes multicell status including registered cell tags:

```bash
yt get //sys/@registered_master_cell_tags
yt get //sys/@dynamically_propagated_masters_cell_tags
```

### Adding new master cells

Adding secondary cells requires a complete cluster downtime. For detailed steps, see [Extending master servers](../admin-guide/cell-addition.md).

After cells are added and global objects are replicated, assign roles via:

```bash
yt set //sys/@config/multicell_manager/cell_descriptors/<cell_tag>/roles '[chunk_host]'
```

If no explicit roles are assigned, a secondary cell may still receive the default `cypress_node_host | chunk_host` roles unless `remove_secondary_cell_default_roles` is enabled or the cell is dynamically propagated. Assign roles explicitly to control what work the new cell serves after addition.

### Scaling recommendations

A single-cell master setup is sufficient for most clusters. Consider adding secondary chunk-host cells when:

- The automaton thread CPU on the primary cell is consistently above 70–80%.
- Master RSS memory is approaching the safety margin.
- Node registration and disposal operations are causing significant latency spikes (visible as long mutation queues in logs).

Starting with three secondary chunk-host cells provides a practical balance between operational complexity and capacity headroom. Up to 48 secondary cells are supported.

When new secondary cells are added, existing chunks do not automatically move — only new chunks are assigned to the new cells. To rebalance existing data, rewrite it into newly created tables (for example, via merge or copy-based workflows) so that new chunks are allocated under the current placement rules. Do not attempt to move an existing table's chunk tree by changing `external_cell_tag`: this attribute is not a supported knob for rebalancing existing tables.
