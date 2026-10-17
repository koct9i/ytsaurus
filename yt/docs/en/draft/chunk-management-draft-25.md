---
type: Draft Article
title: "Draft-25: Chunk management architecture"
last_modified: 2026-09-04T00:00:00Z
tags: [chunks, master, data-node, storage]
status: draft
---

# Chunk management architecture

This article describes the internal lifecycle of a chunk: how a client creates or
finds it, what the master remembers, how Data Nodes store it, and how the system
restores the desired placement or eventually deletes it.

{% note warning "Draft" %}

This is an implementation-oriented draft. Names of queues, jobs, RPCs, and
configuration options can change. The durable invariants and the division of
responsibility are more important than any individual class name.

{% endnote %}

## The chunk model { #model }

A **chunk** is an immutable, block-addressable storage object. Tables, files,
journals, and hunk storage use chunks, but interpret the blocks and metadata
differently. Immutability is the key simplifying invariant: changing a table or
file creates and attaches new chunks instead of modifying confirmed blob chunks
in place. Journal chunks are the deliberate exception during their active life:
records may be appended until the chunk is sealed.

Three layers describe the same object:

* The **chunk client** sees a chunk ID, metadata, a codec, and replica descriptors.
  It creates read or write sessions and transfers blocks directly to Data Nodes.
* A **Data Node** owns physical replicas in disk locations. It serves blocks,
  persists chunk metadata, reports replica changes, and executes master jobs.
* The master **chunk server** owns the authoritative control-plane metadata
  object: type, confirmation/sealing state, requisition, chunk-tree links, and the
  known set of replicas. It retains only a compact subset of the chunk's format
  metadata; the complete read metadata lives with each physical replica. It does
  not carry user data in normal operation.

The master state is a replicated state-machine state. Chunk objects, chunk-tree
links, requisitions, and master-resident registered replicas are **persistent**
and change in Hydra mutations. Some deployments externalize a configured share
of replica records to Sequoia instead; the chunk server fetches and reconciles
those records rather than embedding them in each master chunk object. Refresh
queues, placement indexes, job queues, and running jobs are **transient**; after
leadership change or restart they are rebuilt from persistent metadata.
Correctness therefore cannot depend on a transient job running exactly once.
Every operation is reconciled again from observed state.

### IDs, chunks, parts, and replicas

A chunk ID identifies the logical chunk. A regular replicated chunk has several
complete physical **replicas**. An erasure chunk has individually numbered
**parts**; a replica descriptor therefore includes the chunk ID, replica/part
index, node or location, and storage medium. The master reasons about the logical
chunk while Data Nodes store either the whole chunk or one erasure part.

Two independent classifications are easy to confuse:

1. **Payload type** says how to interpret the contents: file, table, journal, or
   hunk.
2. **Redundancy/object form** says whether the object is a regular blob or
   journal chunk, an erasure chunk, or an individual erasure part.

Compression and table format (`optimize_for=scan` or `lookup`) are format choices,
not additional chunk types. Likewise, a chunk's **vital** flag changes alerting
and loss semantics, not its physical representation.

### Where chunk metadata lives { #metadata-placement }

`TChunkMeta` consists of three fixed fields—chunk **type**, **format**, and a
**features** bit mask—plus a set of typed protobuf extensions. Confirmation does
not copy that complete set into the master. Before `ConfirmChunk`, the writer
filters the extensions to the master allowlist. This distinction keeps detailed,
per-block metadata out of master memory while leaving enough summary information
for chunk-tree traversal, accounting, placement, and scheduling.

The **master stores persistently**:

* the chunk ID/object kind and confirmation or journal-sealing state;
* type, format, and feature bits;
* `TMiscExt`, including aggregate row count, compressed and uncompressed sizes,
  data weight, compression codec, largest data-block size, system-block count,
  timestamps and flags relevant to the chunk;
* `TBoundaryKeysExt` for pruning and validating sorted-table ranges;
* `THunkChunkRefsExt` and `THunkChunkMiscExt`, which are needed to account for and
  navigate hunk references;
* `THeavyColumnStatisticsExt`, when produced;
* the table schema as a separate master schema object/reference, rather than the
  replica's serialized `TTableSchemaExt`;
