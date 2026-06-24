# Cluster and node states, locks, and maintenance (draft)

This page is a compact operator cheat sheet for the most common cluster-wide switches, per-node flags, node lifecycle states, and the maintenance API. It is intentionally practical: use it to choose the smallest safe action before stopping, draining, isolating, or recovering cluster components.

## Cheat sheet

### Cluster-wide controls

| Control | Where to look or change | Scope | Effect | Typical use | How to undo |
| --- | --- | --- | --- | --- | --- |
| `hydra_read_only` / read-only mode | `//sys/@hydra_read_only`; `yt-admin build-master-snapshots --read-only`; `yt-admin master-exit-read-only` | Master cells | Forbids ordinary mutating requests while allowing reads and special administrative requests. | Master snapshots, emergency write freeze, investigations that need a stable master state. | `yt-admin master-exit-read-only` or `yt-admin exit-read-only --cell-id <cell-id>`. |
| `enable_safe_mode` | `//sys/@config/enable_safe_mode` | Cluster clients and services that honor safe mode | Rejects non-read-only requests from everyone except super-users; the `replicator` user is excluded from this exception. | Emergency stop of user writes or automated actions without putting Hydra itself into read-only mode. | Set the dynamic config flag back to `false`. |
| `provision_lock` | `//sys/@provision_lock` | Cluster node registration | Fresh-master safety attribute: while it is `true`, node registration is refused to prevent accidental startup against incomplete or wrong snapshot/changelog directories. | Initial cluster provisioning or disaster-recovery safety check. | Remove only with `yt remove //sys/@provision_lock` after verifying the master data directories and intended cluster identity. |

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
| `unregistered` | Node was unregistered and queued for disposal, either explicitly or because its lease transaction finished. | Investigate as a lost node unless you know the unregistration was intentional. |
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

# Add host-level maintenance; this expands to cluster nodes registered on the host.
yt add-maintenance \
  --component host \
  --address my-host.example.net \
  --type pending_restart \
  --comment "rack reboot"

# Ban an HTTP or RPC proxy from service discovery / balancing.
yt add-maintenance \
  --component http_proxy \
  --address my-http-proxy.example.net:80 \
  --type ban \
  --comment "proxy restart"
yt add-maintenance \
  --component rpc_proxy \
  --address my-rpc-proxy.example.net:9013 \
  --type ban \
  --comment "proxy restart"

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

Supported components are `cluster_node`, `http_proxy`, `rpc_proxy`, and virtual component `host`. For `cluster_node` and `host`, supported maintenance types are `ban`, `decommission`, `disable_scheduler_jobs`, `disable_write_sessions`, `disable_tablet_cells`, and `pending_restart`; `host` expands only to cluster nodes registered on that host. For `http_proxy` and `rpc_proxy`, use `ban`: proxy maintenance targets expose only `@banned` and `@maintenance_requests`, so drain-style node maintenance types are not meaningful for them.

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
4. Wait for reconnection evidence such as fresh heartbeats, updated last-seen/incarnation data, or service-level checks; do not rely on `@state` alone while `pending_restart` keeps the lease alive.
5. Remove the maintenance request ids that were created for the node or host.

### Effective flags vs. maintenance requests

The boolean attributes (`@banned`, `@decommissioned`, and so on) are effective state. `@maintenance_requests` is the reason ledger. Multiple requests can contribute to the same effective flag. Removing one request clears the flag only if no other active request still requires it.

This is why cleanup should normally remove the exact ids returned by `add-maintenance`. Removing by `--type`, `--mine`, `--user`, or `--all` is useful during recovery, but it is broader and should be used deliberately.

### HTTP and RPC proxy maintenance

HTTP proxies and RPC proxies are registered in Cypress under `//sys/http_proxies/<address>` and `//sys/rpc_proxies/<address>`. These entries are maintenance targets too, but they are lighter-weight than cluster nodes: they store `@banned` and `@maintenance_requests`; they do not have chunk, job, tablet-cell, or node-lease state.

Use `ban` for proxy maintenance. A banned proxy should be excluded from normal proxy selection and balancing, which is the right action for proxy restarts, network isolation, or bad frontend behavior. There is no proxy equivalent of `decommission`, `disable_scheduler_jobs`, `disable_write_sessions`, `disable_tablet_cells`, or `pending_restart`; those flags describe cluster-node responsibilities that HTTP/RPC proxies do not own.

Proxy maintenance is addressed by the exact proxy address stored in the corresponding Cypress directory. Inspect and clean it the same way as node maintenance, but under the proxy path:

```bash
yt list //sys/http_proxies
yt get //sys/http_proxies/<address>/@banned
yt get //sys/http_proxies/<address>/@maintenance_requests
yt remove-maintenance --component http_proxy --address <address> --id '<maintenance-id>'

yt list //sys/rpc_proxies
yt get //sys/rpc_proxies/<address>/@banned
yt get //sys/rpc_proxies/<address>/@maintenance_requests
yt remove-maintenance --component rpc_proxy --address <address> --id '<maintenance-id>'
```

### Node registration and unregistration

Registration starts when a cluster node connects to the masters and obtains a lease transaction. The node appears under `//sys/cluster_nodes/<address>` and moves through startup states while it reports the expected heartbeat types. A freshly connected node is typically `registered`; it becomes `online` only after all expected heartbeat types have been observed. After a known restart, the tracker can mark it `restarted` until those heartbeats arrive again.

Unregistration is tied to that lease. If the process is stopped, partitioned, or otherwise loses the lease long enough for the transaction to finish, the node tracker unregisters the node, clears reported heartbeat state, sets `@state` to `unregistered`, and queues disposal. Explicit unregister operations use the same state transition. Therefore, `unregistered` means the node is gone from the tracker's active set; it does not by itself prove an operator intentionally removed it.

During a `pending_restart`, the lease can be extended precisely so short planned restarts do not immediately look like full disappearance. This also means `@state` may remain misleadingly healthy while the process is actually down. For rolling restart automation, combine maintenance ids with heartbeat freshness, incarnation/connection-time changes, logs, and service-level probes before clearing the request or moving to the next batch.

### Node state is not health by itself

`online` only means that registration and expected heartbeats have happened. A node can be `online` and still be `banned`, `decommissioned`, or disabled for scheduler jobs, write sessions, or tablet cells. Conversely, `registered` and `restarted` are often normal startup transients. Always inspect both lifecycle state and operational flags:

```bash
yt get //sys/cluster_nodes/<address>/@state
yt get //sys/cluster_nodes/<address>/@banned
yt get //sys/cluster_nodes/<address>/@decommissioned
yt get //sys/cluster_nodes/<address>/@maintenance_requests
```

### Provision lock

`//sys/@provision_lock` is not a normal Cypress lock and does not have an owner transaction to release. It is a boolean safety attribute created during cluster world initialization when provision locking is enabled. While it is present and `true`, cluster node registration is blocked so that operators do not accidentally attach nodes to masters whose snapshot/changelog directories still need verification.

Treat this attribute as a fresh-master or recovery guard. Remove it only after confirming that the cluster was intentionally initialized and that the master data directories are correct; the manual cleanup command is `yt remove //sys/@provision_lock`.
