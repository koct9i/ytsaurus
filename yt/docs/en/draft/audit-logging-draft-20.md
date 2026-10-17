---
type: Draft Article
title: "Draft-20: YTsaurus audit logging"
last_modified: 2026-08-21T00:00:00Z
tags: [audit-logging, security, operations]
status: draft
---

# YTsaurus audit logging

{% note warning "Draft" %}

This article describes the audit logging coverage that is visible in the current server code. Treat it as an implementation-oriented checklist rather than a formal compliance guarantee: if a component does not call the access-log helpers or does not emit a dedicated structured audit log, its actions are not covered by this page.

{% endnote %}

## What YTsaurus calls an audit log

YTsaurus has two audit-oriented logging paths:

* **Cypress access log**. This is the main audit trail for accesses to Cypress objects and for several transaction and tablet lifecycle actions. Records are emitted through the `AccessLogger` structured logger and include the authenticated user, optional user tag, method, object type, object id, path, mutation id, optional original path, extra method-specific attributes, and transaction information when the request is executed in a transaction.
* **Job shell structured log**. This is a separate SOC-audit log for interactive job-shell sessions. It is emitted through the `JobShell` structured logging category and uses job-shell logging context returned by job-prober services.

The access log is controlled by the dynamic `enable_access_log` flag. It defaults to `true` in the master security manager configuration. The Cypress proxy has its own dynamic `enable_access_log` switch for Sequoia/Cypress-proxy request handling.

## Access-log record contents

A regular Cypress access-log record contains the following fields when available:

| Field | Meaning |
| --- | --- |
| `user` | Authenticated user from the current RPC authentication identity. |
| `user_tag` | Effective user tag when it differs from `user`. |
| `method` | Logged method name. Some call sites override the RPC method with a more precise lifecycle method, for example `PrepareMount`. |
| `type` | Cypress object type converted from the logged object id. |
| `id` | Object id. |
| `path` | Resolved Cypress path. For nested YPath requests, the target suffix is appended when appropriate. |
| `original_path` | Original path from the YPath header, when present. |
| `destination_path` / `original_destination_path` | Destination path for copy/move-like operations, when present. |
| `mutation_id` | Hydra mutation id for master-side mutating requests, when available. |
| `transaction_info` | Transaction id, optional title, operation id/title/type copied from transaction attributes, and the parent transaction chain. |

For transaction-only records such as `StartTransaction`, `CommitTransaction`, and `AbortTransaction`, the path is empty and the relevant data is in `transaction_info`.

## Object types covered by the generic Cypress access log

The generic access-log filter covers these Cypress object types:

* `file`
* `journal`
* `table`
* `document`
* `map_node`
* `link`

Other object types are logged only if a specialized call site explicitly emits a record. In particular, the generic filter does not cover all system objects, accounts, users, groups, chunks, operations, scheduler objects, tablet-cell objects, chaos objects, queues, or replicated-table internals.

## Methods covered by the generic Cypress access log

The generic method filter covers:

* `Lock`
* `Unlock`
* `Get`
* `GetKey`
* `Set`
* `Remove`
* `List`
* `Exists`
* `GetBasicAttributes`
* `CheckPermission`
* `LockCopyDestination`
* `LockCopySource`

This list is intentionally narrower than the full YPath API. Generic coverage is therefore best understood as **metadata/path-level access auditing for selected Cypress methods on selected Cypress object types**, not as a full command log.

## Additional master-side actions that are logged

Several master components emit explicit access-log records in addition to the generic filters:

| Component | Logged actions |
| --- | --- |
| Transaction manager | `StartTransaction`, `CommitTransaction`, `AbortTransaction`. |
| Tablet service | Tablet lifecycle phases: `PrepareMount`, `CommitMount`, `AbortMount`, `PrepareUnmount`, `CommitUnmount`, `AbortUnmount`, `PrepareFreeze`, `CommitFreeze`, `AbortFreeze`, `PrepareUnfreeze`, `CommitUnfreeze`, `AbortUnfreeze`, `PrepareRemount`, `CommitRemount`, `AbortRemount`, `PrepareReshard`, `CommitReshard`, `AbortReshard`. |
| Cypress manager and node proxies | Selected create, copy, move, link, lock, unlock, remove, set, list, exists, permission-check, and attribute/basic-attribute paths where the code calls the access-log helper. |
| Chunk-owner and table node proxies | Selected table/file/journal chunk-owner operations where explicit access-log calls are present. |
| Chaos replicated table proxy | Selected chaos replicated table accesses where explicit access-log calls are present. |
| Cypress proxy / Sequoia session | Selected Cypress proxy operations matching the same object-type and method filters, with transaction-chain information reconstructed by the Sequoia session when available. |

