# YQL-agent configuration and administration

This page summarizes practical YQL-agent admin settings: base configuration, DQ, multicluster execution, Query Tracker integration, and caching.

## Where to configure YQL-agent

For operator-managed clusters, overrides are usually provided in `ytserver-yql-agent.yson` via `configOverrides` (see [Configuration overrides](../admin-guide/config-overrides.md)).

The service root key is `yql_agent`.

Skeleton example:

```json
ytserver-yql-agent.yson: |
    {"yql_agent"={
        "gateway_config" = {};
        "dq_gateway_config" = {};
        "dq_manager_config" = {};
        "process_plugin_config" = {};
    };}
```

## Administrative paths in Cypress

- YQL-agent root: `//sys/yql_agent`.
- Dynamic config path: `//sys/yql_agent/config`.
- Instances: `//sys/yql_agent/instances/<host:rpc_port>`.
- Instance Orchid path: `//sys/yql_agent/instances/<host:rpc_port>/orchid`.

These paths are useful for health checks and troubleshooting.

## Dynamic configuration

Main dynamic options (`//sys/yql_agent/config` → `yql_agent`):

- `max_simultaneous_queries` (default: `63`): max concurrently running queries per agent.
- `state_check_period` (default: `15s`): YQL-agent state update period.
- `gateways`: dynamic YQL gateways config fragment.
- `default_yql_ui_version`: overrides the UI version exposed by YQL-agent.
- `allow_not_released_yql_versions`: allows/disallows unreleased YQL versions in UI/API.

Example:

```yson
{
    "yql_agent" = {
        "max_simultaneous_queries" = 64;
        "state_check_period" = "15s";
        "gateways" = {};
        "default_yql_ui_version" = "2025.03";
        "allow_not_released_yql_versions" = %false;
    };
}
```

## DQ: enabling and base setup

For DQ, use three main sections:

- `enable_dq`: enables DQ execution.
- `dq_gateway_config.default_settings`: DQ gateway settings.
- `dq_manager_config`: DQ backend/coordinator runtime settings (ports, backends, coordinator).

Minimal example:

```yson
{
    "yql_agent" = {
        "enable_dq" = %true;
        "dq_gateway_config" = {
            "default_settings" = [
                { "name" = "EnableComputeActor"; "value" = "1"; };
                { "name" = "MemoryLimit"; "value" = "3G"; };
            ];
        };
        "dq_manager_config" = {
            "interconnect_port" = 31002;
            "grpc_port" = 31001;
            "yt_backends" = [
                {
                    "cluster_name" = "hahn";
                    "token_file" = "/etc/yt/token";
                    "user" = "yql_agent";
                    "max_jobs" = 150;
                    "jobs_per_operation" = 5;
                };
            ];
            "yt_coordinator" = {
                "cluster_name" = "hahn";
                "prefix" = "//sys/yql_agent/dq_coord";
                "token_file" = "/etc/yt/token";
                "user" = "yql_agent";
            };
        };
    };
}
```

## DQ gateway: settings reference {#dq-settings-reference}

Settings in `dq_gateway_config.default_settings[]` control DQ query execution behavior. Each entry has the form `{ "name" = "<SettingName>"; "value" = "<value>"; }`.

The table below lists all user-facing settings. The **YQL-agent default** column shows the value applied by the YQL-agent plugin (from `yt/yql/plugin/config.cpp`). When a YQL-agent default is present it takes precedence over the built-in code default.

### Task limits

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `MaxTasksPerOperation` | uint32 | `100` | `70` | Maximum number of DQ tasks across all stages of a single operation. Operations that would exceed this limit fall back to the YT map-reduce engine (if fallback is allowed). |
| `MaxTasksPerStage` | uint32 | `30` | `20` | Maximum number of tasks per single DQ stage. |
| `WorkersPerOperation` | uint32 | — | — | Fixed number of workers to allocate for an operation. When unset, the number is computed automatically. |
| `ParallelOperationsLimit` | uint64 | — | `16` | Maximum number of DQ operations that may run in parallel within a single query. |
| `HashShuffleTasksRatio` | double | — | `0.5` | Fraction of available workers assigned to hash-shuffle stages. |
| `HashShuffleMaxTasks` | uint32 | — | `24` | Upper cap on hash-shuffle task count regardless of ratio. |

