---
type: Draft Article
title: "Draft-26: Controller agent configuration"
last_modified: 2026-09-11T00:00:00+00:00
tags: [controller-agent, configuration, operations, jobs]
status: draft
---

# Controller agent configuration

This article is a draft reference for administrators of a {{product-name}} cluster. It describes the controller-agent-specific part of the static configuration, the dynamic configuration document, and the cluster-wide defaults and limits that controller agents apply to operations and jobs.

## Configuration layers

A controller agent reads the following layers, in this order:

1. The process configuration file. Controller-agent settings are below `controller_agent`; the other top-level sections are common server settings such as `cluster_connection`, `logging`, `rpc_port`, `monitoring_port`, `address_resolver`, and `solomon_exporter`.
2. The document at `//sys/controller_agents/config`. Its value is a recursive patch over `controller_agent`. The agent polls it every `config_update_period`.
3. For operation settings, `operation_options` is applied to every operation type and then the matching `<type>_operation_options` patch is applied.
4. `spec_template` in the resulting operation-options object supplies defaults for fields omitted from the submitted operation spec. An explicitly supplied spec value wins.

Thus the dynamic document does **not** contain a `controller_agent` wrapper:

```bash
# Create the document once.
yt create document //sys/controller_agents/config --attributes '{value={}}'

# Change a value dynamically.
yt set //sys/controller_agents/config/user_file_limits/max_size 53687091200

# Inspect the effective value on every agent before considering the rollout complete.
yt get //sys/controller_agents/instances/<address>/orchid/controller_agent/config
```

Use the dynamic document for routine tuning. Keep bootstrap-only fields in the process file. Removing a field from the dynamic document restores the value inherited from the static configuration (or its built-in default); it does not necessarily restore the value immediately if an already running operation copied that setting at initialization. Invalid patches are rejected and reported through logs/alerts. Durations are expressed in milliseconds in YSON unless the consuming type supports a textual form; byte sizes are integer bytes.

## Static process configuration

The controller-agent-specific static root has these fields:

| Option | Description |
|---|---|
| `controller_agent` | Full controller subsystem configuration. It is also the schema patched by the dynamic document described below. |
| `addresses` | Known scheduler network addresses. |
| `cypress_annotations` | Attributes published with the controller-agent instance in Cypress. |
| `abort_on_unrecognized_options` | Abort startup instead of merely reporting unknown configuration keys. |
| `heap_profiler` | Heap-profiler configuration. |

The inherited native-server/program options configure the cluster connection, RPC and monitoring ports, logging, address resolution, resource limits, diagnostics, and authentication. They are common to server binaries rather than controller-agent options; configure them in the same way as for other native servers. Only the five fields above are added by the controller-agent bootstrap.

