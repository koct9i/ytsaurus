# CRI job execution environment setup

{% note info "Draft" %}

This article describes the Linux and {{product-name}} settings required to run jobs on an exec node through the Container Runtime Interface (CRI). It assumes `containerd` and cgroup v2. Names and defaults may change while CRI support is being developed.

{% endnote %}

## Architecture

An exec node does not ask Kubernetes to create a pod for every job. The exec node talks directly to a CRI-compatible runtime over its Unix sockets. In the Kubernetes deployment, the exec node pod contains two containers:

* `ytserver` runs the exec node process;
* `jobs` runs `containerd` and the job containers created by it.

The containers share the runtime socket (by default `/config/containerd.sock`) and the host paths used for slots and images. At startup, the exec node creates one CRI `PodSandbox` for every user slot. For each job, it selects a slot and starts a container in that slot's sandbox. The job image contains the user-space environment, while bind mounts expose the slot directory, downloaded artifacts, runtime sockets, and explicitly configured host paths.

The job proxy is the process that prepares and supervises the user process and reports statistics. In the current CRI implementation, the job proxy and the user process run in the same job container. They therefore share the container's namespaces and resource cgroup; CRI does not provide a security boundary between them. Treat the selected image and every configured bind mount as part of the trusted job environment.

The lifecycle of a slot is:

1. The exec node initializes the CRI client and image cache and pulls the configured job-proxy image.
2. It removes stale objects from its CRI namespace and creates a `PodSandbox` for each slot.
3. Before a job starts, its image is resolved or pulled and its workspace is mounted into the slot.
4. The exec node starts the job-proxy container with the job's CPU, memory, device, and NUMA settings.
5. When the job ends, the exec node stops the container, unmounts the workspace, and cleans the sandbox before reusing the slot.

## Prerequisites

Before enabling CRI, prepare every exec-node host as follows:

* Use a Linux kernel with cgroup v2 enabled and mount a writable, delegated cgroup subtree for the `jobs` container. The runtime must be able to create child cgroups and enable the `cpu`, `cpuset`, and `memory` controllers.
* Run a CRI-compatible `containerd` in the `jobs` container and make both its runtime and image-service sockets visible in `ytserver`.
* Give `containerd` the privileges and mount propagation needed to create namespaces, overlay mounts, cgroups, and device bindings. The slot-volume mount shared with `ytserver` must use bidirectional mount propagation if jobs use tmpfs or other non-root volumes.
* Put the containerd root/image cache on a real host filesystem. Do not put it on the pod's overlay root filesystem: overlayfs cannot reliably be mounted on top of overlayfs.
* Ensure that slot directories and every configured bind-mount source have identical absolute paths in `ytserver` and `jobs`. CRI bind mounts are interpreted by the runtime, not by the exec-node container.
* Allow the exec node's effective user or supplementary group to read and write the CRI sockets. Do not make the sockets generally writable.
* Configure registry connectivity and credentials for every image registry used by jobs. Production deployments should use immutable image digests where reproducibility is important.

The `containerd` CRI plugin and its chosen runtime handler must use the same cgroup driver as the surrounding Kubernetes node. With cgroup v2, a systemd-based deployment should normally use systemd cgroups throughout. A mixture of cgroupfs and systemd ownership can put job processes outside the subtree observed by the exec node.

## Minimal operator configuration

For a cluster managed by the {{product-name}} Kubernetes operator, enable the CRI sidecar, reserve separate resources for it, and add an image-cache location:

```yaml
execNodes:
  - name: compute
    instanceCount: 3

    jobEnvironment:
      cri:
        criService: containerd
        apiRetryTimeoutSeconds: 180

    # Resources consumed by ytserver and the pod's supporting processes.
    resources:
      limits:
        cpu: 2
        memory: 10G

    # Capacity advertised for jobs and assigned to the jobs container.
    jobResources:
      limits:
        cpu: 8
        memory: 40G

    locations:
      - locationType: Slots
        path: /yt/node-data/slots
      - locationType: ImageCache
        path: /yt/node-data/image-cache
```

In this example the pod needs at least 10 CPU cores and 50 GB of memory, plus any deployment overhead. `resources` and `jobResources` are not two limits on the same processes: the first protects the node daemon, and the second defines the budget from which jobs are scheduled. Size both explicitly so that Kubernetes does not OOM-kill the pod before {{product-name}} can enforce a job limit.

The operator translates this section into the exec-node `slot_manager.job_environment` CRI configuration and creates the `jobs` sidecar. If you manage exec nodes without the operator, you must provide the sidecar, sockets, shared mounts, cgroup delegation, and the equivalent node configuration yourself. Do not copy the operator YAML directly into a raw node config; they use different schemas.

## Containers and images

`job_proxy_image` is the fallback image and the image used for workspace setup. A job can select another image with the `docker_image` field in its operation spec. The exec node pulls images through the CRI image service and keeps their metadata in its image cache.

Plan image handling before admitting jobs:

* Pin a tested job-proxy image and keep it available on all exec nodes.
* Decide which registry prefixes the exec node manages. Managed images can be pulled and evicted by the CRI image cache; unmanaged images must already exist in the runtime and are neither accounted nor removed by {{product-name}}.
* Allocate the `ImageCache` location for both compressed downloads and unpacked snapshots. Unpacked images can be several times larger than registry blobs.
* Prefer digests to mutable tags. In particular, a `latest` tag may be refreshed and yield different root filesystems across nodes.
* Keep bind mounts narrow and read-only unless the job must write. A writable host bind bypasses the immutability of the image and can expose node state.

By default, the exec node bind-mounts the `ytserver-job-proxy`, `ytserver-exec`, and `ytserver-tools` binaries into the image. An image may carry its own matching binaries instead, but the image and exec-node versions must then be kept compatible. Additional job-proxy bind mounts are an optional escape hatch for certificates, libraries, or site configuration; they should not be used as a substitute for building a reproducible image.

## Cgroups and resource control

There are three resource-control layers:

1. **Kubernetes pod/container limits** cap the complete `ytserver` and `jobs` containers. They are the final host-level safety boundary.
2. **Exec-node capacity** (`jobResources` in the operator spec and `@resource_limits` on the node) tells the scheduler how much CPU, memory, GPU, and how many user slots may be allocated.
3. **Per-job CRI limits** are written into the OCI container specification and implemented by cgroup v2.

All three layers must agree. Advertising more memory or CPU than the `jobs` container can use causes pod-level throttling or OOM kills that {{product-name}} cannot attribute cleanly to a job. Advertising substantially less wastes the reserved capacity.

### CPU

Each job requests `cpu_limit` in its operation spec. The scheduler accounts that request, and CRI converts a configured hard limit to a cgroup CPU quota using its `cpu_period` (100 ms by default). CPU sets can further restrict the job to selected logical processors when NUMA scheduling is enabled.

CPU quota and CPU affinity solve different problems. Quota limits total time across the allowed CPUs; `cpuset.cpus` controls where that time may run. Leave headroom for `containerd`, shim processes, filesystem work, and the job proxy instead of assigning every host CPU to jobs.

### Memory

The operation's `memory_limit`, system buffers, the job proxy, and tmpfs usage contribute to the memory reserved for a job. The job container receives a cgroup memory limit for the slot. With cgroup v2, enabling `memory_oom_group` asks the kernel to kill the container's tasks as a group on cgroup OOM, avoiding a half-alive job proxy or user-process tree.

Do not confuse three different events:

* **{{product-name}} memory overdraft:** the job proxy observes usage above the job allowance and reports or aborts the job;
* **job-cgroup OOM:** the kernel enforces the per-job `memory.max` and records an OOM event in that cgroup;
* **pod OOM:** the complete `jobs` container exceeds its Kubernetes limit, so Kubernetes may report an OOM without identifying the responsible job.

Keep a safety margin between the sum of advertised job memory and the `jobs` container limit. The margin covers runtime processes, page tables, kernel memory charged to the cgroup, image operations, and short-lived overlap during container cleanup.

## Memory tracking

The job proxy periodically inspects the user process tree and reports memory statistics. By default, memory-mapped files are included. This gives {{product-name}} job-level diagnostics and overdraft handling in addition to the kernel's cgroup enforcement.

For workloads that use tmpfs, shared mappings, or unusual process trees, validate the reported values against `memory.current`, `memory.stat`, and `memory.events` in the job cgroup. The optional smaps-based tracker gives more detailed process accounting but costs additional CPU and `/proc` reads. Its statistics cache period can reduce that overhead at the cost of less immediate readings.

Memory used by tmpfs is real memory and is charged to a cgroup. A job that requests tmpfs must reserve both an appropriate `tmpfs_size` and enough job memory. Do not count the same capacity as freely available disk space. If non-root volumes are enabled, configure bidirectional mount propagation; otherwise disable them and reject operation features that depend on those mounts.

## NUMA placement

NUMA scheduling is optional and is useful only on hosts with multiple NUMA nodes and a deliberately partitioned CPU layout. Describe each NUMA node in the static slot-manager configuration:

```yson
slot_manager = {
    numa_nodes = [
        {numa_node_id = 0; cpu_count = 16; cpu_set = "0-15";};
        {numa_node_id = 1; cpu_count = 16; cpu_set = "16-31";};
    ];
};
```

Then enable `exec_node.slot_manager.enable_numa_node_scheduling` in the dynamic node configuration. The slot manager chooses a NUMA node with enough free CPU and passes its CPU set to the CRI container. If no configured NUMA node can satisfy the request, the job may run without an affinity rather than being made local by force.

This setting provides CPU affinity; it does not by itself guarantee strict memory-node binding. Check the CRI runtime and workload policy if the application requires `cpuset.mems`, `mbind`, huge pages, or device locality. Keep the configured CPU sets disjoint, exclude CPUs reserved for the OS and node daemons, and align GPU or high-speed-network devices with their nearest NUMA node where possible.

## Optional features

Enable optional features one at a time and test them on a canary exec node:

* **Non-root volumes and tmpfs.** Require shared slot mounts with bidirectional propagation. Disable `enable_non_root_volumes` if that cannot be guaranteed.
* **Private registries.** Supply credentials through the supported secret mechanism; never embed long-lived passwords in operation specifications or world-readable node configuration.
* **GPU jobs.** Configure the appropriate CRI runtime handler and NVIDIA runtime on every GPU node. The exec node passes the selected GPU indexes through `NVIDIA_VISIBLE_DEVICES`, binds detected InfiniBand devices, and adds `IPC_LOCK` when required. Verify that an empty device selection does not expose every GPU.
* **InfiniBand/RDMA.** Device nodes are writable binds, so expose them only on a dedicated, trusted node group. Account for locked memory and NUMA locality.
* **Job shells and tracing.** CRI job containers receive `SYS_PTRACE` for job-shell diagnostics. Review this capability against the cluster's trust model and runtime security profile.
* **Custom runtime handlers.** A CRI runtime handler can select an alternative OCI runtime or runtime class. All nodes in the scheduling group must provide the same named handler.
* **Image-cache policy.** Managed/unmanaged registry prefixes, pinned images, periodic pulls, and `always_pull_latest` trade disk use and startup latency against freshness.
* **Group access to CRI.** If job sidecars must call the runtime, `container_user_group_name` can add the socket-owning group. This grants powerful container-management access and should not be enabled for untrusted user code.

## Rollout and verification

Roll out to a small node group first. After applying the configuration, verify the pod, runtime, cgroups, and {{product-name}} state in that order:

```bash
# Both containers are ready and neither is being OOM-killed or restarted.
kubectl get pod <pod-name>
kubectl logs --container jobs <pod-name>

# The socket is visible from ytserver.
kubectl exec --container ytserver <pod-name> -- test -S /config/containerd.sock

# containerd has the CRI plugin and expected runtime handler.
kubectl exec --container jobs <pod-name> -- crictl info
```

Then inspect the exec node:

```bash
yt get //sys/exec_nodes/<node-address>/@state
yt get //sys/exec_nodes/<node-address>/@alerts
yt get //sys/exec_nodes/<node-address>/@resource_limits
```

The expected state is `"online"`, alerts are `[]`, and the advertised CPU, memory, GPU, and user-slot limits do not exceed the job-sidecar budget. Run a smoke job with an explicit image:

```bash
yt vanilla \
  --proxy <cluster-address> \
  --tasks '{task={command="cat /proc/self/cgroup; sleep 1"; job_count=1; docker_image="docker.io/library/ubuntu:24.04"}}'
```

For a longer canary, inspect the container from `crictl`, confirm that its cgroup is below the configured base cgroup, and compare its CPU and memory settings with the operation specification. Also test an intentional memory overrun on an isolated node to confirm that only the job cgroup, not the complete `jobs` sidecar, is killed.

## Troubleshooting

| Symptom | Likely cause | What to check |
| --- | --- | --- |
| Exec node remains offline or reports a CRI initialization alert | Socket missing, permission denied, CRI plugin not ready, or wrong endpoint | `jobs` logs, socket ownership, `crictl info`, and the runtime/image endpoint paths |
| `failed to mount rootfs ... invalid argument` | Containerd root is on overlayfs or mount propagation is wrong | Filesystem type of the `ImageCache` location and shared-volume propagation |
| Job starts but has no effective CPU or memory limit | Cgroup controllers were not delegated or runtime cgroup driver differs | `cgroup.controllers`, `cgroup.subtree_control`, runtime settings, and the job's OCI/cgroup files |
| Whole `jobs` container is OOM-killed | Advertised job memory plus overhead exceeds the sidecar limit | Kubernetes events, sidecar limit, node `@resource_limits`, and per-job `memory.events` |
| Memory statistic differs from cgroup usage | Page cache, tmpfs, mappings, kernel charges, or sampling delay | Job statistics, tracker settings, `memory.current`, `memory.stat`, and process `smaps` |
| NUMA-enabled job runs on unexpected CPUs | Dynamic flag disabled, CPU set invalid, or no NUMA node had enough free CPU | Slot-manager dynamic config and the NUMA state in the exec-node orchid |
| Image pulls repeatedly or disk fills | Mutable tags, undersized cache, or unsuitable managed-prefix policy | Image-cache policy, pinned images, free space, and containerd content/snapshot usage |
| Cleanup failures accumulate stale sandboxes | Runtime/shim failure or busy mounts | Exec-node and containerd logs, CRI pod list, mount propagation, and open files under the slot |

Do not delete runtime state or cgroups while the exec node is serving jobs. Drain or disable scheduling on the node first, stop the exec node, and only then clean stale CRI objects. After recovery, recheck alerts and run the smoke operation before returning the node to the main scheduling group.

See the operator's [CRI cluster configuration example](https://github.com/ytsaurus/ytsaurus-k8s-operator/blob/main/config/samples/cluster_v1_cri.yaml) for a complete deployment manifest.
