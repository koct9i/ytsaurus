# Setting up and using ytprof to debug ytserver processes

> **Draft metadata**
>
> - **Draft number:** 18
> - **Author:** AI agent (OpenAI GPT-5.5)
> - **Created:** 2026-07-09
> - **Status:** In progress; requires review by YT server/runtime maintainers.

`ytprof` is the profiling endpoint built into {{product-name}} server binaries. It exposes pprof-compatible CPU, memory, allocation, and spinlock profiles over the component monitoring HTTP server. Use it when a `ytserver-*` process is CPU-bound, grows memory unexpectedly, spends time allocating, or appears blocked on locks.

This page focuses on operational debugging of already running `ytserver` processes. For low-level implementation details and a standalone example, see the in-repository [`ytprof` README](../../../../yt/yt/library/ytprof/README.md).

## When to use ytprof

Use ytprof to answer questions such as:

- **Which functions consume CPU?** Capture `/ytprof/cpu/profile` or the compatibility endpoint `/ytprof/profile`.
- **Which allocations are still live?** Capture `/ytprof/tcmalloc/current` or `/ytprof/heap`.
- **What caused a memory peak that has already passed?** Capture `/ytprof/tcmalloc/peak` or `/ytprof/peak`.
- **Which code allocates and frees heavily in a hot loop?** Capture `/ytprof/tcmalloc/allocation` or `/ytprof/allocations`.
- **Is time lost on spinlocks?** Capture `/ytprof/spinlock/lock` for absl/tcmalloc spinlocks or `/ytprof/spinlock/block` for YT blocking/spinlock primitives.

A ytprof response is a gzipped pprof protobuf. The profile is intended to be self-contained: symbols and line information are added by the process before the response is returned, so the original binary is usually not needed for viewing the profile. Keeping a checkout of the matching source revision is still helpful for `pprof list` and source annotations.

## Prerequisites

### Binary and build type

CPU profiling is most useful with binaries built in profile mode:

```bash
ya make --build=profile --thinlto yt/yt/server/all/ytserver-all
```

Profile builds preserve enough information for useful stack traces and source-level analysis. The `--thinlto` flag is recommended because profile builds can otherwise be a few percent slower than regular release builds.

Memory profiling requires a tcmalloc-backed binary. The server binaries normally use the project allocator setup, but if you are profiling a custom component, make sure its build links tcmalloc and initializes allocation profiling early enough for long-lived objects to appear in heap snapshots.

### Monitoring HTTP endpoint

`ytprof` is registered under `/ytprof` on the component monitoring server on Linux. The same monitoring server also exposes endpoints such as `/orchid` and `/solomon`. To collect a profile you need:

1. the hostname or pod/container address of the target process;
2. the component monitoring port, not the RPC or HTTP-proxy user port;
3. network access from your shell to that monitoring endpoint.

For a manually started process, read `monitoring_port` from the component config. For an operator-managed or containerized cluster, port-forward or exec into a debugging container if the monitoring port is not exposed externally.

Example port-forward pattern:

```bash
kubectl -n <namespace> port-forward pod/<pod-name> 10010:<monitoring-port>
```

Then use `http://localhost:10010/ytprof/...` as the base URL.

### pprof tool

Use `ya tool pprof` from the repository root when possible:

```bash
ya tool pprof -symbolize=none -trim_path='/-S/:/-B/' ./profile.pb.gz
```

`-symbolize=none` avoids a second symbolization pass by pprof because ytprof already embeds symbolized locations. `-trim_path` helps pprof map build paths back to your local checkout. Adjust it if profiles were produced by CI, containers, or another source root layout.

## Quick start: CPU profile of a ytserver process

1. Choose the component and monitoring endpoint:

   ```bash
   export YTPROF=http://<host>:<monitoring-port>/ytprof
   ```

2. Capture a 30-second CPU profile:

   ```bash
   curl -fSL "$YTPROF/cpu/profile?d=30" -o cpu.pb.gz
   ```

   The compatibility endpoint is equivalent:

   ```bash
   curl -fSL "$YTPROF/profile?d=30" -o cpu.pb.gz
   ```

3. Open it in pprof:

   ```bash
   ya tool pprof -symbolize=none -trim_path='/-S/:/-B/' cpu.pb.gz
   ```

4. Start with the high-level commands:

   ```text
   (pprof) top
   (pprof) sort=cum
   (pprof) top
   (pprof) list SomeFunctionOrClass
   (pprof) svg
   ```

Interpretation tips:

- `flat` time points to functions where samples landed directly.
- `cum` time points to functions whose callees are expensive.
- If most samples are in compression, hashing, serialization, RPC parsing, or allocation functions, inspect the nearest YT caller in the cumulative stack.
- If CPU is expected to be idle but the profile is busy, also compare with `/orchid` thread-pool queues and component-specific profiler sensors.

## Endpoint reference

| Purpose | Preferred endpoint | Compatibility endpoint | Useful parameters |
| --- | --- | --- | --- |
| CPU profile | `/ytprof/cpu/profile` | `/ytprof/profile` | `d=<duration>`, `freq=<hz>`, `record_action_run_time=1`, `action_min_exec_time=<duration>` |
| Current live heap | `/ytprof/tcmalloc/current` | `/ytprof/heap` | none |
| Peak heap | `/ytprof/tcmalloc/peak` | `/ytprof/peak` | none |
| Allocation profile over an interval | `/ytprof/tcmalloc/allocation` | `/ytprof/allocations` | `d=<duration>` |
| TCMalloc fragmentation | `/ytprof/tcmalloc/fragmentation` | `/ytprof/fragmentation` | none |
| TCMalloc text stats | `/ytprof/tcmalloc/stat` | none | none |
| absl/tcmalloc spinlocks | `/ytprof/spinlock/lock` | none | `d=<duration>`, `frac=<sampling-fraction>` |
| YT blocking/spinlock primitives | `/ytprof/spinlock/block` | none | `d=<duration>`, `frac=<sampling-fraction>` |
| Running binary | `/ytprof/binary` | none | `check_build_id=<id>` |
| ytprof version | `/ytprof/version` | none | none |
| Build id | `/ytprof/build_id` | `/ytprof/buildid` | none |

Durations use YT duration syntax such as `15`, `30s`, `1m`, or `500ms` where accepted by the endpoint.

## CPU profiling workflow

### Basic capture

```bash
curl -fSL "$YTPROF/cpu/profile?d=30" -o cpu-$(date +%Y%m%d-%H%M%S).pb.gz
ya tool pprof -symbolize=none -trim_path='/-S/:/-B/' cpu-*.pb.gz
```

Use a duration long enough to catch the behavior but short enough to avoid mixing unrelated phases. For steady CPU saturation, 15-30 seconds is usually enough. For periodic stalls, profile across at least one full period.

### Sampling frequency

The default CPU sampling frequency is usually sufficient. Increase it only for short-lived events or when profiles are too sparse:

```bash
curl -fSL "$YTPROF/cpu/profile?d=15&freq=1000" -o cpu-high-frequency.pb.gz
```

Higher frequencies increase overhead and can perturb latency-sensitive components. Prefer a longer duration before increasing frequency on production processes.

### Long action analysis

YT thread pools can annotate CPU samples with action execution time. This is useful when you suspect a heavy callback blocks a pool thread for too long:

```bash
curl -fSL "$YTPROF/cpu/profile?d=30&record_action_run_time=1" -o cpu-actions.pb.gz
```

Inside pprof:

```text
(pprof) tagfocus=action_run_time_us=10000:
(pprof) top
(pprof) traces
```

To collect only samples from actions whose execution time exceeds a threshold:

```bash
curl -fSL "$YTPROF/cpu/profile?d=30&action_min_exec_time=10ms" -o cpu-long-actions.pb.gz
```

## Memory profiling workflow

### Current heap

Use current heap when RSS or allocator usage is high right now:

```bash
curl -fSL "$YTPROF/tcmalloc/current" -o heap-current.pb.gz
ya tool pprof -symbolize=none -trim_path='/-S/:/-B/' heap-current.pb.gz
```

Useful pprof commands:

```text
(pprof) top
(pprof) sort=cum
(pprof) top
(pprof) sample_index=0
(pprof) list SomeAllocatorCaller
(pprof) svg
```

`sample_index=0` switches to object count in profiles where the default view is bytes. This helps distinguish many tiny objects from fewer large allocations.

### Peak heap

Use peak heap after a spike has already subsided but the process retained a peak snapshot:

```bash
curl -fSL "$YTPROF/tcmalloc/peak" -o heap-peak.pb.gz
```

Compare current and peak profiles to decide whether the issue is a leak-like live heap, a transient burst, or allocator retention/fragmentation.

### Allocation profile

Use allocation profiling when CPU is spent allocating, but current heap does not show the culprit because objects are short-lived:

```bash
curl -fSL "$YTPROF/tcmalloc/allocation?d=60" -o allocations-60s.pb.gz
```

Look for constructors, serialization paths, temporary buffers, per-request vectors/maps, and compression buffers that appear high in cumulative allocation volume.

### Fragmentation and text stats

If RSS is high but live heap is not, inspect fragmentation and allocator stats:

```bash
curl -fSL "$YTPROF/tcmalloc/fragmentation" -o fragmentation.pb.gz
curl -fSL "$YTPROF/tcmalloc/stat" -o tcmalloc-stat.txt
```

Fragmentation profiles point at allocation sites responsible for partially used spans. Text stats help separate live heap, cached memory, metadata, and unmapped memory.

## Spinlock profiling workflow

Spinlock profiling is useful when CPU is high but stacks show synchronization internals, allocator locks, or contention rather than application work.

For absl/tcmalloc locks:

```bash
curl -fSL "$YTPROF/spinlock/lock?d=30" -o spinlock-lock.pb.gz
```

For YT blocking/spinlock primitives:

```bash
curl -fSL "$YTPROF/spinlock/block?d=30" -o spinlock-block.pb.gz
```

If the profile is too sparse, tune the sampling fraction:

```bash
curl -fSL "$YTPROF/spinlock/lock?d=30&frac=100" -o spinlock-lock-frac100.pb.gz
```

Review `top`, `sort=cum`, and `traces`. Lock profiles are most useful when you identify both the contended lock owner path and the waiting caller path; cross-check with thread-pool queues and component logs.

## Working with pprof output

Common interactive commands:

```text
(pprof) top                 # hottest functions by current sort/sample index
(pprof) sort=cum            # include callees when ranking
(pprof) list Regex          # annotated source for matching functions
(pprof) traces              # raw sampled stack traces
(pprof) svg                 # write a call graph SVG
(pprof) tagfocus=thread=... # keep samples with matching labels
(pprof) tagfocus=           # clear label filter
(pprof) sample_index=0      # switch sample value, often object count
(pprof) help                # pprof help
```

Run pprof from the repository root that matches the profiled binary revision. If source lines do not appear, inspect profile comments for build metadata and adjust `-trim_path` to remove build-system prefixes from recorded file paths.

## Production safety notes

- A single ytprof profile fetch per endpoint is allowed at a time; concurrent fetches may receive `429 Too Many Requests`.
- CPU, allocation, and spinlock interval profiles intentionally run for the requested duration and add sampling overhead during that interval.
- Avoid high `freq` values and long allocation profiles on latency-sensitive production masters, schedulers, and proxies unless the incident requires it.
- Save raw `.pb.gz` profiles before opening them. They are small enough to attach to incident tickets and can be reanalyzed later.
- Treat profiles as potentially sensitive: symbols, paths, labels, user tags, query strings, and allocation call stacks may reveal workload or deployment details.

## Troubleshooting

### `curl` returns 404

Check that you are using the monitoring port and the `/ytprof` prefix. Also verify that the target binary was built with monitoring handlers and is running on Linux.

### `curl` hangs for the requested duration

This is expected for interval endpoints such as CPU, allocation, and spinlock profiles. Snapshot endpoints such as current heap should return immediately unless symbolization is slow.

### pprof cannot find source files

Run pprof from the repository root and add or adjust `-trim_path`. Profiles from CI or container builds often include build-root prefixes that do not exist on your workstation.

### The profile is empty or too sparse

Increase duration first. For CPU profiles, increase `freq` only if a longer capture is not practical. For spinlock profiles, tune `frac`. For memory profiles, remember that `heap` shows live objects only; use allocation profiling for short-lived objects.

### Heap profile does not explain RSS

Compare current heap, peak heap, fragmentation, and `tcmalloc/stat`. RSS may include allocator caches, fragmentation, memory-mapped files, stacks, JIT/generated code, or other non-heap mappings.

## Minimal incident checklist

1. Identify the component, host/pod, PID, and monitoring port.
2. Save a CPU profile during the bad period:

   ```bash
   curl -fSL "$YTPROF/cpu/profile?d=30" -o cpu.pb.gz
   ```

3. Save memory profiles if RSS or allocator usage is suspicious:

   ```bash
   curl -fSL "$YTPROF/tcmalloc/current" -o heap-current.pb.gz
   curl -fSL "$YTPROF/tcmalloc/peak" -o heap-peak.pb.gz
   curl -fSL "$YTPROF/tcmalloc/stat" -o tcmalloc-stat.txt
   ```

4. If allocation churn is suspected, save an interval allocation profile:

   ```bash
   curl -fSL "$YTPROF/tcmalloc/allocation?d=60" -o allocations.pb.gz
   ```

5. Open profiles from the matching source checkout:

   ```bash
   ya tool pprof -symbolize=none -trim_path='/-S/:/-B/' cpu.pb.gz
   ```

6. Attach raw profiles, the target component config snippet containing `monitoring_port`, relevant logs, and a short interpretation to the incident or debugging ticket.