* control-plane state that is not part of `TChunkMeta`: disk-space charge,
  erasure codec, read/write quorum and lag settings, requisition and replication,
  parent chunk lists, exports, last-seen replica descriptors, placement state, and
  ally-replica endorsement state. Stored and approved replica records live here
  in the classic representation, or in Sequoia when that representation is
  enabled for the chunk.

Several frequently read `TMiscExt` values are also extracted into fields on the
master chunk object. This is an access and memory-layout optimization; the values
still originate in the metadata supplied at confirmation (or in later journal
seal information).

Each **Data Node replica stores the complete blob chunk metadata** beside the
chunk data (commonly in a metadata sidecar) and serves it via `GetChunkMeta`.
Depending on the chunk format, node-side-only extensions include:

* `TBlocksExt`, with physical block sizes, checksums, and offsets;
* erasure and striped-erasure placement information;
* table data-block metadata and indexes, name tables, column metadata and column
  groups, samples, partitions, key-column information, row digests, and other
  format-specific read structures;
* the serialized table-schema extension used by readers, detailed columnar
  statistics not included in the master allowlist, hunk metas, system-block
  metadata, compression-dictionary metadata, and future extensions that readers
  understand but the master does not need.

Thus the master can answer discovery and coarse range-planning requests, but it
cannot by itself decode table rows or locate and verify arbitrary blocks. A
reader gets the detailed metadata from a chosen Data Node (and may cache it) before
performing format-specific reads. For blob chunks, all healthy replicas are
expected to carry equivalent immutable metadata. Journal metadata is different:
while a journal is active, a Data Node derives its current `TMiscExt`—notably
flushed row count, byte size, and sealing state—from the local changelog, so
replicas can temporarily report different progress. The master keeps the agreed
logical summary and finalizes it during sealing.

{% note info "Terminology" %}

"The master stores chunk metadata" therefore means that it stores the compact
control-plane projection above. It does **not** mean that the master is a fallback
repository for the complete replica metadata file. Losing every replica also
loses the detailed format metadata required to read the chunk.

{% endnote %}

## Chunk types { #types }

The public payload types are:

| Type | Purpose | Mutability and access pattern | Notable metadata |
|---|---|---|---|
| **File** | Carries byte ranges of a Cypress file. | Written once, then read as an ordered byte stream. | Block sizes/checksums and file offsets; no table schema or keys. |
| **Table** | Carries rows of a static or dynamic table. | Immutable after confirmation. Readers can scan ranges, seek by row index, or use keys and indexes supported by the format. | Row count, data weight, boundary keys, schema, column statistics, block/index metadata, and timestamps for versioned chunks. |
| **Journal** | Carries an append-only sequence of records. | Open chunks accept appends. A seal fixes the logical row count; replicas can have different transient row counts before quorum/sealing reconciliation. | First row index, row count, quorum and replication settings, overlay/header information where applicable. |
| **Hunk** | Stores large values referenced from table rows so that small rows remain compact. | Append-oriented while produced and immutable when finalized; normally read through references from table chunks. | Hunk offsets, lengths, checksums, and linkage/statistics used by hunk storage. |
| **Unknown** | Sentinel used before metadata is known. | Not a valid confirmed user-data representation. | None. |

At the object-ID and replica layer there are also:

* **blob chunks**: the umbrella for non-journal chunks (file, table, and hunk);
* **journal chunks**: regular or erasure-coded journal objects with sealing rules;
* **erasure chunks**: logical chunks encoded into data and parity parts according
  to an erasure codec; each part is placed independently and can be repaired from
  a sufficient subset of other parts;
* **erasure chunk parts**: physical objects whose part index is encoded in the
  replica descriptor/object type. A part is not an independent Cypress payload.

Replication copies an entire regular chunk. Erasure coding instead spreads parts
across failure domains and trades CPU/network repair cost for lower storage
overhead. An erasure chunk's apparent replication factor is not the count of its
parts; availability and repair are derived from the codec's data/parity layout.

## Components and ownership { #components }

### Master chunk server { #master-chunk-server }

