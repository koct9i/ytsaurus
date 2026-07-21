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
| `dynamic_table` | A YTsaurus dynamic table. | The dynamic-table writer can write to ordered or sorted dynamic tables and must be configured with `format = yson`. Configure `table_path`, flush period, backlog watermarks, batch limits, and write backoff. This backend requires the component to initialize the dynamic-table log writer with a native client; it is available in components that do so, such as Cypress proxy, job proxy, master cache, chaos cache, and tablet balancer. |
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
        flush_period = 1000;
        max_batch_row_count = 50000;
        max_batch_weight = 2097152;
      };
    };
  };
}
```

Backend choice affects durability and operations, but not coverage: a method or component is audited only if it emits the `Access` or `JobShell` structured event in the first place. Conversely, the same audit event may be sent to multiple writers by listing multiple writer names in the logging rule.

## What is not tracked, or is only partially tracked

The current audit logging is not a complete, end-to-end history of every user-visible effect in YTsaurus. Important limitations are:

* **Tabular row contents are not audited by the access log.** The access log records object/path-level actions, not every row read, row written, lookup key, dynamic-table tablet request, or query result.
* **Chunk-server and scheduler internals are not comprehensively audited.** Explicit TODO comments remain near some chunk-owner and controller-agent operation paths, which indicates known gaps.
* **Operation lifecycle is not fully covered as an audit log.** Operation id/title/type can appear in `transaction_info` when copied from transaction attributes, but scheduler/controller-agent state transitions, job scheduling decisions, job starts/finishes, and operation progress are not equivalent to access-log records.
* **Administrative configuration and system-object changes are only covered when they pass through logged Cypress methods on logged object types or dedicated call sites.** There is no blanket audit layer for all dynamic-config updates, user/group/account changes, bundle changes, maintenance changes, or orchid/service-control calls.
* **Read-only and failed authorization events are not the same as access-log coverage.** Authorization failures may be reported through normal error/logging paths, but the access log is emitted only from the access-log call sites.
* **Non-Cypress APIs are not automatically included.** HTTP/RPC proxy requests, admin RPCs, tablet-node RPCs, data-node traffic, query engines, CHYT/SPYT/YQL execution, and queue consumers/producers require their own logging paths unless they eventually trigger a covered Cypress access-log call.
* **Multi-master leader/recovery behavior matters.** On masters, access logging is disabled during Hydra recovery. For multi-peer cells, the helper avoids logging on the leader while allowing non-leader peers, which prevents duplicate or unsafe records during replicated mutation handling.
* **Log availability depends on logging configuration.** Emitting a structured event is not the same as retaining it. Operators still need a logging rule/writer that persists the `Access` and `JobShell` categories to a durable backend such as files or dynamic tables.

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
6. For compliance-sensitive workflows, verify the exact command path in code or by test: only call sites that reach the access-log helpers or the `JobShell` structured logger are covered.
