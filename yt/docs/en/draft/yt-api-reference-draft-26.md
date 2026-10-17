---
type: Draft Article
title: "Draft-26: YT API reference"
last_modified: 2026-10-17T00:00:00Z
tags: [api, http, rpc, sdk, compatibility]
status: draft
---

# YT API reference

This draft describes the command surface in the **current upstream `main` branch**, including every registered command name, command-specific and inherited parameters, API version differences, HTTP/RPC/native driver availability, named SDK helpers, and gaps in the existing command reference. The `pain` branch is based on upstream `main`; this article describes `main` behavior rather than promising the same behavior in every release.

{% note warning "Draft" %}

The main source snapshot is **4 October 2026**, commit `691b8066680325a6f7520755cbefc8cf475a4385`. Commands and defaults can change after this snapshot. A separate source comparison, checked on 5 October 2026, records command-registration history in `stable/24.1` through `stable/26.2`. Stable-branch parameter and transport differences remain follow-up work; the current main schemas are not a release compatibility promise.

{% endnote %}

The catalog includes internal commands because completeness requires recording them. Their registration is conditional on `enable_internal_commands`; registration does not establish authorization or availability on a deployed cluster. SDK convenience functions, CLI switches, and RPCs belonging to master, tablet, job-proxy, or other internal services are separate interfaces. The RPC appendix covers the public proxy `ApiService` wire methods and the separate proxy DiscoveryService.

## How to read the tags

Table 1 describes the availability and behavior tags used in this reference.

<small>Table 1 — Availability and behavior tags</small>

| Tag | Meaning |
| --- | --- |
| `missing-current-docs` | No section for this exact command spelling exists in upstream main’s English API command reference at the pinned revision. A related feature or another alias may still be documented elsewhere. |
| `no-http-api` | No equivalent command is registered in the HTTP proxy’s driver. Used for RPC-only entries; composition through other HTTP commands does not supply that named entry. |
| `no-rpc-api` | The RPC backend cannot execute this command: an explicit client stub, native-only internal interface, or missing server handler blocks it. The entry states the reason. |
| `no-native-api` | No equivalent command is registered in the native driver. This is a driver-command gap; core native C++ client methods may still expose the underlying behavior. |
| `no-go-sdk` | No named public command helper or documented equivalent in the Go SDK scope below. |
| `no-java-sdk` | No named public command helper or documented equivalent in the Java SDK scope below. |
| `no-cpp-sdk` | No named public command helper or documented equivalent in the documented C++ `NYT::IClient` SDK and its mixins. Core `NApi` clients are a separate surface. |
| `no-python-sdk` | No named public command helper or documented equivalent re-exported by `yt.wrapper`. Generic driver requests remain separate. |
| `v3-only` | This command spelling is registered only in driver API v3. |
| `v4-only` | This command spelling is registered only in driver API v4. |
| `since-X.Y` | First inspected stable branch registering this exact command spelling, after the 24.1 baseline. For RPC appendix methods, first public proxy declaration. See the pinned stable-source comparison; this does not date current parameters or implementation support. |
| `versioned-response` | The registered output data type differs between v3 and v4. |
| `driver-local` | The driver performs the operation itself; no corresponding public ApiService wire method exists. |
| `internal` | The driver registers this command only when internal commands are enabled. |
| `compat-alias` | A legacy compatibility spelling shares an implementation with its replacement. |
| `parameter-alias` | At least one parameter has an accepted alternate spelling. |
| `heavy` | The registry marks the command as heavy. This flag is reported exactly; it does not imply that all other commands have no streams. |
| `volatile` | The registry marks the command as volatile. This is the descriptor flag, not a complete guarantee about side effects, retryability, or idempotence. |
| `rpc-wire` | A public proxy RPC method. Its protobuf fields are distinct from HTTP/YSON command parameters. |
| `rpc-client-only` | The client declares the wire method, but the inspected server does not register or implement it; also tagged `no-rpc-api`. |
| `discovery-service` | RPC discovery belongs to the separate DiscoveryService interface. |
| `rpc-review-needed` or `server-review-needed` | A support mapping needs further evidence. These tags express uncertainty and do not mean unimplemented. |

Version tags describe the HTTP/driver API versions. Registered HTTP commands apply to both v3 and v4 unless marked `v3-only` or `v4-only`. The RPC proxy protocol has its own major and minor version; it is not HTTP API v3 or v4. RPC-only methods therefore receive no v3/v4 tag.

## API and SDK coverage

API tags describe the implementation at the pinned main commit. The HTTP proxy creates v3/v4 drivers over its native connection; the native and RPC driver bindings use the same command registry over different connection types. All 213 registered spellings have HTTP/native implementations in the inspected source, including conditional internal registrations. Internal-command configuration and authorization can restrict deployed availability. The six RPC-only methods without registered driver equivalents are tagged `no-http-api` and `no-native-api` in the RPC appendix.

The RPC client has explicit unavailable operations, and one declared method lacks its server handler. These receive `no-rpc-api`. Driver-local helpers remain callable in the RPC driver without a dedicated remote method, so absence of a one-to-one wire method alone does not trigger this tag. For example, `execute_batch` composes subrequests and `parse_ypath` runs locally.

SDK tags describe named public command entry points, including equivalent names and implemented wrappers. They do not date SDK introduction, establish every parameter’s support, or promise that every SDK backend supports the call. A typed helper can exist while its RPC backend is unavailable. Conversely, generic request construction can reach a command even when its named SDK helper is missing. Legacy command aliases share an equivalent helper where available. Special stream transformations such as `read_blob_table` and `flow_execute_plaintext` require a corresponding helper; a general table or Flow call does not supply that helper by itself.

