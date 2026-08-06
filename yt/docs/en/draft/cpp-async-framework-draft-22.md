<!--
Draft number: 22
Author: AI agent (GitHub Copilot)
Created: 2026-08-05
Status: In progress
Target: developer-guide/cpp
-->

# YTsaurus C++ async framework

{% note warning "Draft" %}

This article is an implementation-oriented draft for developers who read or write YTsaurus C++ code. Names and recommendations are based on the current source tree and should be checked against component owners before this page is promoted from Draft.

{% endnote %}

YTsaurus C++ code uses a small set of primitives for asynchronous execution:

- **callbacks/closures** describe delayed work;
- **invokers** choose where that work runs;
- **futures/promises** pass results between producers and consumers;
- **fibers** allow code running on scheduler threads to wait without blocking an OS thread;
- **propagating storage** carries execution context such as trace context, codicils, profiling tags, and other ambient state across callback boundaries.

The most important rule is: do not treat asynchronous code as "just threads". Most server code runs on invokers and often inside fibers, so blocking, thread-local state, and context propagation have different consequences than in plain `std::thread` code.

## Core mental model { #mental-model }

A typical async flow looks like this:

```cpp
auto asyncResult = DoRequest();
return asyncResult.Apply(BIND([] (const TResponse& response) {
    return ConvertResponse(response);
}).AsyncVia(invoker));
```

Conceptually:

1. `DoRequest` returns a `TFuture<TResponse>` immediately.
2. `Apply` attaches a continuation.
3. `BIND` captures the continuation body and the current propagating context.
4. `AsyncVia(invoker)` schedules the continuation through the selected invoker.
5. If some fiber calls `WaitFor(asyncResult)`, the fiber is suspended and later resumed; the worker thread is not intentionally parked by the wait.

This model keeps services composable: a request handler can start I/O, attach continuations, switch execution queues, preserve trace context, and still expose a single `TFuture<T>` to its caller.

## Invokers { #invokers }

`IInvoker` is the framework abstraction for an execution target. Instead of spawning a thread directly, code normally obtains an invoker from an action queue, thread pool, serialized invoker, fair-share pool, periodic executor, or component-specific scheduler and submits a `TClosure` to it.

Use invokers to express both **where** and **under which ordering/concurrency constraints** work runs:

| Invoker style | Typical purpose |
| --- | --- |
| Action queue invoker | Single-threaded component affinity and simple serialization. |
| Serialized invoker | Preserve callback order while using another underlying invoker for actual execution. |
| Bounded-concurrency invoker | Limit number of concurrently running callbacks. |
| Fair-share invoker pool | Isolate users/buckets while sharing a worker pool. |
| Current invoker | Resume on the same logical execution context after a fiber wait or continuation. |

Guidelines:

- Prefer passing `IInvokerPtr` explicitly to objects that own asynchronous work.
- Use `GetCurrentInvoker()` only when resuming the current logical flow is intentional.
- Avoid running heavy CPU work on invokers intended for short control-plane callbacks.
- Check affinity requirements before touching component state; many classes assume all mutation of their internals happens on one invoker.
- Do not block an invoker thread with synchronous I/O or long sleeps. If waiting for a future inside the framework, prefer fiber-aware waiting.

## Futures and promises { #futures }

`TFuture<T>` and `TPromise<T>` are the standard result channel. A promise is held by the producer; a future is handed to consumers. The result type is effectively `TErrorOr<T>` (`TError` for `void`), so errors and values travel through the same channel.

Common operations:

```cpp
auto promise = NewPromise<TValue>();
TFuture<TValue> future = promise.ToFuture();

future.Subscribe(BIND([] (const TErrorOr<TValue>& result) {
    if (!result.IsOK()) {
        // Handle error.
        return;
    }
    // Use result.Value().
}));

promise.Set(value);
```

Practical rules:

- Return futures from asynchronous APIs; do not hide async work behind blocking calls.
- Use `MakeFuture(value)` or `MakeFuture(TError(...))` for already-known results.
- Use aggregation helpers such as `AllSet`/`AllSucceeded` where the caller needs to join several futures.
- Handle cancellation deliberately. Future cancellation is advisory: it tells the producer that the value is no longer needed, but cleanup still depends on producer-side cancellation handlers.
- Avoid `WaitFor` in library code unless the function is explicitly fiber-only or has clear invoker semantics. Prefer returning a future and composing with continuations.

## Fibers and fiber-aware waiting { #fibers }

A fiber is a cooperatively scheduled execution stack managed by the YTsaurus scheduler. Fibers allow code to be written in a synchronous-looking style while still freeing the OS thread when it waits for asynchronous work.

