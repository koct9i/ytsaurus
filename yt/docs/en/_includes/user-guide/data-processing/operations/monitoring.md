# Monitoring operations: status, progress, results, metrics, and logs

This page describes how to observe an operation through its full lifecycle:
- while it is running;
- right after it finishes;
- later, when operation data is available mostly from the operations archive.

Use this page together with:
- [Operation types overview](../../../../user-guide/data-processing/operations/overview.md);
- [API command reference](../../../../api/commands.md);
- [MapReduce debugging](../../../../user-guide/problems/mapreduce-debug.md).

## In the web interface

For day-to-day monitoring, start from the **Operations** page:
- check operation `state`;
- inspect `progress` and job counters;
- open operation details and job details;
- inspect failed jobs and their stderr.

## During execution: fast checks

Use these API commands most often:
- [`get_operation`](../../../../api/commands.md#get_operation): current `state`, `progress`, `brief_progress`, `alerts`, `result`;
- [`list_jobs`](../../../../api/commands.md#list_jobs): running/completed/failed jobs, optional stderr-aware filtering;
- [`get_job_stderr`](../../../../api/commands.md#get_job_stderr): stderr for a specific job.

Typical workflow:
1. Poll `get_operation` for `state` and `progress`.
2. If progress stalls or state becomes `failed`, call `list_jobs` to locate problematic jobs.
3. Call `get_job_stderr` for failed jobs and inspect operation `result` and `alerts`.

## Spec options that simplify diagnostics and later lookup

When starting an operation, these spec fields are especially useful for observability:

- `annotations` — structured metadata persisted to the operations archive and usable in archive search/filter flows.
- `title`, `description`, `alias` — improve discoverability in UI and API-based lookup.
- `suspend_on_job_failure` + `max_failed_job_count` — keep failed operations in a debuggable state instead of fast final failure in noisy scenarios.
- `max_stderr_count` — controls how many job stderrs are retained for the operation (up to 150).
- `max_stderr_size` (user job option) — controls per-job stderr truncation threshold.
- `stderr_table_path` — writes full stderr of jobs (except `aborted`) to your table for longer retention and offline analysis.
- `core_table_path` — stores core dump metadata/data for postmortem analysis.

For exact definitions, see [Operation options](../../../../user-guide/data-processing/operations/operations-options.md).

## After finish: result and diagnostics

When the operation reaches a terminal state (`completed`, `failed`, `aborted`):
- get the final verdict from `state`;
- inspect `result` for error details (for failed/aborted cases);
- inspect `events` and `alert_events` (if requested);
- keep using `list_jobs` and `get_job_stderr` for job-level diagnostics.

For bulk stderr collection patterns, see [Debugging MapReduce programs](../../../../user-guide/problems/mapreduce-debug.md).

## Historical lookup and operations archive

Finished operations are eventually cleaned from Cypress and served from `//sys/operations_archive`.

For efficient historical queries:
- use [`list_operations`](../../../../api/commands.md#list_operations) with `include_archive=true`;
- always set a narrow time window (`from_time`, `to_time`) for archive-backed requests;
- add filtering (`user`, `state`, `type`, `pool`, `pool_tree`, `with_failed_jobs`, `filter`) early.

Notes:
- with `include_archive=true`, `from_time` and `to_time` are required for archive querying;
- `get_operation` works both for running and finished operations, but completed operations may be served from archive depending on lifecycle stage.

## Stderr retention limits and pruning behavior

Stderr data is retained with several limits:
- Per job, only up to `max_stderr_size` bytes are kept; extra bytes are dropped.
- Scheduler-level cap: at most 150 job stderrs are retained.
- Operation-level cap: by default, up to 20 job stderrs are retained; this limit can be raised (up to 150, including via `max_stderr_count`).

If you need longer and fuller retention, configure `stderr_table_path` in operation spec.

## How long operation/job data lives in archive

There is no single fixed retention duration in public user docs: archive/data lifetime is cluster-policy dependent.

Practical implications:
- `get_job`/`list_jobs` return data when job info is still available; `archive_job_count` shows how much came from archive.
- `has_spec`, `stderr_size`, and related job fields indicate what exactly was preserved.
- For guaranteed long-term diagnostics artifacts, save them explicitly (for example, `stderr_table_path`) and/or export structured logs.

## Log rotation and pruning (cluster logs)

For server debug/structured logs written to files, rotation is configured by `rotationPolicy`:
- `rotationPeriodMilliseconds`
- `maxSegmentSize`
- `maxTotalSizeToKeep` (oldest segments are deleted)
- `maxSegmentCountToKeep` (oldest segments are deleted)

When `writerType=stderr`, rotation settings are ignored.
See [Configuring server component logging](../../../../admin-guide/logging.md).

## Metrics and alerts

Track operation health at two levels:

1. **Operation-level signals**:
   - `state` transitions;
   - `progress` / `brief_progress`;
   - operation `alerts`;
   - failed jobs and stderr volume.

2. **Cluster-level monitoring**:
   - scheduler/controller-agent alerts and uptime checks;
   - quantitative metrics in Prometheus/Grafana;
   - qualitative checks in Odin.

For cluster-level setup and key checks, see [Monitoring](../../../../admin-guide/monitoring.md).

## Practical checklist

For an operation that looks unhealthy:
1. `get_operation` → verify `state`, `progress`, `alerts`, `result`.
2. `list_jobs` → isolate failed/stuck jobs.
3. `get_job_stderr` → inspect concrete job errors.
4. If operation already finished/cleaned, switch to archive-oriented `list_operations(include_archive=true, from_time, to_time)` queries.