The C++ SDK scope is the documented `NYT::IClient`, transaction interfaces, Cypress/IO/operation mixins, and batch helpers under `yt/cpp/mapreduce/interface`. This is distinct from core `NApi::IClient`/`IInternalClient`, whose native implementation is used to assess native-driver availability. Python scope is the public `yt.wrapper` re-exports and their functions/classes; `TablePath` provides rich-path parsing, operation helpers use `run_*`, and the public job-shell helper provides polling. Python’s package `get_version()` reports SDK package metadata and is not the driver’s `get_version` command.

Table 2 lists the sources for the public API and SDK entry points.

<small>Table 2 — Public API and SDK sources</small>

| Surface | Entry-point sources |
| --- | --- |
| HTTP proxy | [native-backed driver construction](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/yt/server/http_proxy/bootstrap.cpp#L265), [versioned registry lookup](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/yt/server/http_proxy/context.cpp#L269) |
| Native driver | [native binding construction](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/yt/python/driver/native/driver.cpp#L129), [core native client](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/yt/ytlib/api/native/client_impl.h#L1) |
| RPC driver | [RPC binding construction](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/yt/python/driver/rpc/driver_rpc.cpp#L75) |
| Go SDK | [public interfaces](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/go/yt/interface.go#L1) |
| Java SDK | [ApiServiceClient](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/java/ytsaurus-client/src/main/java/tech/ytsaurus/client/ApiServiceClient.java#L1), [TransactionalClient](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/java/ytsaurus-client/src/main/java/tech/ytsaurus/client/TransactionalClient.java#L1), [CompoundClient](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/java/ytsaurus-client/src/main/java/tech/ytsaurus/client/CompoundClient.java#L1), [ApiServiceTransaction](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/java/ytsaurus-client/src/main/java/tech/ytsaurus/client/ApiServiceTransaction.java#L1) |
| C++ SDK | [client](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/cpp/mapreduce/interface/client.h#L1), [Cypress](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/cpp/mapreduce/interface/cypress.h#L1), [IO](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/cpp/mapreduce/interface/io.h#L1), [operations](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/cpp/mapreduce/interface/operation.h#L1), [batch helpers](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/cpp/mapreduce/interface/batch_request.h#L1) |
| Python SDK | [public namespace](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/python/yt/wrapper/__init__.py#L1), [client API exports](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/python/yt/wrapper/client_api.py#L1), [batch API exports](https://github.com/ytsaurus/ytsaurus/blob/691b8066680325a6f7520755cbefc8cf475a4385/yt/python/yt/wrapper/batch_api.py#L1) |

### RPC methods without driver command entries

These wire methods have no registered HTTP/native driver command equivalent. Their request schemas and SDK entry-point evidence are in the complete article’s RPC appendix.

Table 3 lists these methods and their availability tags.

<small>Table 3 — RPC methods without driver commands</small>

| Wire method | Tags |
| --- | --- |
| `AttachTransaction` | `rpc-wire`, `no-http-api`, `no-native-api`, `missing-current-docs` |
| `BatchModifyRows` | `rpc-wire`, `no-http-api`, `no-native-api`, `missing-current-docs`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` |
| `DetachTransaction` | `rpc-wire`, `no-http-api`, `no-native-api`, `missing-current-docs`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` |
| `FlushTransaction` | `rpc-wire`, `no-http-api`, `no-native-api`, `missing-current-docs`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` |
| `GCCollect` | `rpc-wire`, `no-http-api`, `no-native-api`, `missing-current-docs`, `no-go-sdk`, `no-cpp-sdk`, `no-python-sdk` |
| `MultiLookup` | `rpc-wire`, `no-http-api`, `no-native-api`, `missing-current-docs`, `no-cpp-sdk`, `no-python-sdk` |

## Request and response conventions

HTTP requests use `/api/v3/<command>` or `/api/v4/<command>`. The proxy resolves the command in that version’s driver registry. `/api`, `/api/v3`, and `/api/v4` provide discovery responses; inspect the deployed registry before relying on a command in a released cluster.

A command has a YSON parameter map and independent input/output streams. Stream types are `null`, `binary`, `structured`, and `tabular`. Structured or tabular streams use the appropriate `input_format` and `output_format`; these are read by the driver context rather than registered separately in each command. HTTP’s format handling and defaults depend on the proxy; native driver calls must supply the format when the command accesses it. Binary payloads are raw bytes.

HTTP headers such as authentication, parameter encoding, compression, tracing, and response-format headers belong to the transport envelope. They are not interchangeable with parameters in the tables below. See the [HTTP reference](../user-guide/proxy/http-reference.md).

`ProduceEmptyOutput` writes no value in v3 and an empty map in v4. `ProduceSingleOutput` returns its value directly in v3 and wraps it under a named map key in v4. Commands with custom output producers can return different structures; the per-command source identifies them. A v4-only command can still have `null` output, so v4 does not universally imply an empty-map response.

YSON durations are expressed in milliseconds. The RPC protobuf specifies duration values in microseconds, and instants in microseconds since the Unix epoch. YT timestamps are logical timestamp values, not these duration or instant encodings. GUIDs use YT’s GUID representation. Enum names in YSON use the enum’s snake-case spelling; the appendix records the implementation constants and values.

“Required” means the parameter registration supplies no default. “Unset”, “null”, and an explicit numeric or boolean default are distinct. An unset optional timeout delegates to connection or server behavior; it does not establish a fixed lease or RPC deadline. Some nested defaults refer to C++ constants or default-constructed types; the exact expression is preserved instead of replacing it with a guessed number. Configuration maps can overlay a driver configuration, making effective defaults deployment dependent.

## Transactions and retry behavior

Shared parameters appear in each applicable command’s table; do not assume all commands accept all transaction, timeout, or mutation options. Applicability follows the command’s option inheritance. Starting a transaction treats `transaction_id` as a parent ID. Tablet writes use an attached transaction when specified and can create and commit their own transaction otherwise. A sticky transaction must remain accessible through the driver or proxy that owns it.

`mutation_id` and `retry` support mutation deduplication where the implementation honors them. They do not make arbitrary heavy streams or multi-step operations safely retryable. Reusing an operation’s mutation ID, preserving transaction context, and resuming a stream are separate concerns. Consult the command’s transaction and reader/writer behavior before retrying a partial write.

Unknown YSON keys may be retained by the command base. Retention alone does not mean that a key is a supported parameter. The catalog lists registrations and recognized structured schemas. Arbitrary object attributes, table column names, engine settings, flow function arguments, and application annotations are open-ended data; there is no finite universal list of their keys.

## Command index

Commands apply to both v3 and v4 unless marked `v3-only` or `v4-only`. A single input/output pair applies to all supported versions. Version labels appear only when the stream types differ.

Table 4 lists the registered command spellings, stream types, tags, and RPC mappings.

<small>Table 4 — Command index</small>

| Command | Input → output | Tags | RPC mapping |
| --- | --- | --- | --- |
| `abandon_job` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `AbandonJob` |
| `abort_job` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-go-sdk`, `no-cpp-sdk` | `AbortJob` |
| `abort_op` | null → null | `v3-only`, `missing-current-docs`, `volatile` | `AbortOperation` |
| `abort_operation` | null → structured | `v4-only`, `volatile` | `AbortOperation` |
| `abort_query` | null → structured | `v4-only`, `volatile`, `no-cpp-sdk` | `AbortQuery` |
| `abort_transaction` | null → structured | `v4-only`, `volatile` | `AbortTransaction` |
| `abort_tx` | null → null | `v3-only`, `volatile` | `AbortTransaction` |
| `add_maintenance` | null → structured | `v4-only`, `missing-current-docs`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `AddMaintenance` |
| `add_member` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-java-sdk`, `no-cpp-sdk` | `AddMember` |
| `advance_consumer` | null → structured | `v4-only`, `missing-current-docs`, `compat-alias`, `volatile`, `no-cpp-sdk` | `AdvanceConsumer` |
| `advance_queue_consumer` | null → structured | `v4-only`, `volatile`, `no-cpp-sdk` | `AdvanceQueueConsumer` |
| `alter_query` | null → tabular | `v4-only`, `volatile`, `no-cpp-sdk` | `AlterQuery` |
| `alter_replication_card` | null → structured | `v4-only`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `AlterReplicationCard` |
| `alter_table` | v3: null → null; v4: null → structured | `missing-current-docs`, `volatile`, `versioned-response` | `AlterTable` |
| `alter_table_replica` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `AlterTableReplica` |
| `balance_tablet_cells` | null → structured | `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `BalanceTabletCells` |
| `build_master_snapshots` | null → structured | `missing-current-docs`, `no-rpc-api`, `volatile`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `build_snapshot` | null → structured | `missing-current-docs`, `volatile`, `no-cpp-sdk`, `no-python-sdk` | `BuildSnapshot` |
| `cancel_tablet_transition` | null → structured | `v4-only`, `since-25.1`, `no-rpc-api`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `check_cluster_liveness` | null → structured | `since-26.2`, `missing-current-docs`, `no-go-sdk` | `CheckClusterLiveness` |
| `check_operation_permission` | null → structured | `since-25.2`, `no-java-sdk`, `no-cpp-sdk` | `CheckOperationPermission` |
| `check_permission` | null → structured | | `CheckPermission` |
| `check_permission_by_acl` | null → structured | `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `CheckPermissionByAcl` |
| `commit_transaction` | null → structured | `v4-only`, `volatile` | `CommitTransaction` |
| `commit_tx` | null → null | `v3-only`, `volatile` | `CommitTransaction` |
| `complete_op` | null → null | `v3-only`, `missing-current-docs`, `volatile` | `CompleteOperation` |
| `complete_operation` | null → structured | `v4-only`, `volatile` | `CompleteOperation` |
| `concatenate` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-go-sdk` | `ConcatenateNodes` |
| `copy` | null → structured | `volatile` | `CopyNode` |
| `create` | null → structured | `volatile` | `CreateNode`, `CreateObject` |
| `create_queue_producer_session` | null → structured | `v4-only`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `CreateQueueProducerSession` |
| `create_table_backup` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-java-sdk`, `no-cpp-sdk` | `CreateTableBackup` |
| `delete_rows` | v3: tabular → null; v4: tabular → structured | `heavy`, `volatile`, `versioned-response` | `ModifyRows` |
| `destroy_chunk_locations` | null → structured | `missing-current-docs`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `DestroyChunkLocations` |
| `disable_chunk_locations` | null → structured | `missing-current-docs`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `DisableChunkLocations` |
| `disable_table_replica` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `AlterTableReplica` |
| `discombobulate_nonvoting_peers` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `DiscombobulateNonvotingPeers` |
| `discover_proxies` | null → structured | `v4-only`, `discovery-service`, `parameter-alias`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `DiscoverProxies` |
| `dump_job_context` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `DumpJobContext` |
| `dump_job_proxy_log` | null → structured | `v4-only`, `since-24.2`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `DumpJobProxyLog` |
| `enable_table_replica` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `AlterTableReplica` |
| `erase` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `execute_batch` | null → structured | `driver-local`, `volatile`, `no-java-sdk` | driver local |
| `exists` | null → structured | | `ExistsNode` |
| `exit_read_only` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ExitReadOnly` |
| `explain_query` | null → structured | `heavy`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `ExplainQuery` |
| `externalize` | null → null | `v4-only`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `ExternalizeNode` |
| `finish_distributed_write_file_session` | null → null | `v4-only`, `since-25.3`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `FinishDistributedWriteFileSession` |
| `finish_distributed_write_session` | null → null | `v4-only`, `since-24.2`, `volatile` | `FinishDistributedWriteSession` |
| `flow_execute` | structured → structured | `v4-only`, `since-25.1`, `heavy`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `FlowExecute` |
| `flow_execute_plaintext` | null → binary | `v4-only`, `since-26.1`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `FlowExecute` |
| `forsake_chaos_coordinator` | null → null | `since-25.1`, `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `forsake_chaos_shortcut` | null → null | `since-25.4`, `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `freeze_table` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `FreezeTable` |
| `generate_timestamp` | null → structured | | `GenerateTimestamps` |
| `get` | null → structured | | `GetNode` |
| `get_bundle_config` | null → structured | `no-rpc-api`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `get_connection_config` | null → structured | `missing-current-docs`, `driver-local`, `internal`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | driver local |
| `get_connection_orchid_value` | null → structured | `since-25.4`, `missing-current-docs`, `no-rpc-api`, `internal`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `get_current_user` | null → structured | `since-25.1` | `GetCurrentUser` |
| `get_file_from_cache` | null → structured | | `GetFileFromCache` |
| `get_flow_view` | null → structured | `v4-only`, `no-go-sdk`, `no-cpp-sdk` | `GetFlowView` |
| `get_in_sync_replicas` | tabular → structured | `heavy`, `no-cpp-sdk` | `GetInSyncReplicas` |
| `get_job` | null → structured | | `GetJob` |
| `get_job_fail_context` | null → binary | `heavy`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `GetJobFailContext` |
| `get_job_input` | null → binary | `heavy`, `no-go-sdk`, `no-java-sdk` | `GetJobInput` |
| `get_job_input_paths` | null → structured | `heavy`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `GetJobInputPaths` |
| `get_job_spec` | null → structured | `heavy`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `GetJobSpec` |
| `get_job_stderr` | null → binary | `heavy` | `GetJobStderr` |
| `get_job_trace` | null → binary | `since-24.2`, `heavy`, `no-go-sdk`, `no-java-sdk` | `GetJobTrace` |
| `get_master_consistent_state` | null → structured | `missing-current-docs`, `no-rpc-api`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `get_operation` | null → structured | `parameter-alias` | `GetOperation` |
| `get_ordered_tablet_safe_trim_row_count` | null → structured | `since-25.3`, `missing-current-docs`, `no-rpc-api`, `internal`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `get_pipeline_dynamic_spec` | null → structured | `v4-only`, `no-go-sdk`, `no-cpp-sdk` | `GetPipelineDynamicSpec` |
| `get_pipeline_spec` | null → structured | `v4-only`, `no-go-sdk`, `no-cpp-sdk` | `GetPipelineSpec` |
| `get_pipeline_state` | null → structured | `v4-only`, `no-go-sdk`, `no-cpp-sdk` | `GetPipelineState` |
| `get_query` | null → structured | `v4-only`, `no-cpp-sdk` | `GetQuery` |
| `get_query_declared_parameters_info` | null → structured | `v4-only`, `since-25.1`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `GetQueryDeclaredParametersInfo` |
| `get_query_result` | null → structured | `v4-only`, `no-cpp-sdk` | `GetQueryResult` |
| `get_query_tracker_info` | null → structured | `v4-only`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `GetQueryTrackerInfo` |
| `get_supported_features` | null → structured | `no-rpc-api`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | unavailable |
| `get_table_columnar_statistics` | null → structured | `heavy`, `no-go-sdk`, `no-java-sdk` | `GetColumnarStatistics` |
| `get_table_mount_info` | null → structured | `v4-only`, `since-24.2`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `GetTableMountInfo` |
| `get_table_pivot_keys` | null → structured | `v4-only`, `no-go-sdk`, `no-cpp-sdk`, `no-python-sdk` | `GetTablePivotKeys` |
| `get_tablet_errors` | null → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `GetTabletErrors` |
| `get_tablet_infos` | null → structured | `v4-only`, `volatile`, `no-go-sdk` | `GetTabletInfos` |
| `get_version` | null → structured | `driver-local`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | driver local |
| `heal_exec_node` | null → structured | `missing-current-docs`, `no-rpc-api`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `insert_rows` | v3: tabular → null; v4: tabular → structured | `heavy`, `volatile`, `versioned-response` | `ModifyRows` |
| `internalize` | null → null | `v4-only`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `InternalizeNode` |
| `issue_lease` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `issue_token` | null → structured | `no-rpc-api`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | unavailable |
| `join_reduce` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `link` | null → structured | `volatile` | `LinkNode` |
| `list` | null → structured | | `ListNode` |
| `list_job_traces` | null → structured | `since-25.3`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `ListJobTraces` |
| `list_jobs` | null → structured | `parameter-alias` | `ListJobs` |
| `list_operation_events` | null → structured | `since-25.2`, `no-java-sdk`, `no-cpp-sdk` | `ListOperationEvents` |
| `list_operations` | null → structured | `no-java-sdk` | `ListOperations` |
| `list_queries` | null → structured | `v4-only`, `no-cpp-sdk` | `ListQueries` |
| `list_queue_consumer_registrations` | null → structured | `v4-only`, `no-go-sdk`, `no-cpp-sdk` | `ListQueueConsumerRegistrations` |
| `list_user_tokens` | null → structured | `no-rpc-api`, `no-java-sdk`, `no-cpp-sdk` | unavailable |
| `locate_skynet_share` | null → structured | `no-rpc-api`, `heavy`, `no-java-sdk`, `no-python-sdk` | unavailable |
| `lock` | null → structured | `volatile` | `LockNode` |
| `lock_hunk_store` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `lock_rows` | v3: tabular → null; v4: tabular → structured | `heavy`, `volatile`, `versioned-response`, `no-java-sdk`, `no-cpp-sdk` | `ModifyRows` |
| `lookup_rows` | tabular → tabular | `heavy` | `LookupRows`, `VersionedLookupRows` |
| `map` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `map_reduce` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `master_exit_read_only` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `MasterExitReadOnly` |
| `merge` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `migrate_replication_cards` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `MigrateReplicationCards` |
| `mount_table` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `MountTable` |
| `move` | null → structured | `volatile` | `MoveNode` |
| `multiset_attributes` | structured → structured | `v4-only`, `volatile`, `no-java-sdk`, `no-python-sdk` | `MultisetAttributesNode` |
| `parse_ypath` | null → structured | `driver-local`, `no-go-sdk`, `no-java-sdk` | driver local |
| `partition_file` | null → structured | `since-26.2`, `missing-current-docs`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `PartitionFile` |
| `partition_tables` | null → structured | | `PartitionTables` |
| `patch_op_spec` | null → null | `v3-only`, `since-25.1`, `missing-current-docs`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `PatchOperationSpec` |
| `patch_operation_spec` | null → structured | `v4-only`, `since-25.1`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `PatchOperationSpec` |
| `pause_pipeline` | null → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `PausePipeline` |
| `ping_chaos_lease` | null → structured | `v4-only`, `since-25.3`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `PingChaosLease` |
| `ping_distributed_write_file_session` | null → null | `v4-only`, `since-25.3`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `PingDistributedWriteFileSession` |
| `ping_distributed_write_session` | null → null | `v4-only`, `since-25.2`, `volatile` | `PingDistributedWriteSession` |
| `ping_transaction` | null → structured | `v4-only`, `volatile` | `PingTransaction` |
| `ping_tx` | null → null | `v3-only`, `volatile` | `PingTransaction` |
| `poll_job_shell` | null → structured | `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `PollJobShell` |
| `pull_consumer` | null → tabular | `v4-only`, `missing-current-docs`, `compat-alias`, `heavy`, `no-cpp-sdk` | `PullConsumer` |
| `pull_queue` | null → tabular | `v4-only`, `heavy`, `no-go-sdk`, `no-cpp-sdk` | `PullQueue` |
| `pull_queue_consumer` | null → tabular | `v4-only`, `heavy`, `no-cpp-sdk` | `PullQueueConsumer` |
| `pull_rows` | null → tabular | `heavy`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `PullRows` |
| `push_queue_producer` | tabular → structured | `v4-only`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `PushQueueProducer` |
| `put_file_to_cache` | null → structured | `volatile` | `PutFileToCache` |
| `read_blob_table` | null → binary | `heavy`, `no-go-sdk`, `no-java-sdk` | `ReadTable` |
| `read_file` | null → binary | `heavy`, `parameter-alias` | `ReadFile` |
| `read_file_partition` | null → binary | `v4-only`, `since-26.2`, `missing-current-docs`, `heavy`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `ReadFilePartition` |
| `read_hunks` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `read_journal` | null → tabular | `heavy`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ReadJournal` |
| `read_query_result` | null → tabular | `v4-only`, `heavy`, `no-cpp-sdk` | `ReadQueryResult` |
| `read_shuffle_data` | null → tabular | `v4-only`, `since-25.1`, `heavy`, `no-go-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ReadShuffleData` |
| `read_table` | null → tabular | `heavy` | `ReadTable` |
| `read_table_partition` | null → tabular | `v4-only`, `since-25.1`, `heavy` | `ReadTablePartition` |
| `reduce` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `reference_lease` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `register_queue_consumer` | null → structured | `v4-only`, `volatile`, `no-cpp-sdk` | `RegisterQueueConsumer` |
| `remote_copy` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `remount_table` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `RemountTable` |
| `remove` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `RemoveNode` |
| `remove_chaos_cell_mailbox` | null → null | `since-25.4`, `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `remove_maintenance` | null → structured | `v4-only`, `missing-current-docs`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `RemoveMaintenance` |
| `remove_member` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-java-sdk`, `no-cpp-sdk` | `RemoveMember` |
| `remove_pipeline_dynamic_spec` | null → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `SetPipelineDynamicSpec` |
| `remove_pipeline_spec` | null → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `SetPipelineSpec` |
| `remove_queue_producer_session` | null → structured | `v4-only`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `RemoveQueueProducerSession` |
| `request_restart` | null → structured | `missing-current-docs`, `volatile`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `RequestRestart` |
| `reset_dynamically_propagated_master_cells` | null → structured | `since-25.4`, `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ResetDynamicallyPropagatedMasterCells` |
| `reset_state_hash` | null → structured | `missing-current-docs`, `no-rpc-api`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `reshard_table` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `ReshardTable` |
| `reshard_table_automatic` | null → structured | `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `ReshardTableAutomatic` |
| `restore_table_backup` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-java-sdk`, `no-cpp-sdk` | `RestoreTableBackup` |
| `resume_chaos_cells` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ResumeChaosCells` |
| `resume_coordinator` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ResumeCoordinator` |
| `resume_op` | null → null | `v3-only`, `missing-current-docs`, `volatile` | `ResumeOperation` |
| `resume_operation` | null → structured | `v4-only`, `volatile` | `ResumeOperation` |
| `resume_tablet_cells` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `ResumeTabletCells` |
| `resurrect_chunk_locations` | null → structured | `missing-current-docs`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `ResurrectChunkLocations` |
| `revoke_lease` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `revoke_token` | null → structured | `no-rpc-api`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | unavailable |
| `run_job_shell_command` | null → binary | `since-25.3`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk` | `RunJobShellCommand` |
| `select_rows` | null → tabular | `heavy` | `SelectRows` |
| `set` | v3: structured → null; v4: structured → structured | `volatile`, `versioned-response` | `SetNode` |
| `set_bundle_config` | structured → null | `no-rpc-api`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `set_pipeline_dynamic_spec` | structured → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `GetPipelineDynamicSpec`, `SetPipelineDynamicSpec` |
| `set_pipeline_spec` | structured → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `GetPipelineSpec`, `SetPipelineSpec` |
| `set_user_password` | null → structured | `no-rpc-api`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | unavailable |
| `sort` | null → structured | `v3-only`, `volatile` | `StartOperation` |
| `start_distributed_write_file_session` | null → structured | `v4-only`, `since-25.3`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `StartDistributedWriteFileSession` |
| `start_distributed_write_session` | null → structured | `v4-only`, `since-24.2`, `volatile` | `StartDistributedWriteSession` |
| `start_op` | null → structured | `v3-only`, `missing-current-docs`, `volatile`, `no-cpp-sdk` | `StartOperation` |
| `start_operation` | null → structured | `v4-only`, `volatile`, `no-cpp-sdk` | `StartOperation` |
| `start_pipeline` | null → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `StartPipeline` |
| `start_query` | null → structured | `v4-only`, `volatile`, `no-cpp-sdk` | `StartQuery` |
| `start_shuffle` | null → structured | `v4-only`, `since-25.1`, `volatile`, `no-go-sdk`, `no-cpp-sdk`, `no-python-sdk` | `StartShuffle` |
| `start_transaction` | null → structured | `v4-only`, `volatile` | `StartTransaction` |
| `start_tx` | null → structured | `v3-only`, `volatile` | `StartTransaction` |
| `stop_pipeline` | null → structured | `v4-only`, `volatile`, `no-go-sdk`, `no-cpp-sdk` | `StopPipeline` |
| `suspend_chaos_cells` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `SuspendChaosCells` |
| `suspend_coordinator` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `SuspendCoordinator` |
| `suspend_op` | null → null | `v3-only`, `missing-current-docs`, `volatile` | `SuspendOperation` |
| `suspend_operation` | null → structured | `v4-only`, `volatile` | `SuspendOperation` |
| `suspend_tablet_cells` | null → structured | `missing-current-docs`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `SuspendTabletCells` |
| `switch_leader` | null → structured | `missing-current-docs`, `no-rpc-api`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `transfer_account_resources` | null → structured | `v4-only`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `TransferAccountResources` |
| `transfer_bundle_resources` | null → structured | `v4-only`, `since-25.4`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `TransferBundleResources` |
| `transfer_pool_resources` | null → structured | `v4-only`, `no-rpc-api`, `rpc-client-only`, `volatile`, `no-java-sdk`, `no-cpp-sdk` | `TransferPoolResources` |
| `trim_rows` | v3: null → null; v4: null → structured | `heavy`, `volatile`, `versioned-response`, `no-go-sdk` | `TrimTable` |
| `truncate_journal` | null → null | `v4-only`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `TruncateJournal` |
| `unfreeze_table` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `UnfreezeTable` |
| `unlock` | v3: null → null; v4: null → structured | `volatile`, `versioned-response`, `no-java-sdk` | `UnlockNode` |
| `unlock_hunk_store` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `unmount_table` | v3: null → null; v4: null → structured | `volatile`, `versioned-response` | `UnmountTable` |
| `unreference_lease` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `unregister_queue_consumer` | null → structured | `v4-only`, `volatile`, `no-cpp-sdk` | `UnregisterQueueConsumer` |
| `update_op_parameters` | null → null | `v3-only`, `missing-current-docs`, `volatile` | `UpdateOperationParameters` |
| `update_operation_parameters` | null → structured | `v4-only`, `volatile` | `UpdateOperationParameters` |
| `update_replication_progress` | null → structured | `v4-only`, `no-rpc-api`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `write_file` | v3: binary → null; v4: binary → structured | `heavy`, `volatile`, `versioned-response` | `WriteFile` |
| `write_file_fragment` | binary → structured | `v4-only`, `since-25.3`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-python-sdk` | `WriteFileFragment` |
| `write_hunks` | null → structured | `missing-current-docs`, `no-rpc-api`, `internal`, `heavy`, `volatile`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | unavailable |
| `write_journal` | v3: tabular → null; v4: tabular → structured | `heavy`, `volatile`, `versioned-response`, `no-go-sdk`, `no-java-sdk`, `no-cpp-sdk`, `no-python-sdk` | `WriteJournal` |
| `write_shuffle_data` | tabular → structured | `v4-only`, `since-25.1`, `heavy`, `no-go-sdk`, `no-cpp-sdk`, `no-python-sdk` | `WriteShuffleData` |
| `write_table` | v3: tabular → null; v4: tabular → structured | `heavy`, `volatile`, `versioned-response` | `WriteTable` |
| `write_table_fragment` | tabular → structured | `v4-only`, `since-25.1`, `heavy`, `volatile` | `WriteTableFragment` |

## Stable branch command history

The stable-source comparison was checked on **5 October 2026**. It covers every numeric `stable/X.Y` branch found from `stable/24.1` through `stable/26.2`, at the exact commits below. The article’s command semantics, parameters, and API/SDK availability tags still describe the pinned **main** snapshot.

`since-X.Y` identifies the first inspected stable branch whose pinned source registers that exact command spelling. Commands already registered in the `stable/24.1` baseline have no `since` tag: this comparison does not determine their earlier introduction. Branch tips include maintenance backports, so these tags describe the inspected branch snapshots rather than the original release dates or the first development commit. Registration can be conditional on internal-command configuration.

For the RPC method appendix, the same tag identifies the first inspected public proxy **declaration**, separately from the HTTP spelling. A declaration does not establish that the RPC implementation worked in that release. For example, `GetTableMountInfo` and `CheckClusterLiveness` RPC declarations are present in the 24.1 baseline, while the named HTTP commands first appear in 24.2 and 26.2 respectively. The since tags do not promise that current parameter schemas, stream behavior, or transports existed unchanged from that branch onward.

### Pinned stable sources

Table 5 records the exact stable-branch snapshots used for the comparison.

<small>Table 5 — Pinned stable sources</small>

| Branch | Commit | Registered command spellings | ApiService method declarations | Source |
| --- | --- | --- | --- | --- |
| `stable/24.1` | [dd4bd8bdc4d2](https://github.com/ytsaurus/ytsaurus/commit/dd4bd8bdc4d28993e7185b820930761540b29ae9) | 176 | 134 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/dd4bd8bdc4d28993e7185b820930761540b29ae9/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/dd4bd8bdc4d28993e7185b820930761540b29ae9/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/dd4bd8bdc4d28993e7185b820930761540b29ae9/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/24.2` | [7368708d848c](https://github.com/ytsaurus/ytsaurus/commit/7368708d848c6a4056e18b2824bbe0fe328cfeaa) | 182 | 139 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/25.1` | [4847819ed7ae](https://github.com/ytsaurus/ytsaurus/commit/4847819ed7ae329117a4a75150674762ea51fe32) | 193 | 147 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/25.2` | [78e1bc181cdb](https://github.com/ytsaurus/ytsaurus/commit/78e1bc181cdb11e651e944160b4519be0b45a3c6) | 196 | 150 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/25.3` | [151a441ed08d](https://github.com/ytsaurus/ytsaurus/commit/151a441ed08dbab506859d15fa48179ea7e98985) | 204 | 157 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/25.4` | [22e6287d58bd](https://github.com/ytsaurus/ytsaurus/commit/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6) | 209 | 159 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/26.1` | [f4ff0fbbd1cb](https://github.com/ytsaurus/ytsaurus/commit/f4ff0fbbd1cb1aca8e8eee3b907e1bb72495f1f9) | 210 | 159 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/f4ff0fbbd1cb1aca8e8eee3b907e1bb72495f1f9/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/f4ff0fbbd1cb1aca8e8eee3b907e1bb72495f1f9/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/f4ff0fbbd1cb1aca8e8eee3b907e1bb72495f1f9/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |
| `stable/26.2` | [b916333f5818](https://github.com/ytsaurus/ytsaurus/commit/b916333f58185c5b9fdfc655e1d62e9ce84b655b) | 213 | 161 | [driver registry](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/driver/driver.cpp), [RPC declarations](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/api/rpc_proxy/api_service_proxy.h), [discovery declarations](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/api/rpc_proxy/discovery_service_proxy.h) |

Each snapshot also declares `DiscoveryService.DiscoverProxies`. Counts include internal commands and aliases; they do not measure deployed endpoint availability.

### Commands added after the 24.1 baseline

Table 6 lists command spellings first registered after the `stable/24.1` baseline.

<small>Table 6 — Commands added after the 24.1 baseline</small>

| Command spelling | Tag | First stable registration |
| --- | --- | --- |
| `cancel_tablet_transition` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L241) |
| `check_cluster_liveness` | `since-26.2` | [stable/26.2](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/driver/driver.cpp#L345) |
| `check_operation_permission` | `since-25.2` | [stable/25.2](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/driver/driver.cpp#L308) |
| `dump_job_proxy_log` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp#L312) |
| `finish_distributed_write_file_session` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L422) |
| `finish_distributed_write_session` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp#L401) |
| `flow_execute` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L405) |
| `flow_execute_plaintext` | `since-26.1` | [stable/26.1](https://github.com/ytsaurus/ytsaurus/blob/f4ff0fbbd1cb1aca8e8eee3b907e1bb72495f1f9/yt/yt/client/driver/driver.cpp#L414) |
| `forsake_chaos_coordinator` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L425) |
| `forsake_chaos_shortcut` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/driver/driver.cpp#L440) |
| `get_connection_orchid_value` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/driver/driver.cpp#L443) |
| `get_current_user` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L360) |
| `get_job_trace` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp#L296) |
| `get_ordered_tablet_safe_trim_row_count` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L436) |
| `get_query_declared_parameters_info` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L389) |
| `get_table_mount_info` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp#L237) |
| `list_job_traces` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L310) |
| `list_operation_events` | `since-25.2` | [stable/25.2](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/driver/driver.cpp#L311) |
| `partition_file` | `since-26.2` | [stable/26.2](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/driver/driver.cpp#L194) |
| `patch_op_spec` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L277) |
| `patch_operation_spec` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L285) |
| `ping_chaos_lease` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L260) |
| `ping_distributed_write_file_session` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L421) |
| `ping_distributed_write_session` | `since-25.2` | [stable/25.2](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/driver/driver.cpp#L412) |
| `read_file_partition` | `since-26.2` | [stable/26.2](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/driver/driver.cpp#L196) |
| `read_shuffle_data` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L408) |
| `read_table_partition` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L201) |
| `remove_chaos_cell_mailbox` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/driver/driver.cpp#L441) |
| `reset_dynamically_propagated_master_cells` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/driver/driver.cpp#L346) |
| `run_job_shell_command` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L319) |
| `start_distributed_write_file_session` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L420) |
| `start_distributed_write_session` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp#L400) |
| `start_shuffle` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L407) |
| `transfer_bundle_resources` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/driver/driver.cpp#L302) |
| `write_file_fragment` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/driver/driver.cpp#L423) |
| `write_shuffle_data` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L409) |
| `write_table_fragment` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/driver/driver.cpp#L413) |

