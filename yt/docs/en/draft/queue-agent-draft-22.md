<!--
Draft number: 22
Author: AI agent
Created: 2026-08-03
Status: In progress
Target: admin-guide/queue-agent
-->

# Queue Agent: purpose, features, usage, and administration

Queue Agent is the {{product-name}} service that observes queues and consumers, calculates their state, and performs background queue maintenance. This article explains which tasks belong to Queue Agent, how users interact with it, and how cluster administrators deploy and troubleshoot it.

{% note info "Prerequisites" %}

This article assumes familiarity with [dynamic tables](../user-guide/dynamic-tables/overview.md) and the [queue data model and API](../user-guide/dynamic-tables/queues.md). A queue is an ordered dynamic table; a consumer is a sorted dynamic table that stores the first unread row index for each queue partition.

{% endnote %}

## Purpose

The regular queue API reads, writes, and advances consumer offsets without Queue Agent being on the request path. Queue Agent instead maintains the *control plane* around these operations. It:

- discovers queues and consumers and keeps their metadata in system state tables;
- assigns each tracked object to one active Queue Agent instance;
- periodically builds queue and consumer snapshots, including partition backlog, data weight, rates, and lag;
- publishes status attributes used by the web interface and operators;
- trims queue partitions according to vital-consumer offsets and retention settings;
- exports queue chunks to static tables when static export is configured.

Consequently, an unavailable Queue Agent does not normally stop producers from writing or consumers from reading and committing offsets. Monitoring becomes stale, however, and automatic trimming and static exports stop until an instance takes responsibility for the object again. Applications must not use Queue Agent status as a correctness mechanism.

## Architecture

A Queue Agent deployment consists of one or more identical service instances and a set of dynamic state tables, normally under `//sys/queue_agents`.

### Components

Each instance runs the following logical components:

| Component | Responsibility |
| --- | --- |
| Cypress synchronizer | Discovers queue and consumer objects and copies the attributes required by Queue Agent into state tables. It can poll Cypress or watch configured clusters. |
| Sharding manager | Distributes objects of the instance's stage among non-banned instances and records the selected host in the object-mapping table. |
| Queue Agent controllers | Poll queue and consumer state, calculate status and profiling data, trim queues, and start exports. |
| Export manager | Limits and coordinates static export tasks. |

Only one instance leads a given object at a time. Sharding provides load distribution and failover; it does not partition an individual queue between multiple Queue Agent instances.

### State tables

The initialization and migration utility creates the following principal tables under the configured root:

| Table | Contents |
| --- | --- |
| `queues` | Queue identity, type, revision, trimming and export configuration, stage, ban flag, and synchronization error. |
| `consumers` | Consumer identity, schema, stage, ban flag, and synchronization error. |
| `consumer_registrations` | Queue-to-consumer registrations, including whether a consumer is vital and an optional partition filter. |
| `queue_agent_object_mapping` | Object-to-instance assignment maintained by the sharding manager. |
| `replicated_table_mapping` | Metadata connecting replicated or chaos-replicated objects to their replicas. |

These tables are internal service state. Do not edit their rows manually. Use queue API methods and Cypress attributes instead, and use the supplied migration utility when schemas must change.

### Stages

The static `queue_agent/stage` setting identifies a family of Queue Agents. A queue or consumer is handled only by the family whose stage matches its `@queue_agent_stage` attribute. Stages make it possible to run a new deployment alongside an existing one or to isolate groups of objects.

The queue and each of its consumers should be visible to an appropriate Queue Agent stage. Before moving production objects, deploy healthy instances for the target stage and verify that the synchronizer is populating state. A stage mismatch leaves an object without fresh status or leading actions.

## Features

### Status and monitoring

Queue Agent exposes computed state through Cypress attributes:

- queues: `@queue_status` and `@queue_partitions`;
- consumers: `@queue_consumer_status` and `@queue_consumer_partitions`.

For example:

```bash
yt get //home/example/queue/@queue_status
yt get //home/example/queue/@queue_partitions
yt get //home/example/consumer/@queue_consumer_status
yt get //home/example/consumer/@queue_consumer_partitions
```

Queue-level status includes write rates and aggregate health. Partition status includes row boundaries, available row count and data weight, and write idleness. Consumer status describes each registered queue, while per-partition state includes the committed offset, unread row count, processing lag, and a disposition such as `up_to_date`, `pending_consumption`, `expired`, or `ahead`.

The values are periodic snapshots rather than transactional reads. Small transient inconsistencies are possible when a queue and consumer are sampled at different moments. The attributes are intended for diagnostics and user interfaces, not for high-rate polling. Use profiler metrics for continuous alerting and the queue API for application logic.

