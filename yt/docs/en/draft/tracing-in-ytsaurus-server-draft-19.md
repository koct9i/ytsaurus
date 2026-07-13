# Using tracing in YTsaurus server (draft)

Draft number: 19

{% note warning "Draft" %}

This article is a working draft for operators and developers who want to use distributed tracing in YTsaurus server components. Verify option names against the exact YTsaurus version deployed in your cluster before copying examples to production.

{% endnote %}

## Why trace YTsaurus server requests

Tracing complements logs, metrics, and profiling. It is most useful when an issue crosses process boundaries or when a single request fans out into many asynchronous actions. Typical uses include:

- finding where latency is spent in a user request that enters through an HTTP proxy or RPC proxy;
- following internal RPC calls between proxies, masters, nodes, scheduler, controller agents, and job proxies;
- debugging rare slow requests by forcing or sampling traces for a specific user;
- correlating server logs with `trace_id`, request ids, users, commands, and RPC methods;
- preserving context through asynchronous callbacks, mutations, and queued requests;
- investigating job-related delays and retrieving job trace events;
- attaching allocation tags to sampled requests in components that support request-scoped memory attribution.

## Tracing model

YTsaurus has an internal trace context that is propagated with requests. A trace context carries the trace id, span id, parent span id, request id, logging tag, sampling state, optional baggage, tags, logs, and timing information. A context can be:

- **disabled**: used only to propagate identifiers such as trace id and logging tag;
- **recorded**: eligible for annotations and later sampling;
- **sampled**: reported to the configured tracer when the span is finished.

Server-side code creates root spans for new work or child spans from incoming request metadata. Child spans inherit identifiers from the parent by default, so a single user action can be reconstructed across component boundaries.

## Supported collectors, protocols, and propagation formats

### Export to Jaeger

The server tracing library includes a Jaeger tracer. Configure the global `jaeger` singleton for components that link the Jaeger tracing library. The tracer exports spans to a Jaeger collector over a gRPC channel that calls the Jaeger v2 collector service.

Common static options include:

| Option | Purpose |
| --- | --- |
| `service_name` | Logical service name reported to Jaeger. If it is absent, the Jaeger tracer is disabled. |
| `collector_channel_config` | gRPC channel configuration for the collector endpoint. |
| `flush_period` | Periodic flush interval for queued spans. |
| `rpc_timeout` | Timeout for requests sent to the collector. |
| `max_request_size` | Maximum size of a collector request. |
| `max_batch_size` | Maximum number of spans per batch. |
| `max_memory` | In-memory queue limit. |
| `subsampling_rate` | Optional additional downsampling before export. |
| `process_tags` | Tags attached to the reported process. |
| `enable_pid_tag` | Adds the process id as a process tag. |
| `tvm_service` | Optional TVM configuration for collector authentication. |
| `test_drop_spans` | Test mode that logs dropped batches instead of sending them. |

Dynamic options include `collector_channel`, `max_request_size`, `max_memory`, `subsampling_rate`, and `flush_period`.

Example skeleton:

```yson
jaeger = {
    service_name = "yt-rpc-proxy";
    collector_channel_config = {
        address = "jaeger-collector.example.net:14250";
    };
    flush_period = 15s;
    rpc_timeout = 15s;
    max_batch_size = 128;
    max_request_size = 128KB;
    max_memory = 1GB;
    process_tags = {
        cluster = "prod";
        role = "rpc_proxy";
    };
}
```

### Incoming HTTP `traceparent`

HTTP server helpers understand the W3C Trace Context `traceparent` header. If a valid header is present, YTsaurus creates a child server span from it; otherwise it creates a new root `HttpServer` span. The response headers can include `X-YT-Trace-Id`, which lets clients and operators jump from a failed or slow HTTP request to logs or tracing storage.

The Python client can generate `traceparent` automatically when proxy `force_tracing` is enabled, and ClickHouse helpers accept an explicit `traceparent` value.

### Native YTsaurus RPC tracing extension

Native RPC requests propagate tracing data through the YTsaurus RPC request header tracing extension. This carries trace and span identifiers, sampling/debug flags, and optionally tracing baggage. RPC client configuration has `tracing_mode`, and channel/dispatcher configuration controls whether baggage is sent with `send_tracing_baggage`.

Use native propagation for YTsaurus-to-YTsaurus calls. Use HTTP `traceparent` for HTTP clients and integrations that already produce W3C trace context.

### Baggage

Trace contexts can carry optional baggage encoded as YSON attributes. Baggage is inherited by child trace contexts and can be serialized in the native RPC tracing extension when both the dispatcher and service descriptor allow it. Use baggage sparingly: it is copied across request boundaries and can increase RPC metadata size.