## Job-shell audit coverage

Interactive job-shell access is audited separately from the Cypress access log. The HTTP proxy integration test configures a `JobShell` structured logging category and verifies that a `poll_job_shell` request with operation `spawn` produces a structured record containing the started shell id.

The job-prober protocol also carries a `logging_context` field explicitly described as additional attributes for SOC audit via job-shell structured logging. This makes job-shell session creation/auditing a dedicated coverage area, not just a side effect of ordinary HTTP proxy request logging.

## Audit-log writing backends

Audit records are emitted as ordinary YTsaurus structured log events, so the persistence backend is selected by logging `rules` and `writers` rather than by the access-log call sites themselves. A rule must include the relevant category, for example `Access` for Cypress access logs or `JobShell` for job-shell audit records, and must point to one or more writers.

The following writer backends are available in the logging subsystem used by these audit logs:

| Writer `type` | Destination | Notes for audit logging |
| --- | --- | --- |
| `file` | Local file on the component host. | This is the usual backend for master access logs and JobShell structured logs. It supports `file_name`, rotation policy, timestamp suffixes, and optional compression. Use a structured message format such as JSON or YSON. |
| `dynamic_table` | A YTsaurus dynamic table. | The dynamic-table writer can write to ordered or sorted dynamic tables and must be configured with `format = yson`. Configure `table_path`, flush period, backlog watermarks, batch limits, and write backoff. This backend requires the component to initialize the dynamic-table log writer with a native client; this is done by many server components, including masters, schedulers, controller agents, proxies, cluster nodes, job proxies, queue agents, query trackers, and several balancer/cache services. |
| `stderr` | Process standard error. | Registered by the core logging subsystem and useful mostly for debugging, tests, containers, or bootstrap diagnostics. It is not a recommended durable audit backend by itself. |

A Flow runner also registers a `queue` log writer for controller logs in the Flow subsystem. This is not a general YTsaurus server audit-log backend, but it is another example of a component-specific writer factory that can be registered with the same log manager mechanism.

Example file-backed access log:

```yson
{
  logging = {
    rules = [
      {min_level = info; include_categories = [Access]; writers = [audit_file]; message_format = structured;}
    ];
    writers = {
      audit_file = {
        type = file;
        file_name = "logs/access.json.log";
        accepted_message_format = structured;
        rotation_policy = {max_segment_size = 1073741824; max_segment_count_to_keep = 10;}
      };
    };
  };
}
```

Example dynamic-table-backed access log:

```yson
{
  logging = {
    rules = [
      {min_level = info; include_categories = [Access]; writers = [audit_table]; message_format = structured;}
    ];
    writers = {
      audit_table = {
        type = dynamic_table;
        format = yson;
        table_path = "//sys/audit/access_log";
        flush_period = 1s;
        max_batch_row_count = 50000;
        max_batch_weight = 2097152;
      };
    };
  };
}
```

Backend choice affects durability and operations, but not coverage: a method or component is audited only if it emits the `Access` or `JobShell` structured event in the first place. Conversely, the same audit event may be sent to multiple writers by listing multiple writer names in the logging rule.

## Monitoring fullness and durability

Audit logging has two buffering layers to monitor:

1. The **process log manager queue**, shared by all ordinary log writers in the process.
2. The **writer backend buffer**, which is especially important for `dynamic_table` because it has its own asynchronous queue and retry loop.

Monitor both layers for every component that emits audit records. The general logging subsystem exposes Prometheus-style sensors under the `yt_logging_*` prefix and the internal `/logging` profiler path; dynamic-table writer sensors are tagged by writer name and table path. The most important signals are:

| Signal | Applies to | What it means | Operator action |
| --- | --- | --- | --- |
| `/logging/backlog_events` | All logging backends | Number of events accepted by the log manager but not yet written. | Alert if it grows steadily or approaches `high_backlog_watermark`. |
| `/logging/dropped_events` | All logging backends | Events dropped by the log manager, for example after shutdown starts or when logging is suspended because the backlog reached the high watermark. | Treat any increase for `Access`/`JobShell` producers as possible audit loss. |
| `/logging/suppressed_events` | All logging backends | Events suppressed by logging request-suppression mechanisms. | Ensure audit categories are not unintentionally suppressed. |
| `/logging/message_buffers_size` | All logging backends | Memory retained by queued log message buffers. | Use together with backlog events to detect logging pressure before drops. |
| `/logging/min_log_storage_available_space` and `/logging/min_log_storage_free_space` | File-backed logs | Minimum observed space on log storage. | Alert before the configured `min_disk_space` threshold is reached. |
| `/dynamic_table_logging/backlog_events` | `dynamic_table` writer | Events buffered inside the dynamic-table writer. | Alert on sustained growth. |
| `/dynamic_table_logging/backlog_weight` | `dynamic_table` writer | Serialized byte weight buffered inside the dynamic-table writer. | Alert before `high_backlog_weight_watermark`; this is the main fullness indicator for table-backed audit logs. |
| `/dynamic_table_logging/dropped_events` | `dynamic_table` writer | Events dropped by the dynamic-table writer because it is suspended or because write retries were exhausted. | Treat any increase as confirmed audit loss for that writer. |
| `/dynamic_table_logging/flushed_events` and `/dynamic_table_logging/flushed_bytes` | `dynamic_table` writer | Successfully written events and bytes. | Compare rate with expected audit-event production and backlog trend. |

The log manager also emits explicit warning/error messages when logging is close to loss or has lost durability:

* `Backlog size has exceeded high watermark, logging suspended` means non-essential events are being dropped until backlog falls below the low watermark.
* `Backlog size has dropped below low watermark, logging resumed` marks recovery after a suspension.
* `Log file disabled: not enough space available`, `Log file disabled: space check failed`, or `Log file disabled: reload failed` means a file-backed writer is not durable until it is re-enabled.
* `Backlog weight has exceeded high watermark, dynamic table logging suspended` means the dynamic-table writer is dropping events until its backlog weight falls below the low watermark.
* `Error flushing log events to dynamic table` indicates current write failures; if it continues until retries are exhausted, the writer logs `Flush retries exhausted, dropping log events`.

For file-backed audit logs, fullness is primarily a filesystem-retention problem. Configure a dedicated logs location, reserve enough disk, set `min_disk_space`, and choose rotation limits (`max_segment_size`, `max_segment_count_to_keep`, `max_total_size_to_keep`, and/or `rotation_period`) so that audit logs are not deleted before collection. A file writer disables itself when available space drops below `min_disk_space`; that protects the process but creates an audit gap.

For dynamic-table-backed audit logs, fullness is primarily a writer-backlog and destination-health problem. The writer suspends and drops new events above `high_backlog_weight_watermark`, resumes below `low_backlog_weight_watermark`, and may drop a queued batch if configured write retries are exhausted. Monitor the destination table itself as well: tablet health, mount state, write errors, row count/data weight growth, compaction pressure, and retention/TTL policy.

### Detecting lost events and close calls

There is no end-to-end exactly-once audit sequence number in the access-log record format, so loss detection is based on operational signals and reconciliation:

* **Confirmed loss:** any increase in `/logging/dropped_events` on an audit-producing process, any increase in `/dynamic_table_logging/dropped_events` for an audit writer, or `Flush retries exhausted, dropping log events`.
* **Probable or imminent loss:** backlog gauges near high watermarks, logging suspension/resume messages, dynamic-table flush errors, file-writer disabled messages, or log-storage free space approaching `min_disk_space`.
* **Retention loss:** rotated file segments disappearing before the collector ingests them, dynamic-table TTL trimming data before export, or manual truncation/removal of audit destinations.
* **Configuration loss:** missing `Access` or `JobShell` rules, a rule that writes only to `stderr`, disabled `enable_access_log`, or a dynamic-table writer configured in a component that has not initialized the dynamic-table log writer client.

