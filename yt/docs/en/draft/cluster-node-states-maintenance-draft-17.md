# Cluster and node states, locks, and maintenance (draft)

This page is a compact operator cheat sheet for the most common cluster-wide switches, per-node flags, node lifecycle states, and the maintenance API. It is intentionally practical: use it to choose the smallest safe action before stopping, draining, isolating, or recovering cluster components.

## Cheat sheet

### Cluster-wide controls

| Control | Where to look or change | Scope | Effect | Typical use | How to undo |
| --- | --- | --- | --- | --- | --- |
| `hydra_read_only` / read-only mode | `//sys/@hydra_read_only`; `yt-admin build-master-snapshots --read-only`; `yt-admin master-exit-read-only` | Master cells | Forbids ordinary mutating requests while allowing reads and special administrative requests. | Master snapshots, emergency write freeze, investigations that need a stable master state. | `yt-admin master-exit-read-only` or `yt-admin exit-read-only --cell-id <cell-id>`. |
| `enable_safe_mode` | `//sys/@config/enable_safe_mode` | Cluster clients and services that honor safe mode | Rejects non-read-only requests from everyone except super-users; the `replicator` user is excluded from this exception. | Emergency stop of user writes or automated actions without putting Hydra itself into read-only mode. | Set the dynamic config flag back to `false`. |
| `provision_lock` | `//sys/@provision_lock` | Node registration safety gate | Refuses node registration while a fresh master instance may point at the wrong snapshot/changelog directories. | Fresh cluster initialization safety check. | Remove only after verifying the master data directories: `yt remove //sys/@provision_lock`. |

### Per-node operational flags

All flags below are node attributes under `//sys/cluster_nodes/<address>/@...`. Prefer the maintenance API over setting boolean attributes directly: it records the user, comment, type, and request id and makes cleanup auditable.

| Flag / maintenance type | Main effect | Data node impact | Exec node impact | Tablet/chaos impact | Use when | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| `banned` / `ban` | Excludes the node from normal service. | New reads/writes should avoid the node; replicas on it are treated as unavailable. | Scheduler should not run jobs there. | Cells should not be placed there. | Node is unhealthy, suspected corrupt, or must be isolated immediately. | Strongest per-node isolation; may trigger repair/replication pressure. |
| `decommissioned` / `decommission` | Drains long-lived ownership from the node. | Replicas are moved away over time. | Jobs are disabled as part of decommissioning on exec nodes. | Tablet cells/peers are moved away when possible. | Permanent removal, host evacuation, capacity reshuffle. | Prefer this for planned removal; wait for drain before powering off. |
| `disable_write_sessions` | Prevents new chunk write sessions on the node. | Existing replicas remain readable; new chunk writes avoid the node. | No direct effect unless the node also stores chunks. | No direct effect. | Disk maintenance, local IO problems, or graceful data-node drain without banning reads. | Less disruptive than `ban`; does not by itself move old replicas away. |
| `disable_scheduler_jobs` | Prevents new scheduler jobs on the node. | No direct storage effect. | Scheduler stops assigning new jobs; existing jobs may finish or be aborted depending on higher-level policy. | No direct effect. | Compute-only drain, kernel upgrade, CPU/memory issue. | For exec nodes, this is the usual graceful first step before restart. |
| `disable_tablet_cells` | Prevents tablet cell placement on the node. | No direct chunk-storage effect. | No direct scheduler effect. | Tablet cells should be moved away or not assigned. | Tablet-node maintenance or bundle reshaping. | Use instead of `ban` when only tablet workload must be drained. |
| `pending_restart` | Extends the node lease and marks the outage as expected. | Temporarily unavailable replicas are handled specially by chunk repair. | Avoids treating a short restart as full disappearance. | Useful when the same physical node hosts multiple roles. | Short rolling restart where the node is expected to return soon. | Time-limited; unsafe for long outages. Default timeout is configured at `//sys/@config/node_tracker/pending_restart_lease_timeout`. |
| `maintenance_requests` | Map of active maintenance requests keyed by maintenance id/type/user/comment. | Describes why flags are active. | Describes why flags are active. | Describes why flags are active. | Auditing and cleanup. | A flag may stay active while at least one request of that type remains. |

