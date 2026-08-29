# Synthetic workload for master cell addition

This plan describes a disposable workload for validating a master cell addition and stressing master metadata. It supplements the [master cell extension procedure](cell-addition.md); it does not replace it. Run it on a dedicated cluster, or under isolated accounts, bundles, and scheduler pools with explicit quotas. Never generate an unbounded workload on a production cluster.

## Goals and success criteria

The workload should answer four different questions:

1. **Coverage:** are representative native, replicated, and cell-local object types readable and mutable after the topology change?
2. **Relationships:** do references between objects (ACL subjects, links, schemas, accounts, bundles, transactions, replicas, and portals) survive?
3. **Scale:** can masters snapshot, recover, replicate global objects to the new cell, and process the expected object and chunk count with acceptable headroom?
4. **Activity:** after services return, do reads, writes, transactions, tablet operations, and scheduler operations continue without correctness errors?

Define pass/fail thresholds before the run. At minimum, require:

- every object in the generator manifest to exist with its expected type, object ID, selected attributes, and content digest;
- every recorded relationship to resolve to the expected object;
- no unexpected master alerts, stuck two-phase creation/removal, or permanently growing Hive queues;
- successful snapshots before and after cell addition, with every master peer returning to an active state;
- workload error rate, master memory, mutation latency, snapshot duration, and recovery duration within agreed limits.

Track Cypress nodes, global objects, transactions, chunks, chunk-list nodes, tablets, schemas, and payload bytes separately. A single object count hides which master subsystem is under load.

## Safety and repeatability

Create a run root such as `//tmp/master-cell-addition/<run-id>` and use the same unique prefix for objects that must live under `//sys`. Create dedicated accounts, bundles, scheduler pools, users, and groups. Put hard limits on node count, chunk count, disk space, tablet count, and master-memory use. An external watchdog must stop generators when any limit or cluster alert is reached.

The generator must:

- accept a seed and scale profile, producing the same logical graph for the same inputs;
- append every successful mutation to a durable JSON Lines manifest outside the tested cluster;
- record path, object ID, type, native cell tag, important attributes, relationship targets, row or byte count, and content checksum;
- use bounded batches and concurrency, retry only retryable errors, and validate an existing object before treating `already exists` as success;
- provide independent `create`, `verify`, `mutate`, and `cleanup` modes;
- attach `test_run_id`, `generator_version`, and `seed` attributes wherever permitted.

Keeping the manifest outside YTsaurus prevents a damaged namespace from making the verifier agree with damaged state. Cleanup must consume the manifest in reverse dependency order rather than recursively deleting an assumed prefix.

## Coverage model

Use pairwise combinations for the broad matrix, then add deliberate high-risk combinations. A Cartesian product of every type, codec, account, ACL, transaction state, and placement is costly and adds little coverage.

### Object families

| Family | Representative objects and operations | What it exercises |
|---|---|---|
| Cypress structure | Deep and wide `map_node` trees, `list_node`, scalar nodes, documents, opaque nodes, attributes, links (valid, dangling, and chains) | Namespace traversal, resolve logic, node-count and master-memory accounting |
| Static storage | Empty/non-empty files and journals; schemaless and schemaful sorted/ordered tables; shared schemas | Chunk ownership/lists, schemas, codecs, erasure, replication, account and medium requisitions |
| Dynamic storage | Sorted/ordered dynamic tables, mounted/unmounted states, multiple tablets, reshard, lookup/insert/delete/trim | Tablet cells, mount metadata, stores, and tablet transactions |
| Replication | Replicated tables and enabled/disabled replicas; chaos replicated tables when a chaos bundle is available | Global objects, references, chaos metadata, and cross-cluster configuration |
| Queue API | Queue tables, consumers, registrations, partitions, and producer sessions when enabled | Queue-specific attributes and relationships over dynamic tables |
| Security and quotas | Nested accounts, users, groups, memberships, allow/deny ACLs and inheritance, network projects | Globally replicated objects, two-phase lifecycle, permissions, and accounting |
| Placement topology | Media, racks, data centers, tablet/chaos bundles, areas, and cells when configured | Global topology objects and infrastructure references |
| Scheduler | Pool trees and operations in completed, aborted, and running states | Scheduler-pool objects, operation archive, and transaction interaction |
| Coordination | Master/tablet transactions, nested transactions, snapshot/shared/exclusive locks, and supported leases | Transaction coordinator role, prerequisites, branched nodes, and lock restoration |
| Multicell Cypress | Portal entrance/exit and subtrees on every Cypress-node-host cell | Cross-cell resolution, copy/move, ACL inheritance, and transaction boundaries |