The chunk server is a subsystem inside each master cell, not a separate daemon.
The chunk-host cell stores chunk metadata and replica information; Cypress owners
may be native to another cell. Its main responsibilities are:

* create, confirm, seal, export/import, and destroy chunk objects;
* maintain chunk lists, chunk views, parent links, aggregate statistics, and
  reference counts;
* derive effective replication from all owners and accounts;
* process full and incremental Data Node heartbeats;
* allocate write targets and reconcile placement;
* schedule replication, repair, removal, and journal maintenance jobs;
* publish replica directories to clients and ally replicas to Data Nodes;
* account for lost, unavailable, under-replicated, over-replicated, misplaced,
  and unsafely placed chunks.

The **chunk manager** owns persistent objects and heartbeat mutations. The
**chunk replicator** is the background reconciler. Conceptually it combines:

1. a refresh scanner/queue that classifies each chunk;
2. chunk placement, which chooses suitable source and target nodes;
3. per-node transient queues and a job scheduler, which turn decisions into work.

Only a leader schedules work. Followers replay persistent replica changes but do
not independently issue jobs.

### Data Node chunk store { #data-node }

Each Data Node can have multiple **locations**, normally one per storage path or
disk. A location has a medium, UUID, state, capacity statistics, and a set of
replicas. The node:

* opens upload sessions and writes blocks to temporary files;
* validates checksums and finalizes chunk data and metadata atomically;
* serves block, meta, lookup, and range-read requests;
* scans locations at startup and reports their inventories;
* sends incremental additions/removals after the initial inventory;
* executes master jobs and reports their state in job heartbeats;
* keeps a cache of ally replica directories and can suggest fresher alternatives
  to clients;
* quarantines or reports damaged replicas and removes replicas only through the
  safe removal protocol.

The Data Node is authoritative about bytes actually present on its disks; the
master is authoritative about which logical chunk exists and what placement is
desired. Heartbeats make those views eventually consistent.

### Chunk clients { #clients }

The native chunk client library underlies proxies, operation jobs, tablet nodes,
and user-facing clients. Its writers:

1. ask the master to create a chunk and allocate initial targets;
2. establish a session with a target node (or a chain/set of targets);
3. encode/compress blocks, calculate checksums, and send blocks and metadata;
4. close the node sessions and confirm the chunk at the master;
5. attach the confirmed chunk to the upload chunk list and commit the upload.

Readers obtain chunk specs and replica descriptors from the master or a cache,
choose replicas using locality, medium, network, throttling, and health signals,
fetch metadata/blocks from Data Nodes, validate checksums, and retry other
replicas. Erasure readers can fetch parts and reconstruct missing data. Replica
directories carry revisions so that stale information can be detected and
replaced by an ally-replica suggestion.

## Chunk trees and chunk lists { #chunk-lists }

A chunk owner does not contain a flat vector of IDs. It points to a **chunk tree**
whose leaves are chunks or chunk views and whose internal nodes are **chunk
lists**. A chunk view selects a key/range slice of an underlying table chunk; it
can share physical data without rewriting it.

Chunk lists aggregate subtree statistics such as row count, data weight,
compressed/uncompressed size, chunk count, and boundary information. Traversal
can discard a whole subtree when its row, key, or tablet range cannot intersect a
request. Static trees are rebalanced in the background to keep traversal and
append costs bounded; rebalancing is a persistent metadata change and does not
move chunk bytes.

### Chunk-list content and kinds

`content_type` separates a table's **main** row chunks from its **hunk** chunks.
`kind` describes structural invariants:

| Chunk-list kind | Shape and purpose |
|---|---|
| `static` | General balanced tree for static tables/files and upload fragments. DFS leaf order is logical data order. |
| `sorted_dynamic_root` | Root whose children correspond to sorted-table tablets. |
| `sorted_dynamic_tablet` | Main chunk set for one sorted tablet; compaction may replace arbitrary subsets. |
| `sorted_dynamic_subtablet` | Temporary extra hierarchy used for sorted dynamic-table bulk insert. |
| `ordered_dynamic_root` | Root whose child index equals the ordered-table tablet index. |
| `ordered_dynamic_tablet` | Queue-like ordered chunk sequence; append at the back and trimming/removal at the front. |
| `journal_root` | Ordered list of journal chunks. |
| `hunk_root` / `hunk` | Hunk-side structures associated with tables/tablets. |
| `hunk_storage_root` / `hunk_tablet` | Root and per-tablet organization for a hunk-storage object. |
| `scratch` | Unstructured holder that accepts arbitrary chunks and deliberately maintains no aggregate statistics. |