`controller_agent` may be populated completely in the static file. Every option in [Dynamic controller configuration](#dynamic-controller-configuration) is legal there. Conversely, only the `controller_agent` subtree is patched from `//sys/controller_agents/config`; `addresses`, `heap_profiler`, ports, logging, and other bootstrap fields require a process restart.

## Defaults and limits for operations and jobs

### Precedence and operation types

`operation_options` is the common patch. The type-specific fields are `map_operation_options`, `reduce_operation_options`, `join_reduce_operation_options`, `erase_operation_options`, `ordered_merge_operation_options`, `unordered_merge_operation_options`, `sorted_merge_operation_options`, `map_reduce_operation_options`, `sort_operation_options`, `remote_copy_operation_options`, and `vanilla_operation_options`.

The effective value is built as follows:

```text
built-in defaults
  <- operation_options
  <- <operation_type>_operation_options
  <- fields explicitly present in the user's operation spec (for spec_template only)
```

Changing a type-specific object replaces its dynamic patch, so include every sibling value that must remain customized. Prefer setting an individual path with `yt set` over replacing a large map.

### Common operation options

Every type supports the following fields:

| Option | Built-in default | Purpose or enforced bound |
|---|---:|---|
| `spec_template` | `{}` | Defaults merged into a submitted operation spec. This is the principal mechanism for cluster-wide operation defaults. |
| `slice_data_weight_multiplier` | `0.51` | Controls initial input slicing; must be positive. Alias: `slice_data_size_multiplier`. |
| `max_data_slices_per_job` | `1000` | Default primary-slice cap for a user job; must be positive. |
| `max_data_slices_per_job_limit` | `10000` | Upper bound on the value a user may request; must be positive. |
| `max_slice_data_weight` / `min_slice_data_weight` | `1 GiB` / `1 MiB` | Bounds an individual slice; both must be positive. The former `*_data_size` names are aliases. |
| `max_input_table_count` | `3000` | Per-operation-type input-table limit; must be positive. A separate global limit is described below. |
| `max_output_tables_times_jobs_count` | `2000000` | Protects the controller from the product of output count and job count; minimum accepted configuration is `100000`. |
| `job_splitter` | see below | Long-job splitting and speculation policy. |
| `max_build_retry_count` | `5` | Number of retries after increasing `data_weight_per_job`; non-negative. |
| `data_weight_per_job_retry_factor` | `2.0` | Retry multiplier; greater than `1`. |
| `cpu_limit_overcommit_mode` | `linear` | How the slot-container CPU allowance is calculated. |
| `initial_cpu_limit_overcommit` | `2.0` | Additive CPU allowance; non-negative. |
| `cpu_limit_overcommit_multiplier` | `1.0` | Multiplicative CPU allowance; at least `1`. |
| `set_container_cpu_limit` | `false` | Enforce the computed slot-container CPU limit (Porto). |
| `min_cpu_limit` | `0` | Smallest CPU limit accepted from a user; non-negative. |
| `set_slot_container_memory_limit` | `false` | Enforce a slot-container memory limit. |
| `slot_container_memory_overhead` | `0` | Memory added to that limit; non-negative. |
| `controller_building_job_spec_count_limit` | initialized by the common patch | Throttle concurrent job-spec builds at this count. |
| `controller_total_building_job_spec_slice_count_limit` | initialized by the common patch | Throttle builds at this aggregate slice count. |
| `custom_statistics_count_limit` | `1024` | Maximum aggregated custom-statistic count. |
| `user_job.thread_limit_formula` | `250000` | Formula in `cpu` for the user container's thread limit. |
| `gpu_check` | empty | `layer_paths`, `binary_path`, `binary_args`, and optional `network_project` for a GPU validation command. |
| `allow_locality` | `true` | Whether this operation type may use locality. |

`job_splitter` contains `min_job_time` (`60 s`), `exec_to_prepare_time_ratio` (`20`), `no_progress_job_time_to_average_prepare_time_ratio` (`20`), `min_total_data_weight` (`1 GiB`), `update_period` (`60 s`), `candidate_percentile` (`0.8`, range `[0.5,1]`), `residual_job_factor` (`0.8`, range `(0,1]`), `residual_job_count_min_threshold` (`10`), `max_jobs_per_split` (`5`), `max_input_table_count` (`100`), `split_timeout_before_speculate` (`5 min`), `job_logging_period` (`3 min`), `enable_job_splitting` (`true`), `enable_job_speculation` (`true`), and `show_running_jobs_in_progress` (`false`). `min_total_data_size` is an alias for `min_total_data_weight`.

### Type-specific operation options

Map, ordered/unordered/sorted merge, reduce, join-reduce, erase, and remote-copy options inherit the common fields. The simple-operation fields are `max_job_count` (default `100000`), `data_weight_per_job` (default `256 MiB`, but `128 MiB` for map and reduce and `20 GiB` for merge; positive), and optional `job_size_adjuster` (enabled by default for map). Reduce and join-reduce use the sorted-merge shape; erase uses the ordered-merge shape. Remote copy additionally has `cpu_limit` (default `0.1`) and optional `networks`.

Sort and map-reduce inherit the common fields and add `max_partition_job_count` (`500000`), `max_new_partition_count` (`2000000`), `max_partition_factor` (`500`), `max_sample_size` (`10 KiB`), `compressed_block_size` (`1 MiB`), `min_partition_weight` (`256 MiB`), `min_uncompressed_block_size` (`100 KiB`), `max_value_count_per_simple_sort_job` (`10000000`), `partition_job_size_adjuster`, `sorted_merge_job_size_adjuster`, `data_balancer`, `default_partition_data_weight_for_merging` (`128 MiB`), and `enable_final_partitions_merging_by_default` (`true`). The spelling `max_new_partition_count` is retained for compatibility although it limits total partitions. `data_balancer` contains `logging_min_consecutive_violation_count` (`1000`), `logging_period` (`1 min`), `tolerance` (`2.0`), and `use_node_io_weight` (`true`).

Vanilla adds `max_task_count` (`100`), `max_total_job_count` (`100000`), and `gang_manager.job_reincarnation_timeout` (`20 min`).

### Global limits that users cannot bypass

The top-level limits apply independently of templates: `operation_time_limit`, `max_input_table_count`, `max_output_table_count`, `max_ranges_on_table`, `max_user_file_count`, `max_total_slice_count`, `allocation_job_count_limit`, `secure_vault_length_limit`, `max_unversioned_dynamic_table_output_chunk_size`, and `max_unversioned_dynamic_table_output_block_size`. `max_output_tables_times_jobs_count`, `max_data_slices_per_job_limit`, and the other per-type limits above add further protection.

Artifacts are governed by `user_file_limits`: `max_size` (default `10 GiB`), `max_table_data_weight` (`10 GiB`), and `max_chunk_count` (`1000`). `user_file_limits_per_tree` maps a scheduling-tree name to a partial override containing the same three fields. This allows, for example, a specialized tree to accept larger model layers without weakening limits for every operation:

```yson
{
  user_file_limits = {
    max_size = 10737418240;
    max_table_data_weight = 10737418240;
    max_chunk_count = 1000;
  };
  user_file_limits_per_tree = {
    gpu = {max_size = 53687091200;};
  };
  operation_options = {
    spec_template = {
      max_failed_job_count = 20;
    };
  };
  vanilla_operation_options = {
    max_task_count = 200;
  };
}
```

## Dynamic controller configuration

The following is an exhaustive index of the controller-agent-owned keys accepted by the current schema. A nested object named here is expanded in the next section. Shared infrastructure objects (for example, throttlers, readers, fetchers, reporters, RPC server configuration, and synchronizers) use their component's standard schema.

### Snapshots, transactions, and operation lifecycle

* Chunk-list management: `chunk_list_preallocation_count`, `max_chunk_list_allocation_count`, `chunk_list_watermark_count`, `chunk_list_allocation_multiplier`, `desired_chunk_lists_per_release`, `max_children_per_attach_request`, and `intermediate_output_master_cell_count`.
* Snapshot management: `enable_snapshot_building`, `enable_snapshot_building_disabled_alert`, `snapshot_period`, `snapshot_timeout`, `snapshot_fork_timeout`, `operation_controller_suspend_timeout`, `parallel_snapshot_builder_count`, `snapshot_writer`, `enable_snapshot_loading`, `enable_snapshot_phoenix_schema_during_snapshot_loading`, `enable_snapshot_loading_disabled_alert`, and `snapshot_reader`.
* Lifecycle and transactions: `transactions_refresh_period`, `operations_update_period`, `intermediate_account_usage_update_period`, `chunk_unstage_period`, `operation_initialization_timeout`, `operation_transaction_timeout`, `operation_transaction_ping_period`, `operation_time_limit`, `operation_time_limit_check_period`, `operation_progress_log_backoff`, `operation_build_progress_period`, `task_update_period`, `check_tentative_tree_eligibility_period`, and `update_account_resource_usage_leases_period`.
* Revival, retention, and archives: `enable_job_revival`, `zombie_operation_orchids`, `max_retained_jobs_per_operation`, `max_archived_job_spec_count_per_operation`, `guaranteed_archived_job_spec_count_per_operation`, `min_job_duration_to_archive_job_spec`, `enable_operation_progress_archivation`, `operation_progress_archivation_timeout`, `enable_controller_features_archivation`, `enable_job_archive_ttl`, and `operation_events_reporter`.

### Scheduler communication and execution

* Scheduler connection: `scheduler_handshake_rpc_timeout`, `scheduler_handshake_failure_backoff`, `scheduler_heartbeat_rpc_timeout`, `scheduler_heartbeat_failure_backoff`, `scheduler_heartbeat_period`, `schedule_allocation_heartbeat_period`, `config_update_period`, `exec_nodes_update_period`, `operations_push_period`, `operation_job_metrics_push_period`, `operation_alerts_push_period`, and `suspicious_jobs_push_period`.
* Node information and demand: `available_exec_nodes_check_period`, `banned_exec_nodes_check_period`, `controller_exec_node_info_update_period`, `scheduling_tag_filter_expire_timeout`, `resource_demand_sanity_check_period`, `safe_online_node_count`, `safe_scheduler_online_time`, `enable_network_in_operation_demand`, and `fail_operations_in_empty_trees`.
* Scheduling execution: `schedule_allocation_controller_queue`, `job_events_controller_queue`, `schedule_allocation_throttling`, `schedule_allocation_statistics_log_backoff`, `schedule_allocation_statistics_moving_average_window_size`, `controller_throttling_log_backoff`, `invoker_pool_total_time_aggregation_period`, `job_events_total_time_threshold`, `allocation_job_count_limit`, and `release_failed_job_on_exception`.
* Threads: `controller_thread_count`, `chunk_scraper_thread_count`, `job_spec_build_thread_count`, `statistics_offload_thread_count`, and `max_concurrent_safe_core_dumps`.

### Input, output, tables, and chunks

* Fetching and slicing: `max_chunks_per_fetch`, `max_chunks_per_locate_request`, `fetcher`, `chunk_slice_fetcher`, `chunk_location_throttler`, `chunk_scraper`, `chunk_teleporter`, `max_total_slice_count`, and `controller_row_buffer_chunk_size`.
* Table limits and behavior: `max_input_table_count`, `max_output_table_count`, `max_ranges_on_table`, `use_columnar_statistics_default`, `enable_columnar_statistics_early_finish`, `enable_table_column_renaming`, `lock_non_atomic_output_dynamic_tables`, `register_lockable_dynamic_tables`, `allow_bulk_insert_under_user_transaction`, `enable_bulk_insert_for_everyone`, `enable_merge_schemas_during_schema_infer`, and `fail_operation_on_errors_in_live_preview`.
* Writers: `desired_block_size`, `max_estimated_write_buffer_size`, `write_buffer_memory_overrun_alert_factor`, `max_unversioned_dynamic_table_output_chunk_size`, `max_unversioned_dynamic_table_output_block_size`, and `enable_dynamic_table_output_chunk_constraint_validation`.
* Remote copy and media: `enable_versioned_remote_copy`, `enable_hunks_remote_copy`, `enable_compression_dictionary_remote_copy`, `remote_operations`, `obligatory_account_media`, `deprecated_media`, `nbd_media`, `fast_intermediate_medium`, `fast_intermediate_medium_limit`, and `forbid_operations_on_offshore_media`.
* Compatibility backoff fields for bulk-insert locking: `dynamic_table_lock_checking_attempt_count_limit`, `dynamic_table_lock_checking_interval_scale`, `dynamic_table_lock_checking_interval_duration_min`, and `dynamic_table_lock_checking_interval_duration_max`.

### Jobs, resources, and containers

* Job behavior: `enable_job_splitting`, `enable_job_interrupts`, `enable_locality`, `heavy_job_spec_slice_count_threshold`, `job_spec_slice_throttler`, `job_spec_codec`, `job_tracker`, `max_job_aborts_until_operation_failure`, `enable_job_fails_tolerance`, and `max_job_thread_count_formula`.
* Job-size adjustment switches: `enable_map_job_size_adjustment`, `enable_ordered_map_job_size_adjustment`, `enable_partition_map_job_size_adjustment`, `enable_ordered_partition_map_job_size_adjustment`, and `enable_sorted_merge_in_sort_job_size_adjustment`.
* Memory prediction and limits: `user_job_memory_digest_precision`, `user_job_memory_reserve_quantile`, `job_proxy_memory_reserve_quantile`, `memory_digest_resource_overdraft_factor`, `user_job_resource_overdraft_memory_multiplier`, `job_proxy_resource_overdraft_memory_multiplier`, `use_resource_overdraft_memory_multiplier_from_spec`, `total_controller_memory_limit`, `memory_watchdog`, `memory_usage_profiling_period`, `footprint_memory`, and `exec_footprint_memory`.
* Sandbox and environment: `environment`, `max_user_file_count`, `user_file_limits`, `user_file_limits_per_tree`, `system_layer_path`, `default_layer_path`, `enable_bypass_artifact_cache`, `secure_vault_length_limit`, `default_enable_porto`, `iops_threshold`, and `iops_throttler_limit`.
* GPU and networking: `cuda_toolkit_layer_directory_path`, `docker_registry`, `enable_job_profiling`, `cuda_profiler_layer_path`, `cuda_profiler_environment_variables`, `cuda_profiler_environment`, `network_projects_allowed_for_offloading`, and `udf_registry_path`.

### Observability, alerts, and diagnostics

* Orchid and caches: `controller_static_orchid_update_period`, `controller_orchid_keys_update_period`, `static_orchid_cache_update_period`, `cached_running_jobs_update_period`, `running_job_statistics_update_period`, `max_running_job_statistics_update_count_per_heartbeat`, and `running_allocation_time_statistics_updates_send_period`.
* Metrics and reports: `event_log`, `job_metrics_report_period`, `custom_job_metrics`, `job_reporter`, `enable_master_resource_usage_accounting`, and `operation_events_reporter`.
* Alerts: `enable_unrecognized_alert`, `alerts_update_period`, `alert_manager`, `suspicious_jobs`, and `write_buffer_memory_overrun_alert_factor`.
* Miscellaneous diagnostics and access: `legacy_live_preview_user_blacklist`, `allow_users_group_read_intermediate_data`, `heavy_request_immediate_response_timeout`, `rpc_server`, `master_cell_directory_synchronizer`, and `tags`.
* Test-only controls: `enable_controller_failure_spec_option` and `testing_options`. Do not enable these in production.

### Operation-default objects

The complete set is `operation_options`, `map_operation_options`, `reduce_operation_options`, `join_reduce_operation_options`, `erase_operation_options`, `ordered_merge_operation_options`, `unordered_merge_operation_options`, `sorted_merge_operation_options`, `map_reduce_operation_options`, `sort_operation_options`, `remote_copy_operation_options`, and `vanilla_operation_options`. Their fields and precedence are described above.

## Nested controller-agent objects

This section completes the index for controller-agent-specific nested schemas.

* `chunk_scraper`: `restart_timeout` (default `10 s`).
* `testing_options`: `enable_snapshot_cycle_after_materialization`, `rootfs_test_layers`, `delay_in_unregistration`, `delay_in_handshake`, `abort_output_transaction_after_completion_transaction_commit`, and `enable_events_on_fs`.
* `suspicious_jobs`: `inactivity_timeout`, `cpu_usage_threshold`, `input_pipe_time_idle_fraction`, `output_pipe_time_idle_fraction`, `update_period`, `max_orchid_entry_count_per_type`, and `min_required_cpu_limit`.
* `zombie_operation_orchids`: `limit`, `clean_period`, and `enable`.
* `user_job_monitoring`: `default_max_monitored_user_jobs_per_operation`, `extended_max_monitored_user_jobs_per_operation`, `enable_extended_max_monitored_user_jobs_per_operation`, `max_monitored_user_jobs_per_agent`, and the compatibility-spelled `max_monitired_gang_jobs_per_agent`.
* `memory_watchdog`: `total_controller_memory_limit`, `operation_controller_memory_limit`, `operation_controller_memory_overconsumption_threshold`, and `memory_usage_check_period`.
* `job_tracker`: `node_disconnection_timeout`, `revival_node_disconnection_timeout`, `job_confirmation_timeout`, `logging_job_sample_size`, `duration_before_job_considered_disappeared_from_node`, `check_node_heartbeat_sequence_number`, `heavy_invoker_thread_count`, and nested `testing_options`, whose only field is `delay_in_settle_job`.
* `docker_registry`: `internal_registry_address`, `internal_registry_alternative_addresses`, `internal_registry_regex`, `use_yt_token_for_internal_registry`, `forward_internal_images_to_job_specs`, and `translate_internal_images_into_layers`.
* Each `remote_operations.<cluster>` entry: `allowed_users`, `allowed_for_everyone`, `max_total_data_weight`, and `networks`.
* `schedule_allocation_throttling`: `schedule_allocation_total_time_threshold` and `job_events_total_time_threshold`.
* `alert_manager`: `period`, `tmpfs_alert_max_unused_space_ratio`, `tmpfs_alert_min_unused_space_threshold`, `tmpfs_alert_memory_usage_mute_ratio`, `memory_usage_alert_max_unused_size`, `memory_usage_alert_max_unused_ratio`, `memory_usage_alert_max_job_count`, `memory_reserve_factor_alert_max_unused_ratio`, `aborted_jobs_alert_max_aborted_time`, `aborted_jobs_alert_max_aborted_time_ratio`, `short_jobs_alert_min_job_duration`, `short_jobs_alert_min_job_count`, `short_jobs_alert_min_allowed_operation_duration_to_max_job_duration_ratio`, `intermediate_data_skew_alert_min_partition_size`, `intermediate_data_skew_alert_min_interquartile_range`, `job_spec_throttling_alert_activation_count_threshold`, `low_cpu_usage_alert_min_execution_time`, `low_cpu_usage_alert_min_average_job_time`, `low_cpu_usage_alert_cpu_usage_threshold`, `low_cpu_usage_alert_statistics`, `low_cpu_usage_alert_job_states`, `high_cpu_wait_alert_min_average_job_time`, `high_cpu_wait_alert_threshold`, `high_cpu_wait_alert_statistics`, `high_cpu_wait_alert_job_states`, `operation_too_long_alert_min_wall_time`, `operation_too_long_alert_estimate_duration_threshold`, `low_gpu_usage_alert_min_total_gpu_duration`, `low_gpu_usage_alert_min_duration`, `low_gpu_usage_alert_gpu_usage_threshold`, `low_gpu_usage_alert_gpu_utilization_power_threshold`, `low_gpu_usage_alert_gpu_utilization_sm_threshold`, `low_gpu_usage_alert_statistics`, `low_gpu_usage_alert_job_states`, `low_gpu_power_usage_on_window`, `queue_total_time_estimate_threshold`, and `task_unavailable_network_bandwidth_time_ratio_alert_threshold`. `queue_average_wait_time_threshold` is an alias. `low_gpu_power_usage_on_window` contains `window_size`, `record_period`, and `threshold`.

The nested shared schemas are `snapshot_writer`, `snapshot_reader`, `chunk_location_throttler`, `event_log`, `fetcher`, `chunk_slice_fetcher`, both job-size adjusters, `job_spec_slice_throttler`, `job_reporter`, `chunk_teleporter`, `rpc_server`, `master_cell_directory_synchronizer`, `operation_events_reporter`, and `cuda_profiler_environment`. Their fields are maintained by the corresponding reusable library and are not controller-agent-specific options.

Within operation options, the nested object keys themselves are `user_job`, `gpu_check`, `job_splitter`, `data_balancer`, and `gang_manager`; their leaf fields include `thread_limit_formula` and `job_reincarnation_timeout` as described above.

## Safe rollout procedure

1. Read and save the current document: `yt get //sys/controller_agents/config > controller-agents.yson`.
2. Modify the narrowest possible path. Keep operation defaults permissive enough for existing workloads and use hard limits only to protect the cluster.
3. Check logs for parse or validation errors.
4. Compare `orchid/controller_agent/config` on **all** instances. Agents poll independently, so observing one instance is insufficient.
5. Start a small canary operation in every relevant scheduling tree. Defaults are most safely verified on a newly started operation.
6. To roll back, restore the saved document or remove only the newly added key, then repeat the Orchid check.

Be especially cautious with snapshots/revival, transaction timeouts, controller memory, chunk/slice caps, and operation templates. These options can change recovery behavior or reject workloads cluster-wide. Testing fields, compatibility fields, and options whose names mention archivation or schema protocols should normally be changed only during a coordinated version rollout.