Some system objects are created only by their owning component. Exercise them through the public lifecycle operation—for example, create a tablet cell through its bundle or start an operation through the scheduler—rather than forging objects by ID. Report unsupported optional features as skipped; do not silently substitute another type.

### Combination dimensions

Distribute each applicable family across:

- empty, tiny, medium, and boundary-sized payloads;
- shallow/wide and deep/narrow namespace shapes;
- default/custom account, ACL, medium, replication factor, compression and erasure codec, schema, and optimization mode;
- primary-cell paths, every existing portal, and a new-cell portal after role assignment;
- standalone creation and creation inside committed, aborted, and open transactions;
- no custom attributes, many small attributes, and a few large attributes;
- hot objects continuously read or mutated and cold objects retained for checksum comparison;
- same-cell and cross-cell references, including links, schemas, ACL subjects, replicas, and copy/move.

Reserve cases for empty objects, Unicode and escaped names, maximum supported path depth and attribute size (below configured limits), broken links, an open transaction spanning the maintenance boundary, an unmounted dynamic table, a mounted table with stores, and immutable objects during the snapshot. Derive boundaries from deployed configuration instead of embedding release-specific constants.

## Scale profiles

Separate metadata pressure from data-volume pressure so that failures are attributable.

| Profile | Dominant generated state | Purpose |
|---|---|---|
| Functional | Tens of every supported type and pairwise combinations | Fast correctness gate |
| Namespace | Millions of small nodes, links, attributes, ACLs, and directory children across accounts/portals | Cypress count, resolution, snapshot size, master memory |
| Chunk metadata | Many small table/file writes and tablets/stores; intentionally small chunks only in isolation | Chunk/list count, requisitions, chunk hosting, heartbeats |
| Data volume | Fewer objects with large, compressibility-controlled payloads and production-size chunks | Disk/network throughput and replication without excessive nodes |
| Global objects | A safe high count of accounts, users/groups, schemas, bundles, cells, and other replicated types | Registration-time replication and two-phase lifecycle |
| Mixed soak | Production-like weighted mix plus reads, writes, transactions, mounts, and operations | Leaks, queue growth, latency, and convergence |

Express targets as an absolute value and a fraction of a known safe cluster limit. Ramp through 1%, 5%, 10%, 25%, and the approved target, pausing for snapshot, verification, and resource review. Stop at a predeclared memory, disk, RPC latency, mutation queue, snapshot-time, or alert threshold.

### Efficient generation

- Batch namespace requests with 10–100 bounded workers. Shard paths by a stable hash so retries and verification resume independently.
- Generate both wide directories (child maps) and deep trees (path resolution).
- Increase **chunk count** with separately committed small batches or flushed dynamic stores. Increase **chunk-list complexity** by concatenating/appending groups of tables. Tiny chunks are deliberately pathological and need strict quotas.
- Increase **data bytes** using large deterministic blocks in a modest number of files/tables and production-like chunk sizes. Stream bytes instead of holding the target data set in client memory.
- Use deterministic rows such as `(run_id, object_index, row_index, payload_hash)` and deterministic pseudorandom payloads. Include compressible and incompressible content.
- Generate independent prefixes from several clients under one rate controller. Raise concurrency only while error rate and p99 latency remain below stop thresholds.

