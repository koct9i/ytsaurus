<!--
Draft number: 11
Author: AI agent (GitHub Copilot)
Created: 2026-05-27
Status: In progress
Target: user-guide/dynamic-tables/queues
-->

# Queues — corner cases, performance analysis, and administration how-to

## Corner cases and behavior details

- Queue ordering and offsets are per partition. For multi-tablet queues, treat each `partition_index` as an independent stream and store offsets independently.
- `register_queue_consumer` supports optional `partitions`. If this field is set, the registration and lag/trimming logic for this consumer apply only to listed partitions.
- Consumer offset is the index of the first unread row. If trimming has already removed older rows, reading with an older offset starts from the current `lower_row_index` of the partition.
- In `@queue_consumer_partitions`, `disposition` can be:
  - `ahead` — consumer offset is greater than queue `upper_row_index` (for example after queue recreation or manual offset move).
  - `up_to_date` — no unread rows.
  - `pending_consumption` — unread rows are still available in the queue.
  - `expired` — unread rows exist logically, but some were already trimmed and cannot be read anymore.
- Automatic trimming by Queue Agent requires both:
  - queue `@auto_trim_config/enable = %true`;
  - at least one **vital** registration for the queue.  
  If there are no vital consumers, Queue Agent does not trim.
- `queue_agent_banned=%true` on a queue or consumer disables Queue Agent leading actions for that object (including trimming and exports for queues).
- For replicated and chaos-replicated queues, `static_export_config` must be set on queue replicas, not on the replicated table object itself.

## Performance analysis

For regular diagnostics, use Queue Agent status attributes as a low-frequency introspection source (not as a high-QPS API):

- Queue-level throughput:
  - `@queue_status/write_row_count_rate`
  - `@queue_status/write_data_weight_rate`
- Per-partition backlog basis:
  - `@queue_partitions/*/available_row_count`
  - `@queue_partitions/*/available_data_weight`
  - `@queue_partitions/*/commit_idle_time`
- Consumer lag and read speed:
  - `@queue_consumer_partitions/*/*/unread_row_count`
  - `@queue_consumer_partitions/*/*/unread_data_weight`
  - `@queue_consumer_partitions/*/*/processing_lag`
  - `@queue_consumer_status/queues/*/read_row_count_rate`
  - `@queue_consumer_status/queues/*/read_data_weight_rate`

Interpretation notes:

- Small negative lag/unread values may appear transiently during asynchronous updates.
- `unread_data_weight` may be null for `ahead` and `expired` dispositions.
- Queue data-weight fields (`trimmed_data_weight`, `available_data_weight`) are approximate by design.
- For static exports, table-lag values are approximate, especially with cron-like schedules.

## Queue Agent administration how-to

1. Initialize (or migrate) Queue Agent state tables in `//sys/queue_agents`:
   ```bash
   python3 yt/python/yt/environment/init_queue_agent_state.py \
       --proxy <cluster_proxy> --root //sys/queue_agents --latest
   ```
2. Check Queue Agent instances:
   ```bash
   yt ls //sys/queue_agents/instances
   ```
3. Inspect component health on an instance:
   ```bash
   yt get //sys/queue_agents/instances/<instance>/orchid/queue_agent/pass_error
   yt get //sys/queue_agents/instances/<instance>/orchid/cypress_synchronizer/pass_error
   yt get //sys/queue_agents/instances/<instance>/orchid/queue_agent_sharding_manager/pass_error
   yt get //sys/queue_agents/instances/<instance>/orchid/queue_agent/controller_info
   ```
4. Temporarily remove an unhealthy Queue Agent instance from sharding:
   ```bash
   yt set //sys/queue_agents/instances/<instance>/@banned %true
   ```
   Re-enable:
   ```bash
   yt set //sys/queue_agents/instances/<instance>/@banned %false
   ```
5. Route objects to a specific Queue Agent stage:
   - Set queue/consumer attribute `@queue_agent_stage` to match Queue Agent `stage` config.
   - Use `@queue_agent_banned=%true` on a specific queue/consumer to stop Queue Agent actions for that object while troubleshooting.
