<!--
Draft number: 22
Author: AI agent (OpenAI)
Created: 2026-08-11
Status: In progress
Target: admin-guide/components-compatibility.md
-->

# Component compatibility and persistent-state versions

{% note warning "Draft" %}

This article describes compatibility mechanisms visible in the current server
and migration code. Check the deployed release's upgrade instructions before
changing persistent state.

{% endnote %}

YTsaurus does not have one cluster-wide "state version". Different components
persist different kinds of state and use independent compatibility mechanisms.
The same word *version* can mean a Hydra serialization reign, a snapshot format,
a migrated system-table schema, a tolerant YSON structure, or merely a binary
build version.

## Compatibility domains at a glance

| Owner | Persistent data | Compatibility mechanism | Current/deployed value |
|-------|-----------------|-------------------------|------------------------|
| Master cell | Hydra snapshots and mutations | Master reign | `//sys/primary_masters/<address>/orchid/reign`; secondary peers use `//sys/secondary_masters/<cell-tag>/<address>/orchid/reign` |
| Tablet cell | Hydra snapshots and mutations | Tablet reign | `//sys/tablet_nodes/<address>/orchid/tablet_cells/<cell-id>/reign` |
| Chaos cell | Hydra snapshots and mutations | Chaos reign | Stored in snapshot and mutation metadata; there is no dedicated current-reign child in the chaos-cell Orchid |
| Cluster clock | Hydra snapshots and mutations | Clock reign | Used internally during clock-cell recovery; the clock Orchid does not expose a standalone current-reign field |
| Controller agent | Per-operation revival snapshots | Controller-agent snapshot version | `//sys/controller_agents/instances/<address>/orchid/controller_agent/snapshot_version` |
| Scheduler | Operations in Cypress and tolerant YSON strategy state | No scheduler snapshot version | No equivalent counter; inspect the controller-agent snapshot version and operations-archive version for the persisted formats the scheduler consumes |
| Operations archive | Dynamic tables under `//sys/operations_archive` | Migration/schema version | `//sys/operations_archive/@version` |
| Query tracker | Dynamic tables under `//sys/query_tracker` | State migration/schema version | `//sys/query_tracker/@version` |
| Queue agent | Dynamic tables under `//sys/queue_agents` | State migration/schema version | `//sys/queue_agents/@version` |
| Sequoia ground | Tables and state in the ground cluster | Ground migration reign | `<sequoia-root>/@ground_reign`; the expected value is `//sys/primary_masters/<address>/orchid/ground_reign` |
| Sequoia requests | Requests between Sequoia-aware components | Sequoia protocol reign | Compiled into components and carried in relevant requests; not a snapshot or table-schema version |

Numbers from different rows are unrelated. For example, controller-agent
snapshot version 70 is neither newer nor older than operations-archive version
60. Compare a value only with the supported or expected value in the same row.

## Hydra automaton reigns

A Hydra reign versions the serialized automaton state and mutations of one kind
of Hydra cell. Recovery code uses it to select compatibility logic and to decide
whether an old snapshot or changelog can be loaded. Master, tablet, chaos, and
clock automata each define their own reign sequence.

Reusable automaton subsystems also have internal reigns. The transaction
supervisor and lease manager are examples. They are embedded in an owning
automaton rather than deployed as independently upgraded services, so there is
normally no separate operator-facing value to compare.

Inspect exposed reigns as follows:

```bash
# Master binary's compiled value (also available in YSON form with --yson).
ytserver-master --compatibility-info

# Values of running master and tablet-cell peers.
yt get //sys/primary_masters/<address>/orchid/reign
yt get //sys/tablet_nodes/<address>/orchid/tablet_cells/<cell-id>/reign
```

An individual tablet's `#<tablet-id>/orchid/reign` is the reign stored for that
tablet. Use the tablet-cell slot path above when determining which reign the
hosting node is currently running.

## Scheduler and controller-agent state

The scheduler and controller agent have different persistence responsibilities.
They must not be described as sharing a scheduler snapshot version.

### Controller-agent operation snapshots

The controller agent serializes per-operation state so an operation can be
revived after an agent restart or reassignment. `ESnapshotVersion` gates fields
in that serialization. The static controller-agent Orchid publishes the version
used for newly written snapshots:

```bash
yt get //sys/controller_agents/instances/<address>/orchid/controller_agent/snapshot_version
```

A stored snapshot also carries its own version. Loading validates that stored
version against the range supported by the running controller-agent binary.
Thus the Orchid value describes the writer, while successful revival depends on
the version embedded in the particular operation snapshot.