## Execution plan

### Phase 0: characterize and budget

Capture software/configuration versions, master IDs and roles, portal placement, quotas, media, bundles, node/chunk/Cypress counts, master RSS and detailed master memory, snapshot/changelog sizes, and baseline latency. Calculate approved peak counts and bytes for every profile. Reserve enough headroom for cleanup.

### Phase 1: build the graph

1. Create isolated principals, accounts, pools, bundles, and roots.
2. Create dependencies first: principals/accounts; topology/schemas; roots/portals; storage; replicas/queues; transactions/operations.
3. Populate deterministic content, then apply ACLs, attributes, links, copy/move, locks, mount states, and replica states.
4. Divide objects into **cold** (unchanged), **hot** (mutated during service), and **boundary** (open transaction, lock, mounted table, or running operation handled by the maintenance procedure) sets.
5. Fully verify the manifest and save baseline metrics and per-family counts.

### Phase 2: ramp and soak

Run profiles independently before the mixed profile. At each checkpoint, stop creation, wait for background convergence, build a non-read-only test snapshot, and measure its duration and size. Run the mixed soak across multiple snapshots, chunk refreshes, tablet compactions, and transaction timeouts. Continuously verify a random sample at low rate.

### Phase 3: add the master cell

Follow the official extension procedure exactly. Stop mutators before shutdown and record the final acknowledged manifest offset. That procedure requires a read-only snapshot and downtime; this is not an online-write test.

Monitor every primary and secondary peer separately. Record registration time, replicated-object rate, Hive queue size, memory/RSS, CPU, snapshot/changelog state, and alerts. Wait for object replication and cell-state convergence, not a fixed sleep.

Assign only intended roles. For `chunk_host`, verify newly created chunks are distributed to it; old metadata need not immediately rebalance. For `cypress_node_host`, create a test portal targeting it and run the portal subset there. For `transaction_coordinator`, start new transactions touching several native cells. Adding a cell does not automatically move existing objects.

### Phase 4: verify and resume

Before load resumes:

1. Compare family counts with baseline and verify every cold entry.
2. Validate IDs, native cell tags, types, attributes, ACLs, links, schemas, replicas, portal exits, transaction/lock outcomes, tablet mount states, row counts, and hashes.
3. Query through every proxy pool and a freshly created native client to detect stale master-cell directories.
4. Exercise create/read/update/delete and copy/move per family, inside and outside transactions.
5. Build another snapshot. In a separate planned test, restart one follower and perform a controlled leader switch per cell, verifying after each recovery.

Resume at functional rate, then repeat the ramp. Compare latency percentiles, error rate, master memory per object, snapshot time/size, recovery time, Hive convergence, and chunk distribution with baseline. Investigate monotonic queue or memory growth even when correctness passes.

### Phase 5: cleanup and evidence

Stop writers, verify again, and archive the manifest, generator/config versions, metrics, alerts, logs, snapshot IDs, and skipped cases. Remove registrations/replicas before tables; unmount/remove dynamic tables before cells/bundles; remove portals before roots; abort remaining transactions/operations; then remove principals/accounts. Verify that object/chunk counts and account usage return to baseline. Cleanup success is part of the test.

## Result report

Report topology and version before/after; roles, seed, generator version, and profile; requested/created/verified/failed/cleaned counts per family; logical/compressed bytes and chunk/tablet/store/Cypress/global-object counts; baseline/peak/final resource and latency metrics per peer; snapshot, restart, leader-switch, registration, replication, and convergence durations; failures with manifest keys and IDs; unexpected alerts; skipped features; threshold breaches; and the final go/no-go decision.

Retain the functional profile as a regression suite. Keep larger profiles parameterized rather than preserving one fixed data set because limits and production ratios evolve.