Important operations include:

```cpp
auto resultOrError = WaitFor(asyncResult);
if (!resultOrError.IsOK()) {
    THROW_ERROR resultOrError;
}

SwitchTo(otherInvoker);
Yield();
```

`WaitFor` defaults to suspending the current fiber and resuming it through an invoker. `WaitForFast` avoids rescheduling when the future is already ready. `WaitUntilSet` waits for readiness and leaves result inspection to the caller.

Guidelines:

- Only use fiber-aware waits on threads managed by the scheduler. On arbitrary external threads, blocking waits may harm latency or deadlock if the awaited callback needs the blocked thread.
- Do not hold locks while calling `WaitFor`, `SwitchTo`, or `Yield`; another callback can run while the fiber is suspended.
- Be careful with object lifetimes across waits. Capture `MakeStrong(this)` in callbacks that must keep an object alive, or use `MakeWeak(this)` when shutdown should cancel the action safely.
- Remember that a fiber may resume later and sometimes on a different worker thread, depending on the invoker.

## Stackful coroutines { #coroutines }

The `NConcurrency::TCoroutine` utility is a lower-level stackful coroutine primitive used by some parsers and tests. It provides explicit `Run`/yield control between a caller and coroutine body. It is not the main application-level async API.

Use `TCoroutine` when an algorithm is naturally generator-like or parser-like and needs a private execution stack. For service code, prefer futures, callbacks, invokers, and fibers.

## Thread-local and fiber-local state { #local-state }

YTsaurus has ordinary C++ `thread_local` state and wrappers such as `YT_DEFINE_THREAD_LOCAL`. The wrapper intentionally hides direct access behind a function to reduce compiler caching problems in fiber-aware code.

This matters because fibers may switch on the same OS thread and may later resume elsewhere. A raw `thread_local` variable describes the current OS thread, not the logical request. Therefore:

- Use thread-local state only for data that is truly thread-affine: cached thread id, thread name, allocator state, profiling guards tied to the executor thread, etc.
- Do not store request identity, trace/span information, user, or cancellation scope in plain thread-local variables unless it is guarded by the framework's propagation mechanisms.
- Prefer RAII guards for ambient state and keep their scope small.
- Assume values not stored in propagating storage will not automatically follow `BIND` callbacks.

The phrase "fiber-local storage" in YTsaurus code usually means logical execution context that should follow a fiber/callback chain, not a portable C++ language feature. In practice, this is represented by propagating storage and specialized guards.

## Closures and callbacks { #callbacks }

`TCallback<R(Args...)>` is the general callable type. `TClosure` is the no-argument callback used to represent a unit of work submitted to invokers.

The framework callbacks are reference-counted handles. Store them by value and pass them by `const TCallback<...>&` when avoiding extra reference-count churn matters. Prefer move when transferring ownership of a callback to another object.

Example:

```cpp
void Schedule(IInvokerPtr invoker, TString path)
{
    invoker->Invoke(BIND([path = std::move(path)] {
        YT_LOG_DEBUG("Processing path (Path: %v)", path);
    }));
}
```

## `BIND` { #bind }

`BIND` creates a `TCallback` and captures arguments. It is preferred over raw lambdas when passing work to framework APIs because it integrates with:

- reference-counting helpers (`MakeStrong`, `MakeWeak`, `Unretained`);
- ownership wrappers (`Owned`, `Passed`, `ConstRef`);
- source-location tracking when enabled;
- propagation of the current async context.

Typical patterns:

```cpp
invoker->Invoke(BIND(&TComponent::DoWork, MakeStrong(this), requestId));

future.Subscribe(BIND([weakThis = MakeWeak(this)] (const TError& error) {
    if (auto this_ = weakThis.Lock()) {
        this_->OnFinished(error);
    }
}));
```

Use `MakeStrong(this)` when the callback must keep the object alive until execution. Use `MakeWeak(this)` for background/shutdown-sensitive callbacks that should silently stop if the owner is destroyed.

## `BIND_NO_PROPAGATE` { #bind-no-propagate }

`BIND` captures the current propagating storage at bind time. `BIND_NO_PROPAGATE` creates the same kind of callback but deliberately does **not** capture that ambient context.

Use `BIND_NO_PROPAGATE` for:

- component shutdown hooks and finalizers;
- timer internals and background maintenance loops where the caller's request trace must not leak into future iterations;
- cancellation handlers that should not resurrect stale request context;
- infrastructure code that explicitly installs its own trace or null trace guard.