Parent links and reference counts matter. Sharing a chunk between owners combines
their storage requirements, and a chunk becomes collectible only after no live
chunk tree references it. In normal upload flows a new chunk is attached to a
transactional upload list immediately; it should not live as an unowned object.

### Transactional upload

`begin_upload` branches the chunk owner and creates an upload chunk list inside
the upload transaction. Newly confirmed chunks are attached there. `end_upload`
merges or replaces the destination root according to append/overwrite mode and
commits the branch. If the transaction aborts, its temporary list and references
are released; unreferenced chunks then enter normal destruction. This isolates a
partially completed upload from readers without requiring mutable chunks.

## Desired replication: requisitions { #requisitions }

An owner specifies replication factors by medium, erasure codec, and vitality.
Because the same chunk or view can be referenced by multiple owners and accounts,
the master preserves an annotated **requisition**: requirements together with
the accounts that requested them. The effective **replication** is the merged,
per-medium placement requirement derived from that requisition.

Requisition updates propagate from changed chunk-tree owners to leaves in a
background traversal and are committed by mutation. Until propagation finishes,
the old effective policy may remain visible. The requisition registry interns
identical values to reduce master memory. Resource accounting charges the
relevant accounts even when physical data is shared.

## Replica discovery and heartbeats { #heartbeats }

On registration, a Data Node sends a **full heartbeat** inventory (in current
deployments this can be split by location and into batches). The master compares
the report with its stored replica set, registers discovered replicas, removes
stale entries, and marks each reported location online. This is a heavy persistent
mutation path, so batching prevents one large node from monopolizing the
automaton thread.

Afterward, **incremental heartbeats** report deltas: replicas added, removed, or
damaged, completed announcements, and location statistics. Any material change
schedules affected chunks for refresh. The master may initially mark a newly
reported replica **unapproved** until it can associate it with the intended
session/job; this prevents an unexpected copy from immediately satisfying the
desired replication count.

**Job heartbeats** are a separate control loop. A node reports running and
finished master jobs and available resources. The master replies with new jobs.
If a job fails, times out, or disappears with a disposed node, its chunks are
refreshed and eligible for another attempt. Jobs and their queues are transient;
the later inventory delta is what persistently proves that a replica was created
or removed.

## Allocation and placement { #allocation }

Allocation is used both for a new write and for repair/re-replication. The client
or replicator supplies a medium and a desired count (or erasure part indexes),
plus forbidden/existing targets. Placement filters nodes and locations that are
offline, read-only, full, disabled for writes, on the wrong medium, or otherwise
ineligible. It then balances among candidates while observing topology rules.

Important constraints include:

* do not place two replicas/parts of the same logical chunk on one node;
* spread replicas across racks and, when configured, data centers;
* honor medium and node-tag policies;
* avoid locations without sufficient space and nodes over write/session limits;
* balance stored bytes, active writes, and consistent-placement load;
* preserve enough eligible sources and destinations for the codec.

Placement is best-effort under degraded topology. Failure to allocate does not
change persistent state; the chunk remains in an attention queue and is retried
as nodes, space, or configuration change. For initial writes, allocation returns
targets to the client. For replication, targets are selected when a source node's
job heartbeat has capacity, reducing reservations that could go stale.

## Reconciliation and master jobs { #reconciliation }

Refresh compares current approved replicas with effective requirements and puts
work into priority queues. Typical states include healthy, under-replicated,
over-replicated, lost, data-missing or parity-missing, misplaced, and
inconsistently placed. Vital lost chunks receive operational attention; non-vital
intermediate chunks can normally be recomputed by their owner.

### Replication jobs { #replication-jobs }

For a regular chunk, a **replication job** reads a complete replica from a source
node and writes it to one or more allocated targets. In the usual push model the
source executes the job. Successful transfer alone does not update the master's
durable set: the destination reports the addition, the master registers/approves
it, and refresh observes that the deficit is gone.