### Scheduler persistent state

The scheduler itself has no `ESnapshotVersion` and does not write an equivalent
serialized scheduler snapshot. Operation metadata and revival descriptors live
under `//sys/operations`; the controller agent owns the versioned operation
snapshot. The scheduler also stores fair-share strategy state as YSON at
`//sys/scheduler/strategy_state`. That structure uses defaulted YSON fields
rather than a numeric format version; if it cannot be deserialized, the
scheduler logs a warning and drops the saved strategy state.

The scheduler does consume another independent compatibility value: the
operations-archive schema version described below.

## Versioned system dynamic-table schemas

Some components persist state in dynamic tables instead of private snapshots.
The migration framework stores a numeric `@version` on the root map node. A
migration may alter tables, create or remove tables, transform rows, or perform
an action, and advances the root version only as part of that migration.

Do not infer compatibility only from an individual table's `@schema`: the root
`@version` identifies the migration set applied across all tables owned by the
component.

### Operations archive

The archive contains completed-operation and job information used by the
scheduler, controller agents, exec nodes, job proxies, and native clients. Its
deployed schema version is:

```bash
yt get //sys/operations_archive/@version
```

The scheduler periodically reads this value, publishes an `archive_is_outdated`
alert when it is below `min_required_archive_version`, and distributes it to
controller agents and nodes. Writers use the value to omit fields that older
archive schemas cannot represent; readers likewise gate fields and queries by
the archive version.

Upgrade the archive with the operations-archive migration tool, not by editing
`@version` or table schemas manually. The latest version known to the migration
tool and the minimum required by a server binary are related but different:
the former is a migration target, while the latter is a compatibility floor.

### Query tracker state

Query tracker state is a set of tables rooted at `//sys/query_tracker`. Inspect
its migration version with:

```bash
yt get //sys/query_tracker/@version
yt get //sys/query_tracker/instances/<address>/orchid/config/min_required_state_version
```

The query tracker performs a health check against the configured minimum and
publishes an invalid-state alert if the deployed version is too old. Use the
query-tracker state migration tool to change the schema.

### Queue-agent state

Queue agents use migration-managed tables rooted at `//sys/queue_agents`,
including queue and consumer metadata, registrations, and replicated-table
mappings. Inspect the migration version with:

```bash
yt get //sys/queue_agents/@version
```

Use the queue-agent state migration tool when upgrading them. Do not confuse
the root's schema version with row revisions maintained by the synchronizer or
with the binary `@version` attribute of a queue-agent instance.

### Other component-owned dynamic tables

Not every system dynamic table has a migration version. Examples include the
scheduler event log and tablet-balancer performance counters. Such tables are
created or configured from explicit schemas and generally evolve through
optional columns and tolerant readers. The absence of a root `@version` means
there is no supported numeric migration counter to compare; it does not mean
that arbitrary schema changes are safe.

Before treating another `//sys` subtree as versioned, verify all three facts:

1. The component has a migration definition for the whole subtree.
2. The migration tool owns the root `@version` attribute.
3. The running component validates or branches on that value.

If these do not hold, inspect the component's configured schema and release
upgrade instructions instead of inventing or manually advancing a version.

## Sequoia compatibility values

Sequoia has two distinct reigns:

- **Sequoia reign** is a protocol compatibility value carried in relevant
  requests between Sequoia-aware components.
- **Ground reign** is the migration level of Sequoia state in the ground
  cluster.

Compare the deployed and expected ground reigns with:

```bash
# Run against the ground cluster.
yt get <sequoia-root>/@ground_reign

# Run against the cluster whose master uses that ground state.
yt get //sys/primary_masters/<address>/orchid/ground_reign
```

The Sequoia migration tool advances ground reign after applying the associated
migration plan. Do not set the attribute directly as a substitute for running
the migration.

## Upgrade checklist

1. Identify every persistence owner affected by the binaries being upgraded.
2. Record the deployed value and the new binary's required or supported value
   in the same compatibility domain.
3. Run supported table or ground-state migration tooling before a component
   requires the new schema. Never advance only the marker attribute.
4. During rolling upgrades, compare master peers with master peers and tablet
   cells with tablet cells. Check tablet reigns when smooth movement fails near
   a node restart.
5. Confirm scheduler, query-tracker, and component alerts are clear after the
   migration and restart.
6. Test controller-agent operation revival; a healthy agent-level Orchid value
   alone does not prove that every stored operation snapshot can be loaded.