### Memory and data sizes

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `MemoryLimit` | size string | `3G` | — | Per-job memory limit for DQ compute actors (e.g. `3G`, `512M`). |
| `ChannelBufferSize` | uint64 (bytes) | `1000000` | `2147483648` (2 GB) | Size of in-memory channel buffers between compute actors. |
| `OutputChunkMaxSize` | uint64 (bytes) | — | `4194304` (4 MB) | Maximum size of a single output chunk written by a compute actor. |
| `ChunkSizeLimit` | uint64 (bytes) | — | `134217728` (128 MB) | Maximum allowed chunk size when reading from YT. Chunks larger than this value are split. |
| `DataSizePerJob` | uint64 (bytes) | — | `134217728` (128 MB) | Target input data size per DQ job when splitting input. |
| `MaxDataSizePerJob` | uint64 (bytes) | — | `629145600` (600 MB) | Hard upper limit on input data size per DQ job. |
| `MaxDataSizePerQuery` | uint64 (bytes) | — | — | Total input data size cap for a DQ query. Queries exceeding this fall back to YT engine. |

### Execution and fallback

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `EnableComputeActor` | `0`/`1` | `1` | — | Enable DQ compute actor execution. Must be `1` for DQ to work. |
| `ComputeActorType` | string | `async` | — | Compute actor implementation type. Accepted value: `async`. |
| `EnableStrip` | bool | `true` | — | Strip unused columns from input before passing to compute actors. |
| `EnableInsert` | bool | `true` | — | Enable DQ-side INSERT support. |
| `EnableFullResultWrite` | bool | `true` | — | Write full query results via DQ (instead of through coordinator). |
| `AnalyzeQuery` | bool | `true` | — | Analyze query complexity before execution; enables automatic DQ/fallback routing. |
| `FallbackPolicy` | string | — | — | Controls when to fall back to the YT engine. Accepted values: `never`, `always`, `condition`. |
| `UseFinalizeByKey` | bool | — | — | Use finalize-by-key aggregation when possible (reduces shuffle). |
| `UseAggPhases` | bool | `true` | — | Enable multi-phase aggregation (combine-then-reduce). |
| `EnableDqReplicate` | bool | — | `false` | Enable DQ replicate operator (required for some broadcast joins). |
| `SplitStageOnDqReplicate` | bool | — | `true` | Automatically split stages at replicate boundaries. |
| `UseSimpleYtReader` | bool | — | — | Use simpler YT reader instead of the block-optimized one. |
| `UseBlockReader` | bool | — | — | Use block (Arrow) reader for YT input. |
| `DisableLLVMForBlockStages` | bool | — | — | Disable LLVM codegen for block-processing stages. |
| `OptLLVM` | string | — | — | LLVM optimization level override (e.g. `O2`). |
| `UseGraceJoinCoreForMap` | bool | — | — | Use grace-join algorithm for map-side joins. |
| `HashJoinMode` | string | `off` | — | Hash join algorithm. Accepted values: `off`, `map`, `broadcast`, `grace`, `graceandself`. |

### Network and transport

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `PullRequestTimeoutMs` | uint64 (ms) | `3000000` | — | Timeout for pulling results from a compute actor (milliseconds). |
| `PingTimeoutMs` | uint64 (ms) | `30000` | — | Heartbeat ping timeout for compute actors (milliseconds). |
| `UseWideChannels` | bool | `true` | — | Use wide (multi-column) channels between compute actors for better throughput. |
| `UseWideBlockChannels` | bool | — | — | Use block-encoded wide channels. |
| `UseFastPickleTransport` | bool | `true` | `false` | Use fast pickle serialization for inter-actor data transport. |
| `UseOOBTransport` | bool | `true` | `false` | Use out-of-band transport for large data chunks between compute actors. |
| `MaxNetworkRetries` | int | — | `5` | Number of network-level retries before failing a task. |
| `MaxRetries` | int | — | — | Total retry limit for a failed DQ operation. |
| `RetryBackoffMs` | uint64 (ms) | — | — | Backoff delay between retries (milliseconds). |
| `Scheduler` | string | — | — | DQ task scheduler algorithm override. |