Under-replication is prioritized by severity. The scheduler throttles bytes,
jobs, and per-node work so that a mass node outage does not turn into an
uncontrolled replication storm.

### Erasure repair jobs

A **repair job** reads enough available data/parity parts to reconstruct missing
parts, computes them, and writes each result to a separately allocated target.
The reconciler distinguishes unavailable data parts from missing parity because
their effect on reads and urgency differs. Repair is impossible after losses
exceed the codec's tolerance; such a chunk is lost even if some parts survive.

### Journal jobs

Journals require additional reconciliation. **Seal jobs** establish the final
row count and close an active chunk. Other journal maintenance can bring lagging
replicas to the agreed row count or repair erasure journal parts. Quorum rules
govern successful appends; a replica containing fewer records is not treated like
a valid complete blob replica merely because its file exists.

### Removal jobs and two-phase destruction { #removal }

Removal happens for two different reasons:

1. **excess/misplaced replicas**: the logical chunk remains alive, but refresh
   selects a safe copy to delete after verifying that enough appropriately placed
   replicas or parts remain;
2. **chunk destruction**: the logical chunk has no live owner/reference, so every
   physical replica must eventually disappear before metadata can be forgotten.

The master issues a removal job to the node holding the replica. To avoid a race
where lost master knowledge leaves undeletable garbage—or stale heartbeats
resurrect a deleted chunk—the protocol tracks **destroyed replicas**. The master
remembers that a particular location must not contain the replica and repeatedly
requests removal. The node confirms absence through a heartbeat; only then is the
destroyed-replica entry dropped. Unknown replicas of dead/nonexistent chunks are
also scheduled for removal. This makes deletion idempotent across retries,
node/master restarts, and delayed heartbeats.

Logical object removal, chunk-tree reference release, master chunk destruction,
physical removal jobs, and disk-space reclamation are therefore separate steps.
Large table deletion can legitimately produce a temporary removal backlog.

## Ally replicas and endorsements { #ally-replicas }

A client normally receives replicas from the master, but that list can become
stale between retries. **Ally replicas** let each Data Node know where the other
replicas of its local chunks live. When serving a read, a node can attach a newer
replica directory and revision to the response. The client can update its cache
without another master round trip, reducing master load and improving retry
latency.

The master does not send the directory separately to every replica. It places a
replica-announcement request in a full or incremental heartbeat response to one
Data Node. That seed node stores the directory in its ally-replica manager and
fans the revisioned announcement out directly to the other nodes named in the
directory. A recipient retains the announcement only if it actually stores the
chunk (or the relevant erasure part), and ignores a revision older than the one it
already knows. Announcements may be:

* immediate for stable, exactly replicated chunks;
* delayed for under-replicated chunks, allowing placement to settle;
* lazy while the cluster is unstable, avoiding an announcement storm.

This cache is eventually consistent. Correct readers must still tolerate a dead
suggested replica and retry or refresh from the master.

An **endorsement** closes the master-to-seed delivery loop. When the replica set
changes, the master marks the chunk as requiring an endorsed announcement and
assigns one surviving location (deterministically, currently the greatest
suitable location ID) to carry it. The announcement includes
`confirmation_needed` and a master revision. On accepting that request, the seed
node queues the chunk ID and revision for confirmation in a following heartbeat;
the master removes the endorsement only after the matching or newer revision is
acknowledged. If the chosen location disappears or the replica set changes again,
the endorsement is discarded or reassigned and resent.

An endorsement proves that a designated seed accepted the current directory; it
does not synchronously prove that every peer received the fan-out. Peer delivery
remains eventually consistent and revision guarded. This is sufficient because
ally information is only a read-retry optimization: the master replica directory
and ordinary reader fallback remain authoritative.

Chunks with one non-erasure replica need no ally directory. Endorsements apply to
blob chunks; journal availability follows its own quorum and sealing machinery.

## End-to-end lifecycles { #lifecycles }

### New immutable chunk

1. The owner starts an upload transaction and receives a fresh upload chunk list.
2. The client creates a chunk; the master records an unconfirmed object and
   allocates targets for the requested medium/codec.