### Automatic trimming

Automatic trimming reclaims rows already processed by all relevant vital consumers. Configure it on the queue:

```bash
yt set //home/example/queue/@auto_trim_config '{
    enable = %true;
    retained_rows = 10000;
    retained_lifetime_duration = 3600000;
}'
```

The options have the following meaning:

- `enable` enables Queue Agent trimming;
- `retained_rows` keeps at least this many rows in every partition;
- `retained_lifetime_duration` keeps rows younger than the specified duration in milliseconds; the value must be a multiple of one second.

Trimming occurs only when there is at least one vital registration applicable to the partition. Queue Agent chooses a safe boundary based on the smallest vital-consumer offset, retention constraints, and completed exports. A slow vital consumer therefore holds back trimming. Non-vital consumers do not protect data and can become `expired` if they fall behind the trimmed lower row index.

The controller setting `enable_automatic_trimming` is a cluster-wide kill switch. Both it and the queue's `auto_trim_config/enable` must permit trimming.

### Static exports

Static export periodically exposes flushed queue chunks through static tables without copying their payload. A basic configuration is:

```bash
yt set //home/example/queue/@static_export_config '{
    hourly = {
        export_directory = "//home/example/queue_exports";
        export_period = 3600000;
        export_ttl = 604800000;
    };
}'
```

Authorize the destination for this queue before enabling export:

```bash
QUEUE_ID=$(yt get //home/example/queue/@id --format '<format=text>yson')
yt set //home/example/queue_exports/@queue_static_export_destination \
    "{originating_queue_id=\"${QUEUE_ID}\"}"
```

Export frequency cannot usefully exceed the queue's dynamic-store flush frequency. Export progress is stored in `@queue_static_export_progress` on the destination and is internal state. Do not modify or build applications around it. For replicated and chaos-replicated queues, configure export on the queue replica rather than on the replicated table.

## Using Queue Agent

### Prepare queue objects

Create and mount an ordered dynamic table. Including `$timestamp` and `$cumulative_data_weight` system columns improves lag and data-weight reporting:

```bash
yt create table //home/example/queue --attributes '{
    dynamic = %true;
    schema = [
        {name = "data"; type = "string"};
        {name = "$timestamp"; type = "uint64"};
        {name = "$cumulative_data_weight"; type = "int64"};
    ];
}'
yt mount-table //home/example/queue --sync

yt create queue_consumer //home/example/consumer
yt mount-table //home/example/consumer --sync
```

Register the consumer and deliberately choose its trimming semantics:

```bash
yt register-queue-consumer //home/example/queue //home/example/consumer --vital
```

Use a vital registration only when the consumer must protect unread data. For best-effort or independently recoverable consumers, register without `--vital` so that an outage cannot indefinitely retain queue data.

### Verify discovery and assignment

After the next synchronizer and controller passes, verify that Queue Agent recognizes the objects:

```bash
yt get //home/example/queue/@queue_status
yt get //home/example/consumer/@queue_consumer_status
```

Administrators can also find the responsible instance in the mapping table:

```bash
yt lookup-rows //sys/queue_agents/queue_agent_object_mapping \
    --format '<format=pretty>yson' <<'EOF'
{object="example-cluster://home/example/queue"};
EOF
```

The exact object key contains the cluster name known to Queue Agent. Prefer the queue status attributes for routine user checks; mapping-table inspection is an administration tool.

### Pause leading actions for one object

Set the built-in ban attribute while investigating incorrect configuration or unwanted trimming/export activity:

```bash
yt set //home/example/queue/@queue_agent_banned %true
# Investigate or change configuration.
yt set //home/example/queue/@queue_agent_banned %false
```

Banning an object prevents its controller from performing leading actions. It does not reject queue API requests, unmount the table, or pause producers and consumers. Avoid leaving a queue banned: monitoring remains in an error state and maintenance does not progress.

## Administration

### Initialize or migrate state

Create a dedicated tablet cell bundle for Queue Agent state when possible. The migration utility prefers `yt-queue-agent`, then `sys`, then `default`. Run it with a client that has permission to create and alter objects under the state root:

```bash
python3 yt/python/yt/environment/init_queue_agent_state.py \
    --proxy <cluster-proxy> \
    --root //sys/queue_agents \
    --latest
```

The command is also used during upgrades to migrate the existing tables to the latest schema. Back up state and follow the release-specific upgrade procedure before a production migration. Do not use `--force` as a routine remedy: it permits migration actions that would otherwise stop for safety.

### Configure and start instances

At minimum, each instance needs:

- a native cluster connection and service user;
- RPC and monitoring endpoints;
- paths to the same Queue Agent dynamic state tables;
- a shared dynamic configuration path;
- the intended `queue_agent/stage`;
- discovery, election, and Cypress registration configuration.

Run at least two instances for failover. Give the service user read access to tracked queue and consumer metadata, write access to Queue Agent state, and the permissions needed for trimming and export. Export can use a separate `queue_agent/queue_export_manager/user` to isolate master request limits.

Dynamic configuration controls pass periods, controller thread count, automatic trimming, export concurrency and rate, Cypress synchronization policy, sharding cadence, and whether replicated objects are handled. Change one setting at a time and observe pass errors and latency; very short pass periods can overload masters, tablet nodes, and Queue Agent itself.

### Check instance health

Instances register under `//sys/queue_agents/instances` by default:

```bash
yt list //sys/queue_agents/instances
```

For each instance, inspect the component Orchids:

```bash
INSTANCE='<host:rpc-port>'
ROOT="//sys/queue_agents/instances/${INSTANCE}/orchid"

yt get "${ROOT}/queue_agent/pass_error"
yt get "${ROOT}/queue_agent/controller_info"
yt get "${ROOT}/cypress_synchronizer/pass_error"
yt get "${ROOT}/queue_agent_sharding_manager/pass_error"
```

An empty `pass_error` and regularly changing pass indexes indicate progress. `controller_info` summarizes active and inactive objects. Individual queue and consumer controller Orchids provide detailed snapshots and errors; requests may be redirected to the instance currently responsible for the object.

Correlate Orchid results with Queue Agent logs and profiler metrics. In particular, alert on persistent component pass errors, an increasing age of the last successful pass, repeated controller errors, export backlog, and a state-table tablet becoming unavailable.

### Remove an unhealthy instance from sharding

Ban an instance to prevent the sharding manager from assigning it objects:

```bash
yt set //sys/queue_agents/instances/<instance>/@banned %true
```

After healthy instances take over, restart or repair the failed instance. Unban it only after its component pass errors have cleared:

```bash
yt set //sys/queue_agents/instances/<instance>/@banned %false
```

An instance ban and an object `@queue_agent_banned` attribute solve different problems: the former drains a service instance, while the latter disables Queue Agent actions for one queue or consumer.

## Troubleshooting

### Status is absent or stale

1. Confirm that the queue is dynamic and ordered, or that the consumer has a valid consumer schema.
2. Compare the object's `@queue_agent_stage` with the deployed instance stage.
3. Check the synchronizer `pass_error`, then inspect the object's `synchronization_error` in the `queues` or `consumers` state table.
4. Check that the object has an entry in `queue_agent_object_mapping` and that the assigned instance is present and not banned.
5. Inspect `queue_agent/pass_error`, `controller_info`, and the assigned controller's Orchid.

### A queue is not trimming

1. Confirm `@auto_trim_config/enable` and the dynamic `enable_automatic_trimming` switch.
2. List registrations and verify that at least one applicable registration is vital.
3. Compare vital-consumer offsets; the slowest vital consumer determines the upper safe boundary.
4. Account for `retained_rows`, `retained_lifetime_duration`, and incomplete static exports.
5. Check whether the queue or its assigned Queue Agent instance is banned and inspect controller errors.

Do not manually advance a consumer merely to make trimming progress unless loss of its unread data is explicitly acceptable.

### Exports do not appear

1. Validate `@static_export_config`, including a period divisible by one second and a unique output-name pattern.
2. Check that `@queue_static_export_destination/originating_queue_id` matches the queue ID.
3. Verify write permissions for the Queue Agent or configured export user.
4. Ensure queue stores have flushed; export operates on flushed chunks.
5. Inspect the queue controller Orchid and logs for retry errors. Failed tasks are retried, so fix the cause rather than editing export progress.

### State-table or migration problems

Treat unavailable state tablets like other critical dynamic tables: restore tablet-cell health first. Before rerunning a migration, compare the root's version with the version supported by the running binary and review migration logs. Never recreate individual state tables by hand, because their schemas and secondary indexes evolve together.

## Operational checklist

- Run multiple instances for every production stage.
- Place Queue Agent state in a monitored system tablet cell bundle.
- Back up state and migrate it as part of the server upgrade procedure.
- Monitor component pass errors, controller pass age, and state-table health.
- Make vital registrations intentional and alert on their lag.
- Set retention limits so a stalled consumer cannot cause unbounded storage growth.
- Match export periods to flush behavior and monitor export retries.
- Use instance bans for draining and object bans only for short investigations.
- Keep application correctness independent of Queue Agent's asynchronous status attributes.