### Spilling (overflow to disk)

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `SpillingEngine` | string | — | `disable` | Spilling backend. Accepted values: `disable`, `file`. |
| `EnableSpillingNodes` | uint64 | — | `0` | Bitmask of node types allowed to spill (0 = none). |
| `EnableSpillingInChannels` | bool | — | `false` | Allow channel buffers to spill to disk when full. |
| `DisableCheckpoints` | bool | — | — | Disable DQ checkpointing (useful for debugging or when checkpoints are not needed). |

### Statistics and diagnostics

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `AggregateStatsByStage` | bool | — | `true` | Aggregate task statistics at the stage level before reporting. |
| `EnableChannelStats` | bool | — | `false` | Collect and report per-channel statistics. |
| `ExportStats` | bool | — | `false` | Export statistics to the YT cluster. |
| `TaskRunnerStats` | string | — | `basic` | Level of task-runner statistics. Accepted values: `disable`, `basic`, `full`, `profile`. |
| `CollectCoreDumps` | bool | — | — | Collect core dumps from failed compute actors for debugging. |
| `WorkerFilter` | string | — | — | Filter expression to select which DQ workers handle a query (for testing/routing). |

### Watermarks (streaming queries)

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `WatermarksMode` | string | — | — | Watermark mode for streaming execution (e.g. `default`). |
| `WatermarksEnableIdlePartitions` | bool | — | — | Advance watermarks for idle (no-data) partitions. |
| `WatermarksIdleTimeoutMs` | uint64 (ms) | — | `5000` | How long a partition must be idle before its watermark is advanced. |
| `WatermarksGranularityMs` | uint64 (ms) | — | `1000` | Watermark advancement granularity. |
| `WatermarksLateArrivalDelayMs` | uint64 (ms) | — | `5000` | Tolerance window for late-arriving data before it is discarded. |
| `AnalyticsHopping` | bool | — | — | Enable analytics hopping windows in streaming mode. |

### Data format and serialization

| Setting | Type | YQL-agent default | Code default | Description |
| --- | --- | --- | --- | --- |
| `ValuePackerVersion` | string | — | `v0` | Serialization format version for inter-actor values. Accepted values: `v0`, `v1`. |

### Example: tuning DQ for a large cluster

```yson
{
    "yql_agent" = {
        "enable_dq" = %true;
        "dq_gateway_config" = {
            "default_settings" = [
                { "name" = "EnableComputeActor"; "value" = "1"; };
                { "name" = "MemoryLimit"; "value" = "8G"; };
                { "name" = "MaxTasksPerOperation"; "value" = "200"; };
                { "name" = "MaxTasksPerStage"; "value" = "50"; };
                { "name" = "ChannelBufferSize"; "value" = "33554432"; };
                { "name" = "HashJoinMode"; "value" = "grace"; };
                { "name" = "ParallelOperationsLimit"; "value" = "32"; };
                { "name" = "FallbackPolicy"; "value" = "condition"; };
            ];
        };
    };
}
```

## YQL DQ architecture: manager, gateway, ports, coordinator

At runtime, DQ in YQL-agent is split into two roles:

- **`dq-manager`** (server/runtime side) starts and owns the DQ service node actor system, resource managers, and global worker logic.
- **`dq-gateway`** (query side) is a client used by YQL execution to send DQ requests to the local manager over gRPC.

High-level flow:

1. YQL-agent starts DQ manager from `dq_manager_config`.
2. DQ manager starts a service node and exposes gRPC + interconnect endpoints.
3. Query execution creates `dq-gateway` and connects to `localhost:<grpc_port>`.
4. DQ manager uses `yt_coordinator` to register/discover service nodes and coordinate workers in YT.

### Port usage

- `grpc_port`:
  - used by DQ service node gRPC API;
  - used by local `dq-gateway` (`localhost:<grpc_port>`);
  - published into coordinator metadata and consumed by dynamic node resolver as `host:grpc_port`.
- `interconnect_port`:
  - used by actor interconnect traffic between DQ nodes/workers;
  - published into coordinator metadata and used by service-node pinger/registration logic.
- `grpc_port` and `interconnect_port` must be non-zero and different.

### `yt_coordinator` responsibilities