3. The client writes blocks to Data Nodes and closes the sessions.
4. The client confirms chunk metadata at the master. The destination replicas are
   also reported by incremental heartbeat and approved.
5. The confirmed chunk is attached to the transactional chunk list.
6. `end_upload` commits the owner branch. Requisition propagation derives the
   final per-medium requirements.
7. Refresh corrects any placement deficit and announces allies.

Failure before commit releases the temporary tree. Partial node sessions and
unowned replicas are garbage-collected through the same destruction/removal loop.

### Lost node and re-replication

1. The node stops heartbeating; its liveness lease expires and the master disposes
   the node/locations in a mutation.
2. Registered replicas on those locations are removed from persistent sets and
   affected chunks enter the transient refresh queue.
3. Refresh detects a deficit and places each chunk in source-node replication or
   repair queues.
4. A source's job heartbeat obtains work; placement chooses destinations.
5. The job copies or reconstructs data. Destination incremental heartbeats prove
   the new replicas exist.
6. Refresh marks the chunk healthy and the master issues revised ally
   announcements. Endorsement confirmation completes their propagation.

A returning node performs a full location inventory. Copies that are still useful
may be registered; obsolete or explicitly destroyed copies are removed.

### Policy or medium change

Changing replication factor, medium, codec, or ownership first changes owner
metadata/requisitions. Requisition propagation updates affected chunks. Refresh
creates required replicas on the new medium before deleting old/excess copies,
subject to safety checks. Changing a codec normally requires rewriting chunks;
placement cannot transform a regular chunk into erasure parts by metadata alone.

### Removing data

1. A transaction detaches the owner's root or selected chunk-tree leaves.
2. Reference counts and requisitions are recalculated; shared chunks remain.
3. An unreferenced chunk is marked dead and all known replicas become destroyed
   replica records.
4. Removal jobs and heartbeat retries delete physical files.
5. Confirmation of absence drains destroyed-replica records; metadata and account
   usage can then be fully reclaimed.

## Failure handling and operational invariants { #failure-handling }

The design is intentionally asynchronous. At any instant:

* a node may have a replica that the master has not learned about yet;
* the master may list a replica whose disk or node has just failed;
* an ally directory may lag behind the master;
* a completed job may wait for its destination heartbeat;
* a deleted table may still occupy disk while removals drain.

These are expected intermediate states. Safety comes from the following rules:

* persistent replica changes occur only in mutations;
* physical existence is learned from inventories/deltas, never inferred solely
  from successful RPC completion;
* transient work is reconstructible and idempotent;
* create-before-remove ordering protects the desired redundancy;
* topology diversity is part of health, not just replica count;
* destroyed-replica tombstones prevent stale reports from reviving garbage;
* revisioned endorsements make ally dissemination retryable.

Operators should watch refresh-queue progress, oldest queued chunk age,
under-replicated and lost-vital counts, missing erasure parts, replication/repair
and removal job throughput, destroyed-replica backlog, allocation failures,
heartbeat lag, and endorsement backlog. A queue spike after an outage or a large
deletion is normal if its age and size decrease. A flat or growing oldest age is
more dangerous: until refresh runs, even loss classification can be stale.

For one problematic chunk, inspect its `stored_replicas`, `last_seen_replicas`,
`replication_status`, requisition/replication attributes, parents, and the state of
the corresponding nodes and locations. On a Data Node, inspect the chunk and ally
replica manager Orchid views and correlate chunk ID, location UUID, job ID, and
heartbeat logs. Allocation failures should be checked against medium capacity,
write eligibility, node/rack/data-center diversity, node tags, and throttling—not
only total free bytes.

## Summary { #summary }

Chunk management is a convergent control loop across three parties. Clients move
immutable blocks and tolerate stale replica information; Data Nodes own physical
truth and execute idempotent work; the master chunk server owns logical truth,
chunk trees, policy, and reconciliation. Chunk lists make large logical objects
traversable and transactional, requisitions turn shared ownership into placement
requirements, replication and repair jobs restore redundancy, tombstoned removal
makes garbage collection safe, and endorsed ally announcements keep the read path
fast without making caches authoritative.