As a rule of thumb: use `BIND` for request work and continuations; use `BIND_NO_PROPAGATE` for framework plumbing and long-lived background callbacks.

## Trace context and propagation { #trace-context }

Trace context is the ambient span/request context used by tracing, logging, and diagnostics. The current trace context is installed with RAII guards such as `TTraceContextGuard` or `TCurrentTraceContextGuard`. `BIND` captures propagating storage, so the continuation sees the same logical trace context even if it runs later or on another invoker.

Example:

```cpp
auto traceContext = NTracing::GetOrCreateTraceContext("MyOperation");
NTracing::TTraceContextGuard traceGuard(traceContext);

return asyncStep.Apply(BIND([] (const TStepResult& step) {
    YT_LOG_DEBUG("Step finished");
    return MakeNextRequest(step);
}).AsyncVia(invoker));
```

When code intentionally starts a detached background operation, use a fresh trace context or `TNullTraceContextGuard` and `BIND_NO_PROPAGATE` to prevent the parent RPC trace from accumulating unrelated work.

## Backtraces and callback locations { #backtraces }

Asynchronous code makes ordinary thread backtraces less complete: the stack at a crash shows the currently running callback, but not necessarily who scheduled it. YTsaurus compensates with several mechanisms:

- source-location tracking for callbacks when `YT_ENABLE_BIND_LOCATION_TRACKING` is enabled;
- error codicils and trace tags that add logical context to failures;
- fiber-aware debugger helpers that can inspect active and parked fibers;
- logging tags and trace context tags that connect callbacks to requests.

Debugging checklist:

1. Look for the current trace id and logging tags in the failing log line.
2. Inspect the error and its inner errors/codicils; async boundaries often preserve logical context there.
3. If a process is stopped under `gdb`, inspect both OS threads and parked fibers, not only the native thread stack.
4. If a callback appears detached from the initiating request, check whether it used `BIND_NO_PROPAGATE`, raw lambda storage, or a hand-written invoker wrapper.

## Memory allocation tracking { #memory-tracking }

YTsaurus supports memory attribution through memory tags. `TMemoryTagGuard` installs a current tag for the dynamic scope, and helper functions read per-tag usage. Tracing also has allocation-tag hooks that can attribute allocations to tracing state when enabled.

Example:

```cpp
void BuildLargeResponse()
{
    TMemoryTagGuard guard(GetMyComponentMemoryTag());
    auto buffer = TSharedMutableRef::Allocate<TDefaultSharedBlobTag>(Size);
    // Allocation is attributed while the guard is active.
}
```

Guidelines:

- Install memory tags at component/request boundaries where attribution is meaningful.
- Keep guards narrow: broad guards can make unrelated allocations look like part of the same request.
- Remember that asynchronous continuations may run after the original stack has unwound. If attribution must continue, arrange propagation explicitly or install a new guard in the continuation.
- Treat memory tracking as diagnostic metadata, not as ownership. Freeing memory depends on object lifetimes and reference counts.

## Common pitfalls { #pitfalls }

| Pitfall | Why it hurts | Prefer |
| --- | --- | --- |
| Blocking an invoker thread with synchronous I/O | Starves unrelated callbacks sharing that invoker. | Return a future or use an async API. |
| Holding a lock across `WaitFor` | Other callbacks can run while the fiber is suspended, causing deadlocks or invariant breaks. | Release locks before waiting. |
| Capturing raw `this` in delayed callbacks | The owner may be destroyed before callback execution. | `MakeStrong(this)` or `MakeWeak(this)`. |
| Using plain `thread_local` for request data | Fibers and callbacks do not map one-to-one to OS threads. | Trace/context guards and propagating storage. |
| Using `BIND` for indefinite background loops | Request trace/context may leak into unrelated work. | `BIND_NO_PROPAGATE` plus explicit trace setup. |
| Calling `WaitFor` from a foreign thread | The awaited continuation may require the blocked execution context. | Compose with futures or switch into a scheduler-managed invoker. |

## Minimal style guide { #style-guide }

- Async API names often use `Async` or return `TFuture<T>` without blocking.
- Accept `IInvokerPtr` in constructors for components that schedule callbacks.
- Use `BIND` for request-scoped continuations and invoker submissions.
- Use `BIND_NO_PROPAGATE` for shutdown, cancellation, timers, and detached background plumbing.
- Use `WaitFor` only in fiber-aware code and document the invoker on which the fiber resumes if it is not obvious.
- Preserve trace context across meaningful async boundaries; reset it intentionally for detached background work.
- Use memory tags around allocations that need attribution.