`yt_coordinator` is the DQ control-plane binding to YT. It provides cluster/prefix/user/token context and is used to:

- assign and persist DQ node IDs;
- register/refresh service-node liveness metadata in Cypress;
- run cleanup/registration background actors;
- bootstrap global worker management and dynamic node discovery.

Without `yt_coordinator`, DQ manager startup fails.

### `actor_threads` and `interconnect_settings`

- `actor_threads` controls the number of actor runtime threads in DQ service node.
- `interconnect_settings` maps to `TDqConfig.TICSettings` and tunes interconnect behavior (timeouts, buffers, scheduler/thread-pool knobs, etc.).
- YQL-agent postprocessing auto-fills `interconnect_settings.close_on_idle_ms = 0` if it is not specified.
- The same interconnect settings are applied to the service node and propagated to `yt_backends[]` entries that do not define backend-specific IC settings.

## Multicluster configuration

Multicluster routing is configured in `gateway_config.cluster_mapping`: cluster names, proxy addresses, and per-cluster settings.

Base example:

```yson
{
    "yql_agent" = {
        "gateway_config" = {
            "cluster_mapping" = [
                {
                    "name" = "hahn";
                    "cluster" = "hahn.yt.your-domain:80";
                    "default" = %true;
                    "settings" = [
                        { "name" = "_AllowRemoteClusterInput"; "value" = "true"; };
                    ];
                };
                {
                    "name" = "arnold";
                    "cluster" = "arnold.yt.your-domain:80";
                    "default" = %false;
                };
            ];
        };
    };
}
```

Recommendations:

- Keep exactly one `default` cluster.
- Check `_AllowRemoteClusterInput` for cross-cluster reads.
- Use YQL pragmas for query-time execution placement and behavior (see [YT pragmas](../yql/syntax/pragma/yt.md)).

## Query Tracker integration

YQL queries submitted through Query Tracker are routed to YQL-agent by stage.

- Query Tracker uses `production` by default (`query_tracker.yql_engine.stage`).
- For a specific YQL query, stage can be set via `settings.stage` in `start_query`.
- The stage must exist in `//sys/@cluster_connection/yql_agent/stages`.

Example YQL-agent stage routing:

```yson
{
    "yql_agent" = {
        "stages" = {
            "production" = {
                "channel" = {
                    "addresses" = ["yql-agent-1:9013"; "yql-agent-2:9013"];
                };
            };
            "testing" = {
                "channel" = {
                    "addresses" = ["yql-agent-testing:9013"];
                };
            };
        };
    };
}
```

Also note `query_tracker_stage` in Query Tracker API: it selects a Query Tracker installation, not a YQL-agent stage.

For API details, see [Query Tracker](../user-guide/query-tracker/about.md).

## Caching

Query caching is controlled at two levels:

1. **YQL-agent defaults:** `gateway_config.default_settings` and per-cluster `cluster_mapping[].settings`.
2. **Per-query pragmas** in YQL text.

Main cache settings:

- `QueryCacheMode`
- `QueryCacheSalt`
- `QueryCacheChunkLimit`