### Node lifecycle states

The node tracker state is exposed as `//sys/cluster_nodes/<address>/@state`.

| State | Meaning | Usual interpretation |
| --- | --- | --- |
| `offline` | Node object is known, but the node is not currently registered. | Process is down, disconnected, or has not completed registration. |
| `registered` | Node registered with masters but has not reported all expected heartbeat types yet. | Startup is in progress; wait before declaring it healthy. |
| `online` | Node registered and has reported every expected heartbeat type at least once. | Normal healthy serving state, subject to flags such as `banned` or `decommissioned`. |
| `restarted` | Node registered after restart but has not reported all expected heartbeats yet. | Transitional startup state after a known restart. |
| `unregistered` | Node lease ended or the node was unregistered, and the node is queued for disposal. | Can be caused by process stop, network partition, lease expiration, or planned removal; investigate before assuming it was intentional. |
| `being_disposed` | Disposal of an unregistered node is in progress. | Master is cleaning persistent node state. |
| `mixed` | Aggregated state differs between master cells. | Multi-cell convergence issue or propagation delay; inspect per-cell details. |
| `unknown` | Internal/default state. | Treat as diagnostic-only; do not build operational procedures around it. |

### Maintenance API quick reference

```bash
# Add a request. The result contains maintenance ids per affected target.
yt add-maintenance \
  --component cluster_node \
  --address my-node.example.net \
  --type disable_scheduler_jobs \
  --comment "drain before kernel upgrade"

# Add host-level maintenance; this expands to cluster nodes on the host.
yt add-maintenance \
  --component host \
  --address my-host.example.net \
  --type pending_restart \
  --comment "rack reboot"

# Inspect active requests and effective flags.
yt get //sys/cluster_nodes/my-node.example.net/@maintenance_requests
yt get //sys/cluster_nodes/my-node.example.net/@disable_scheduler_jobs

# Remove one request by id.
yt remove-maintenance \
  --component cluster_node \
  --address my-node.example.net \
  --id '<maintenance-id>'

# Remove requests by filter.
yt remove-maintenance --component cluster_node --address my-node.example.net --type pending_restart --mine
yt remove-maintenance --component cluster_node --address my-node.example.net --all
```

Supported components are `cluster_node`, `http_proxy`, `rpc_proxy`, and virtual component `host`. For `host`, requests expand to the cluster nodes on that host; proxy objects on the same host are not changed. Cluster nodes support `ban`, `decommission`, `disable_scheduler_jobs`, `disable_write_sessions`, `disable_tablet_cells`, and `pending_restart`. HTTP and RPC proxy maintenance is effectively ban-only: non-`ban` proxy requests may be recorded in the ledger, but they do not apply the role-specific effects described for cluster nodes.

## Explanations for non-trivial cases

### Read-only mode vs. safe mode

Read-only mode is a Hydra/master-cell property. It is the stronger consistency-oriented switch: ordinary mutations are rejected at the replicated state-machine level, and operators commonly enter it while building snapshots or freezing master state.

Safe mode is a policy/configuration switch. Services and clients check `//sys/@config/enable_safe_mode` and reject non-read-only requests from everyone except super-users; the `replicator` user is excluded from this exception. It is useful when you want a reversible operational brake while keeping the masters writable for carefully chosen administrative work by trusted operators.

Rule of thumb: use read-only mode for master maintenance and snapshot workflows; use safe mode to stop broad classes of user or automation activity while preserving an emergency admin escape hatch.

### `ban`, `decommission`, and partial drains

`ban` is isolation. It is appropriate when the node should stop serving immediately, for example after suspected data corruption, persistent crashes, or bad hardware. Because replicas or workloads on the node become unavailable for scheduling/placement decisions, banning many nodes can create repair storms or reduce fault tolerance.