There are **37** current-main command spellings added after the baseline; **176** are present in the baseline. Every current-main command spelling appears in at least one inspected stable snapshot. No current-main spelling disappears from later inspected snapshots after its first appearance.

### RPC declarations added after the baseline

Table 7 lists public proxy RPC declarations first present after the `stable/24.1` baseline.

<small>Table 7 — RPC declarations added after the 24.1 baseline</small>

| Wire method | Tag | First stable declaration |
| --- | --- | --- |
| `ApiService.CheckOperationPermission` | `since-25.2` | [stable/25.2](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L122) |
| `ApiService.DumpJobProxyLog` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L129) |
| `ApiService.FinishDistributedWriteFileSession` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L233) |
| `ApiService.FinishDistributedWriteSession` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L210) |
| `ApiService.FlowExecute` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L201) |
| `ApiService.GetCurrentUser` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L184) |
| `ApiService.GetJobTrace` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L124) |
| `ApiService.GetQueryDeclaredParametersInfo` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L212) |
| `ApiService.ListJobTraces` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L120) |
| `ApiService.ListOperationEvents` | `since-25.2` | [stable/25.2](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L115) |
| `ApiService.PartitionFile` | `since-26.2` | [stable/26.2](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L147) |
| `ApiService.PatchOperationSpec` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L112) |
| `ApiService.PingChaosLease` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L86) |
| `ApiService.PingDistributedWriteFileSession` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L232) |
| `ApiService.PingDistributedWriteSession` | `since-25.2` | [stable/25.2](https://github.com/ytsaurus/ytsaurus/blob/78e1bc181cdb11e651e944160b4519be0b45a3c6/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L220) |
| `ApiService.ReadFilePartition` | `since-26.2` | [stable/26.2](https://github.com/ytsaurus/ytsaurus/blob/b916333f58185c5b9fdfc655e1d62e9ce84b655b/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L148) |
| `ApiService.ReadShuffleData` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L224) |
| `ApiService.ReadTablePartition` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L152) |
| `ApiService.ResetDynamicallyPropagatedMasterCells` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L177) |
| `ApiService.RunJobShellCommand` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L136) |
| `ApiService.StartDistributedWriteFileSession` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L231) |
| `ApiService.StartDistributedWriteSession` | `since-24.2` | [stable/24.2](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L209) |
| `ApiService.StartShuffle` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L221) |
| `ApiService.TransferBundleResources` | `since-25.4` | [stable/25.4](https://github.com/ytsaurus/ytsaurus/blob/22e6287d58bd535623b9ec4e3d359d75f6a0c8e6/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L67) |
| `ApiService.WriteFileFragment` | `since-25.3` | [stable/25.3](https://github.com/ytsaurus/ytsaurus/blob/151a441ed08dbab506859d15fa48179ea7e98985/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L234) |
| `ApiService.WriteShuffleData` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L222) |
| `ApiService.WriteTableFragment` | `since-25.1` | [stable/25.1](https://github.com/ytsaurus/ytsaurus/blob/4847819ed7ae329117a4a75150674762ea51fe32/yt/yt/client/api/rpc_proxy/api_service_proxy.h#L217) |

### Removed historical spellings and remaining comparisons

`participant_write_table` is registered in the inspected `stable/24.2` source, and `ApiService.ParticipantWriteTable` is declared there. Neither spelling is present in the inspected 24.1 or 25.1–26.2 snapshots, or in the pinned main catalog. This historical spelling remains outside the current-main command index. [24.2 driver registration](https://github.com/ytsaurus/ytsaurus/blob/7368708d848c6a4056e18b2824bbe0fe328cfeaa/yt/yt/client/driver/driver.cpp#L402).

Command-presence tracking is now complete for these snapshots. Follow-up comparisons should cover parameter additions and removals, defaults and aliases, nested schemas, transport implementation changes, feature gates, protobuf fields, and response semantics. Keep branch commits pinned and record backports and removals explicitly; a since tag alone cannot express all release compatibility differences.