For cache-mode semantics (`disable`/`readonly`/`refresh`/`normal`) and TTL, see [yt.QueryCacheMode](../yql/syntax/pragma/yt.md#querycache), [yt.QueryCacheSalt](../yql/syntax/pragma/yt.md#ytquerycachesalt), and [yt.QueryCacheTtl](../yql/syntax/pragma/yt.md#ytquerycachettl).

## Configuration field reference

This section summarizes configuration fields from:

- `yt/yql/plugin/config.h`
- `yt/yql/plugin/config.cpp`
- `contrib/ydb/core/protos/config.proto`
- `yql/essentials/providers/common/proto/gateways_config.proto`

### `yql_agent` (plugin config root)

Fields (`yt/yql/plugin/config.cpp`):

- `gateway` (alias: `gateway_config`) — `TYtGatewayConfig` structure serialized as YSON.
- `dq_gateway` (alias: `dq_gateway_config`) — `TDqGatewayConfig`-compatible map.
- `ytflow_gateway` (alias: `ytflow_gateway_config`) — `TYtflowGatewayConfig`-compatible map.
- `pq_gateway` (alias: `pq_gateway_config`) — `TPqGatewayConfig`-compatible map.
- `solomon_gateway` (alias: `solomon_gateway_config`) — `TSolomonGatewayConfig`-compatible map.
- `file_storage` (alias: `file_storage_config`) — `TFileStorageConfig`-compatible map.
- `tvm` (alias: `tvm_config`) — `TYtTvmConfig`-compatible map.
- `yt_access_provider` (alias: `yt_access_provider_config`) — `TYtAccessProviderConfig`-compatible map.
- `operation_attributes` — YSON map with operation attributes.
- `yt_token_path` — path to YT token file.
- `yql_plugin_shared_library` — path to `libyqlplugin.so`.
- `additional_system_libs` — list of additional shared libraries.
- `dq_manager` (alias: `dq_manager_config`) — DQ manager config.
- `enable_dq` — enable DQ execution.
- `libraries` — map of UDF/library aliases.
- `process_plugin_config` — process isolation settings.

Important defaults/automatic merges:

- `gateway.remote_file_patterns` gets default pattern for `yt://<cluster>/<path>`.
- `gateway.mr_job_bin` defaults to `./mrjob`.
- `gateway.yt_log_level` defaults to `YL_DEBUG`.
- `gateway.execute_udf_locally_if_possible` defaults to `false`.
- `file_storage.max_files = 8192`, `file_storage.max_size_mb = 16384`, `file_storage.retry_count = 3`.
- `gateway.default_settings`, `dq_gateway.default_settings`, `ytflow_gateway.default_settings`, `pq_gateway.default_settings`, `solomon_gateway.default_settings` are merged with built-in defaults.
- `dq_gateway.default_auto_percentage` is set to `100` by default.

### `additional_system_libs[]`

Each item:

- `file` — local path to shared library.

### `process_plugin_config`

- `enabled` (default `false`)
- `slot_count` (default `32`)
- `slots_root_path` (default `/yt/plugin_slots`)
- `check_process_active_delay` (default `1m`)
- `default_request_timeout` (default `1m`)
- `run_request_timeout` (default `7d`)
- `log_manager_template`

### `dq_manager_config`

- `interconnect_port`
- `grpc_port`
- `actor_threads` (default `4`)
- `use_ipv4` (default `false`)
- `address_resolver`
- `yt_backends[]`
- `yt_coordinator`
- `interconnect_settings` (defaults to empty map; `close_on_idle_ms` is auto-filled with `0` if absent)

#### `dq_manager_config.yt_backends[]`

- `cluster_name`
- `jobs_per_operation` (default `5`)
- `max_jobs` (default `150`)
- `vanilla_job_lite`
- `vanilla_job_command` (default `./dq_vanilla_job`)
- `vanilla_job_file[]` (`name`, `local_path`)
- `prefix` (default `//sys/yql_agent/dq/data`)
- `upload_replication_factor` (default `7`)
- `token_file`
- `user`
- `pool`
- `pool_trees[]`
- `owner[]` (default `["yql_agent"]`)
- `cpu_limit` (default `6`)
- `worker_capacity` (default `24`)
- `memory_limit` (default `64424509440`)
- `cache_size` (default `6000000000`)
- `use_tmp_fs` (default `true`)
- `network_project` (default empty string)
- `can_use_compute_actor` (default `true`)
- `enforce_job_utc` (default `true`)
- `use_local_l_d_library_path` (default `false`)
- `scheduling_tag_filter`

#### `dq_manager_config.yt_coordinator`

- `cluster_name`
- `prefix` (default `//sys/yql_agent/dq_coord`)
- `token_file`
- `user`
- `debug_log_file`

### `gateway_config` (`TYtGatewayConfig` + nested)

#### Common helper messages

- `TAttr`: `name`, `value`, `activation`
- `TRemoteFilePattern`: `pattern`, `cluster`, `path`
- `TFileWithMd5`: `file`, `md5`

#### `TYtClusterConfig`

- `name`
- `cluster`
- `default`
- `yt_token`
- `yt_name`
- `enabled_yt_ql_queries`
- `enabled_spyt_queries`
- `settings[]` (`TAttr`)

#### `TYtGatewayConfig`

- `gateway_threads` (default `0`)
- `yt_log_level` (default `YL_ERROR`)
- `mr_job_bin`
- `mr_job_bin_md5`
- `mr_job_udfs_dir`
- `execute_udf_locally_if_possible`
- `local_chain_test` (default `false`)
- `yt_debug_log_file`
- `yt_debug_log_size` (default `0`)
- `yt_debug_log_always_write` (default `false`)
- `local_chain_file`
- `mr_job_system_libs_with_md5[]` (`TFileWithMd5`)
- `remote_file_patterns[]` (`TRemoteFilePattern`)
- `cluster_mapping[]` (`TYtClusterConfig`)
- `default_settings[]` (`TAttr`)

### `dq_gateway_config` (`TDqGatewayConfig`)

- `default_auto_percentage` (default `0`)
- `default_auto_by_hour[]` (`hour`, `percentage`)
- `no_default_auto_for_users[]`
- `default_analyze_query_for_users[]`
- `default_settings[]` (`TAttr`)
- `with_hidden_percentage` (deprecated)
- `with_hidden_by_hour[]` (deprecated)
- `no_with_hidden_for_users[]` (deprecated)
- `with_hidden_for_users[]` (deprecated)
- `hidden_activation`

### `ytflow_gateway_config` (`TYtflowGatewayConfig`)

#### `TYtflowClusterConfig`

- `name`
- `real_name`
- `proxy_url`
- `token`
- `settings[]` (`TAttr`)

#### `TYtflowGatewayConfig`

- `gateway_threads` (default `1`)
- `ytflow_worker_bin`
- `cluster_mapping[]` (`TYtflowClusterConfig`)
- `default_settings[]` (`TAttr`)

### `pq_gateway_config` (`TPqGatewayConfig`)

#### `TPqClusterConfig`

- `name`
- `cluster_type`
- `endpoint`
- `config_manager_endpoint`
- `token`
- `database` (default `/Root`)
- `tvm_id` (default `0`)
- `use_ssl`
- `service_account_id`
- `service_account_id_signature`
- `add_bearer_to_token`
- `database_id`
- `settings[]` (`TAttr`)
- `shared_reading`
- `reconnect_period`
- `read_group`

#### `TPqGatewayConfig`

- `cluster_mapping[]` (`TPqClusterConfig`)
- `default_token`
- `default_settings[]` (`TAttr`)

### `solomon_gateway_config` (`TSolomonGatewayConfig`)

#### `TSolomonClusterConfig`

- `name`
- `cluster`
- `use_ssl`
- `cluster_type`
- `token`
- `service_account_id`
- `service_account_id_signature`
- `path` (`project`, `cluster`)
- `settings[]` (`TAttr`)

#### `TSolomonGatewayConfig`

- `cluster_mapping[]` (`TSolomonClusterConfig`)
- `default_settings[]` (`TAttr`)

### `TGatewaysConfig` root (for reference)

`TGatewaysConfig` includes root sections for all gateways; for YQL-agent-relevant parts use:

- `yt`
- `dq`
- `ytflow`
- `pq`
- `solomon`

`TGatewaysConfig` also includes adjacent global sections (`http_gateway`, `fs`, `yql_core`, etc.) that can be configured as needed.

### `contrib/ydb/core/protos/config.proto` (YQL section)

In `TQueryServiceConfig`, YQL-related fields are:

- `s3` (`NYql.TS3GatewayConfig`)
- `yt` (`NYql.TYtGatewayConfig`)
- `solomon` (`NYql.TSolomonGatewayConfig`)
- `file_storage` (`NYql.TFileStorageConfig`)
- `http_gateway` (`NYql.THttpGatewayConfig`)
- `generic` (`NYql.TGenericGatewayConfig`)

And related service-level toggles/limits:

- `script_operation_timeout_default_seconds`
- `script_forget_after_default_seconds`
- `script_results_ttl_default_seconds`
- `script_result_size_limit`
- `script_result_rows_limit`
- `hostname_patterns[]`
- `query_artifacts_compression_method`
- `query_artifacts_compression_min_size`
- `progress_stats_period_ms`
- `query_timeout_default_seconds`
- `enable_match_recognize`
- `available_external_data_sources[]`
- `all_external_data_sources_are_available`
- `streaming_queries`