## Sampling and forcing traces

Tracing every request in a busy cluster is usually too expensive. YTsaurus components that accept user traffic expose sampler configuration with these controls:

| Option | Purpose |
| --- | --- |
| `global_sample_rate` | Probability for sampling any request. |
| `user_sample_rate` | Per-user sampling probability. |
| `user_endpoints` | Routes spans for selected users to a specific collector endpoint. |
| `clear_sampled_flag` | Clears an incoming sampled flag for selected users. |
| `min_per_user_samples` | Samples the first N requests for each user in a time window. |
| `min_per_user_samples_period` | Window used by `min_per_user_samples`. |

RPC proxy and HTTP proxy configurations also have `force_tracing` switches that mark incoming requests as sampled. This is convenient for troubleshooting, but it can overload the collector if left enabled on high-traffic components.

Example sampler skeleton:

```yson
tracing = {
    global_sample_rate = 0.001;
    user_sample_rate = {
        alice = 1.0;
        batch_user = 0.01;
    };
    min_per_user_samples = 5;
    min_per_user_samples_period = 1m;
}
```

## Component-specific entry points

### HTTP proxy

HTTP proxy creates or continues trace contexts for HTTP requests. Configure its `tracing` sampler to control which user requests are exported. Enable `force_tracing` only for a short diagnostic window or for a small test proxy.

For HTTP clients, send a W3C `traceparent` header when you need to connect a client-side trace to YTsaurus server spans. Save the `X-YT-Trace-Id` response header in application logs.

### RPC proxy

RPC proxy samples user requests after authentication. Sampled traces can be annotated with user and user tag, and allocation tags can be enabled in dynamic configuration for request-scoped memory investigation.

### Exec node, scheduler, controller agent, and job proxy

Exec-node connectors to the scheduler and controller agents use tracing samplers for heartbeat and controller-agent communication. Job proxy has dynamic Jaeger settings, and exec-node configuration contains a `job_proxy_jaeger` template that is passed to job proxies. These traces are useful when diagnosing job startup, scheduling, heartbeat, and controller-agent interaction delays.

YTsaurus also has job trace collection and APIs/CLI commands for retrieving job trace events. Treat job traces as a complementary mechanism: distributed tracing explains server-side control flow, while job trace events explain what happened inside a particular job.

### Masters and nodes

Masters and cluster nodes link the Jaeger tracing library and can export internal spans when configured. Use these traces when debugging master mutations, transaction finishing, Cypress access, tablet-node reporting, chunk reading, or other internal workflows that already create trace contexts.

## Operational workflow

1. **Deploy a collector and UI.** Start with Jaeger collector and UI, or an observability backend that accepts Jaeger collector gRPC traffic.
2. **Configure `jaeger`.** Set `service_name`, collector channel, batch limits, and useful process tags for each server component.
3. **Enable low-rate sampling.** Begin with `global_sample_rate` around `0.001` or with per-user sampling.
4. **Capture identifiers.** Ensure logs include trace ids, request ids, users, and commands. Encourage clients to log `X-YT-Trace-Id`.
5. **Force only during incidents.** Temporarily enable `force_tracing` or a per-user sample rate of `1.0` for a narrow scope, then revert it.
6. **Validate collector pressure.** Watch tracer queue memory, dropped spans, collector RPC errors, and collector storage ingestion rate.
7. **Iterate on tags.** Add process tags such as cluster, datacenter, role, shard, or environment so traces are easy to filter.

## Limitations and caveats

- Jaeger export is the server-side exporter implemented in this codebase. OpenTelemetry-native OTLP export and Zipkin export are not exposed by the YTsaurus C++ server tracer in the files reviewed for this draft.
- HTTP propagation supports `traceparent`; native RPC propagation uses YTsaurus protobuf extensions. Do not assume every third-party tracing header is understood.
- Sampling decisions can be inherited from incoming requests. Use `clear_sampled_flag` for users or integrations that send overly aggressive sampled flags.
- Baggage should be small and non-sensitive. It may cross service boundaries and appear in diagnostics.
- Trace volume grows with fan-out. A single sampled request can produce many spans on a large cluster.

## Troubleshooting checklist

- No traces in Jaeger: verify `service_name` is set, collector address is reachable, and the component links/enables the Jaeger tracer.
- Too many traces: lower `global_sample_rate`, remove temporary `force_tracing`, or add subsampling in the Jaeger config.
- Missing user requests: check proxy `tracing` sampler config and whether the incoming request was authenticated as the expected user.
- Missing child spans: check whether the internal call path propagates trace context and whether baggage/metadata propagation is enabled where needed.
- Cannot correlate with a client request: log `X-YT-Trace-Id` for HTTP and request ids for RPC calls.