For high-assurance deployments, send audit events to at least two independent destinations, for example a local rotated file collected by an external log pipeline and a YTsaurus dynamic table. Keep alerts on both the producer-side drop counters and the consumer-side ingestion lag. Periodically generate a known audit event with a unique path, transaction title, user tag, or request context, such as a controlled `Get`/`List` on a test Cypress path or a test job-shell spawn in a non-production environment, and verify that it appears exactly where expected in every configured sink within the expected delay.

## What is not tracked, or is only partially tracked

The current audit logging is not a complete, end-to-end history of every user-visible effect in YTsaurus. Important limitations are:

* **Tabular row contents are not audited by the access log.** The access log records object/path-level actions, not every row read, row written, lookup key, dynamic-table tablet request, or query result.
* **Chunk-server and scheduler internals are not comprehensively audited.** Explicit TODO comments remain near some chunk-owner and controller-agent operation paths, which indicates known gaps.
* **Operation lifecycle is not fully covered as an audit log.** Operation id/title/type can appear in `transaction_info` when copied from transaction attributes, but scheduler/controller-agent state transitions, job scheduling decisions, job starts/finishes, and operation progress are not equivalent to access-log records.
* **Administrative configuration and system-object changes are only covered when they pass through logged Cypress methods on logged object types or dedicated call sites.** There is no blanket audit layer for all dynamic-config updates, user/group/account changes, bundle changes, maintenance changes, or orchid/service-control calls.
* **Read-only and failed authorization events are not the same as access-log coverage.** Authorization failures may be reported through normal error/logging paths, but the access log is emitted only from the access-log call sites.
* **Non-Cypress APIs are not automatically included.** HTTP/RPC proxy requests, admin RPCs, tablet-node RPCs, data-node traffic, query engines, CHYT/SPYT/YQL execution, and queue consumers/producers require their own logging paths unless they eventually trigger a covered Cypress access-log call.
* **Multi-master leader/recovery behavior matters.** On masters, access logging is disabled during Hydra recovery. For multi-peer cells, the helper avoids logging on the leader while allowing non-leader peers, which prevents duplicate or unsafe records during replicated mutation handling.
* **Log availability depends on logging configuration and backend health.** Emitting a structured event is not the same as retaining it. Operators still need a logging rule/writer that persists the `Access` and `JobShell` categories to a durable backend such as files or dynamic tables, plus alerts for backlog, drops, disabled writers, and retention lag.

## Practical interpretation

Use the access log to answer questions such as:

* Who accessed, changed, listed, removed, locked, or checked permissions for a file, table, document, map node, link, or journal path?
* Which transaction or operation context was associated with that access?
* Which tablet lifecycle action was applied to a table?
* Was a job-shell session spawned for a job?

Do not use it alone to answer questions such as:

* Which exact table rows or dynamic-table keys did a user read or write?
* Which jobs did the scheduler run on which nodes and why?
* Which low-level chunks were read from or written to by a client?
* Which users/groups/accounts or dynamic configuration entries changed unless the specific change is visible as a covered Cypress access-log record?
* What happened inside a user job, query, CHYT clique, SPYT application, or external system connected to YTsaurus?

## Operator checklist

1. Ensure master dynamic config has `security_manager.enable_access_log = true`.
2. Ensure Cypress proxy dynamic config has `enable_access_log = true` if Cypress proxy / Sequoia access logging is required.
3. Choose a persistence backend: usually rotated files for local collection, or `dynamic_table` when the component supports writing structured logs directly to a YTsaurus dynamic table.
4. Configure structured log writers for the access-log category and verify that records contain `user`, `method`, `path`, and `transaction_info` for test requests.
5. Configure a structured writer for the `JobShell` category if job-shell audit is required.
6. Alert on `/logging/dropped_events`, `/logging/backlog_events`, file-writer disabled messages, and dynamic-table writer `/dynamic_table_logging/*` backlog/drop counters where applicable.
7. For compliance-sensitive workflows, verify the exact command path in code or by test: only call sites that reach the access-log helpers or the `JobShell` structured logger are covered.