`decommission` is evacuation. It is appropriate when the node should leave the cluster permanently or for a long time. The system can move replicas, jobs, and cells away in a more controlled way. For planned work, decommission first, wait for drain, and only then stop the process or remove the host.

Partial drain flags are more precise:

* use `disable_scheduler_jobs` for compute maintenance;
* use `disable_write_sessions` for storage write drain while preserving reads;
* use `disable_tablet_cells` for tablet workload relocation;
* use `pending_restart` for short expected restarts that should not trigger full repair behavior.

### Why `pending_restart` is not a drain

`pending_restart` tells the master that a short disappearance is expected. It extends the node lease and changes how chunk replicas on the node are interpreted while the node is temporarily unavailable. It does not mean the node is safe to keep down indefinitely, nor does it evacuate data. If the maintenance may exceed the configured lease timeout, drain with more specific flags or decommission instead.

A safe rolling restart pattern is:

1. Add `pending_restart` with a clear comment.
2. Add role-specific drains if needed, for example `disable_scheduler_jobs` for exec nodes.
3. Restart only a fault-domain-safe batch, such as one rack at a time when replication policy permits it.
4. Confirm that the restarted process really reconnected by checking heartbeat freshness, last-seen time, incarnation/build information, or service-level health. Do not use `@state == online` alone while `pending_restart` is active: the extended lease can keep the node `online` while the process is still stopped.
5. Remove the maintenance request ids that were created for the node or host.

### Effective flags vs. maintenance requests

The boolean attributes (`@banned`, `@decommissioned`, and so on) are effective state. `@maintenance_requests` is the reason ledger. Multiple requests can contribute to the same effective flag. Removing one request clears the flag only if no other active request still requires it.

This is why cleanup should normally remove the exact ids returned by `add-maintenance`. Removing by `--type`, `--mine`, `--user`, or `--all` is useful during recovery, but it is broader and should be used deliberately.

### Node state is not health by itself

`online` only means that registration and expected heartbeats have happened. A node can be `online` and still be `banned`, `decommissioned`, or disabled for scheduler jobs, write sessions, or tablet cells. Conversely, `registered` and `restarted` are often normal startup transients. Always inspect both lifecycle state and operational flags:

```bash
yt get //sys/cluster_nodes/<address>/@state
yt get //sys/cluster_nodes/<address>/@banned
yt get //sys/cluster_nodes/<address>/@decommissioned
yt get //sys/cluster_nodes/<address>/@maintenance_requests
```

### Node registration and unregistration

Node registration starts when a cluster node process connects to the primary master and sends its addresses, flavors, tags, lease transaction id, build information, and chunk-location information. The master rejects registration on secondary masters, rejects banned existing nodes, and also refuses registration while `//sys/@provision_lock` is present.

During successful registration the master creates or updates the node object, attaches it to the host and address maps, records register and last-seen time, stores the lease transaction, sets the local state to `registered`, and then waits for the expected heartbeat types. After all required heartbeats for the node's roles have been observed, the node becomes `online`. A restarted node can pass through `restarted` before the heartbeat set is complete.

Unregistration is most often driven by the lease transaction: if the process stops, loses connectivity, or fails to renew the lease until the transaction finishes, the master unregisters the node. Unregistration can also be requested explicitly. In both cases the master aborts or forgets the lease transaction, sets the local state to `unregistered`, clears reported heartbeats, fires node-unregistered notifications, propagates the change to other cells when needed, and queues the node for disposal. Therefore `unregistered` is a symptom to investigate, not proof that somebody intentionally removed the node.

### Provision lock

`provision_lock` is the boolean `//sys/@provision_lock` safety attribute created during world initialization when provision locking is enabled. While it is present, node registration fails with a warning that the masters may be fresh instances pointed at wrong snapshot or changelog directories. Removing it is not releasing an automation transaction; it is acknowledging the safety warning. Remove it only after verifying that the master snapshot/changelog directories are correct and that starting nodes against this master cannot cause unrecoverable data loss:

```bash
yt remove //sys/@provision_lock
```
