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
- **propagating storage** carries typed ambient values—most notably the current trace context—across callback boundaries.

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

## Memory reference counting { #memory-refcount }

Most framework objects use intrusive reference counting. Derive owned objects from `TRefCounted`, construct them with `New<T>()`, and expose `TIntrusivePtr<T>` aliases declared with `DEFINE_REFCOUNTED_TYPE`. The strong reference owns the object; a `TWeakPtr<T>` observes it without extending its lifetime and must be promoted with `Lock()` before use.

Reference counting is central to async lifetime management:

```cpp
invoker->Invoke(BIND(&TController::Run, MakeStrong(this)));
invoker->Invoke(BIND(&TController::Poll, MakeWeak(this)));
```

`MakeStrong(this)` keeps the receiver alive through callback completion. `MakeWeak(this)` lets a callback become a no-op after destruction. A raw pointer or `Unretained(this)` is valid only when an external lifetime invariant guarantees that the callback cannot outlive the object.

Strong-reference cycles do not collect themselves. A component that stores a callback which strongly captures the component creates a cycle; break it with weak capture, explicit `Reset`, or shutdown ordering. Destruction occurs when the last strong reference is released, potentially on whichever thread or fiber performs that release, so destructors must not assume invoker affinity unless release is deliberately scheduled there. Refcounts protect lifetime, not object state: concurrent access still needs affinity, atomics, or locking.

## Memory tracing and accounting { #memory-tracking }

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

### Memory accounting primitives { #memory-accounting }

Allocation tags answer *who allocated this memory?*; `IMemoryUsageTracker` answers *how much memory may this subsystem use?* These mechanisms complement each other but are independent.

An `IMemoryUsageTracker` exposes `Acquire`/`Release` for unconditional accounting and `TryAcquire`/`TryChange` for limit-aware operations that return `TError`. `GetUsed`, `GetFree`, `GetLimit`, and `IsExceeded` expose tracker state. Prefer the fallible methods when a request can reject or shed work instead of violating its limit.

`TMemoryUsageTrackerGuard` is the RAII primitive for recounting a changing amount:

```cpp
auto guardOrError = TMemoryUsageTrackerGuard::TryAcquire(tracker, initialSize);
if (!guardOrError.IsOK()) {
    return guardOrError;
}
auto guard = std::move(guardOrError.Value());
if (auto error = guard.TrySetSize(actualSize); !error.IsOK()) {
    return error;
}
```

`Build` starts at zero, `Acquire` accounts immediately, and `TryAcquire` enforces the limit. `SetSize`/`TrySetSize`, `IncreaseSize`, and `DecreaseSize` recount as a buffer changes. A non-unit granularity batches tracker updates while retaining the exact logical size. `TransferMemory` moves part of the accounted amount to another guard without double counting. Destruction releases the accounted memory; `ReleaseNoReclaim` deliberately detaches the guard without decrementing the tracker and therefore requires the caller to transfer or release that accounting manually.

For reference-counted buffers, use `TrackMemory`/`TryTrackMemory`: the returned `TSharedRef` holder releases accounting when the final tracked reference dies. Tracking the same reference again normally replaces its previous tracking holder; request `keepExistingTracking` only when accounting in multiple trackers is intentional. `TMemoryTrackedBlob` couples blob resize/reserve operations to a guard. `CreateScopedMemoryTracker` delegates to an underlying tracker while exposing the current and peak amount acquired through that scope.

Accounting is not allocation and does not extend lifetime. Keep the guard or tracked holder alive for exactly as long as the bytes it represents, and make ownership transfer explicit at asynchronous boundaries.

## Logging { #logging }

Declare a component logger and use severity-specific macros such as `YT_LOG_TRACE`, `YT_LOG_DEBUG`, `YT_LOG_INFO`, `YT_LOG_WARNING`, `YT_LOG_ERROR`, and `YT_LOG_FATAL`. Use structured-looking fields in the message rather than building strings eagerly:

```cpp
YT_LOG_INFO("Request completed (Method: %v, RowCount: %v)", method, rowCount);
YT_LOG_WARNING(error, "Request failed (Method: %v)", method);
```

Pass a `TError` or `TErrorException` to an error-aware logging overload so nested errors and attributes are retained in the rendered event. Choose levels by action: trace/debug for diagnosis, info for meaningful lifecycle events, warning for a recoverable abnormal condition, error when an operation has failed, and fatal only when the process cannot safely continue. Do not use logging as synchronization and do not place secrets or unbounded payloads in fields.

### Trace context in logs { #trace-context-in-log }

Every log call obtains a `TLoggingContext`. When a current trace context exists, the logging layer automatically copies its trace id, request id, and logging tag into the log event; it also records timestamp, OS thread id/name, and fiber id. Thus a log emitted by a propagated `BIND` callback remains correlated with the initiating request without manually formatting those identifiers.

Set a concise stable value with `traceContext->SetLoggingTag(...)` when operators need a human-readable correlation tag. The request id and trace id are separate machine-readable fields. Formatter configuration controls which fields appear in plain-text or structured output, so application code should not duplicate them in every message. Detached work created with `BIND_NO_PROPAGATE` has no inherited trace fields unless it explicitly installs a context.

## `TError` { #errors }

`TError` is the framework's structured failure value. Besides a code and message, it may carry origin information, attributes, and nested inner errors. Prefer it at asynchronous and component boundaries: `TFuture<T>` resolves to `TErrorOr<T>`, and `TFuture<void>` resolves to `TError`, so failures retain their structure instead of being flattened into strings.

```cpp
return ReadRow().Apply(BIND([] (const TErrorOr<TRow>& rowOrError) {
    if (!rowOrError.IsOK()) {
        return TError("Failed to read input row") << rowOrError;
    }
    return ValidateRow(rowOrError.Value());
}));
```

Use `TErrorAttribute("key", value)` for machine-readable detail and nest the original error when adding context. `Wrap(...)` and `THROW_ERROR_EXCEPTION_IF_FAILED(error, "...")` preserve the causal error tree. Avoid logging and then discarding an error at every layer; either handle it, enrich and return it, or log it once at the boundary that owns the operation.

## Exceptions { #exceptions }

`TErrorException` is the exception wrapper around a `TError`. `THROW_ERROR_EXCEPTION(...)` constructs and throws one; `THROW_ERROR error` throws an existing structured error. Catch `const TErrorException&` when exception-based control flow is required and use `ex.Error()` to recover the structured value. Ordinary `std::exception` can be converted to `TError`, but usually loses framework-specific codes and attributes.

Exceptions are useful for synchronous-looking fiber code and validation helpers, but futures must still be completed with an error. Framework continuation helpers generally convert a thrown exception to a failed future; code that manually invokes a promise must catch at its ownership boundary and complete the promise with a converted error. Never let an exception escape a thread entry point, destructor, or C callback.

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

## Assertions and affinity checks { #assertions }

Use assertion macros for programmer errors and broken invariants, not for invalid user input, failed I/O, overload, or other expected runtime failures:

- `YT_ASSERT(expr[, description])` checks only in debug builds; in release builds it does not evaluate the expression. Never put side effects in it.
- `YT_VERIFY(expr[, description])` evaluates and checks in every build and terminates the process on failure.
- `YT_ABORT([description])` marks an unconditional fatal invariant violation; `YT_UNIMPLEMENTED` and `YT_UNREACHABLE` document narrower cases.
- `YT_ASSERT_INVOKER_AFFINITY(invoker)` and related thread/spin-lock/serialized-invoker macros document and debug-check async execution affinity.

Failed assertions terminate rather than producing a `TError`; they are not an error-handling shortcut. For preconditions originating outside the process, return or throw a structured error instead.

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

## Synchronous waiting { #synchronous-waiting }

`TFuture::Get` and related blocking waits park the current OS thread. Reserve them for process edges such as tests, command-line tools, startup code before scheduler activity, or integration with a foreign blocking API. They can deadlock when called on an invoker whose queue must run the callback that completes the future.

Inside scheduler-managed code use fiber-aware `WaitFor`, not a thread-blocking wait. Even then, synchronous-looking waits should be local and documented: returning the future usually composes better and avoids retaining a fiber stack.

## Asynchronous waiting and continuations { #asynchronous-waiting }

Prefer callbacks and continuation composition when the caller does not need synchronous-looking control flow. `Subscribe` observes completion and receives `TErrorOr<T>` (or `TError` for `void`); it does not transform the result. `Apply` transforms a successful value or complete result and returns a new future, flattening a callback result that is itself a future.

```cpp
return Fetch().Apply(BIND([] (const TValue& value) {
    return Store(Convert(value)); // TFuture<void> is flattened.
}).AsyncVia(invoker));
```

Use `Via`/`AsyncVia` deliberately to select where the callback runs. Without an explicit invoker, a completion callback may run inline on the producer's thread; callback bodies must therefore be short and must not silently rely on caller affinity.

## Future cancellation and timeouts { #future-cancellation }

Cancellation is advisory. `future.Cancel(error)` completes or requests cancellation of shared state and invokes producer-side cancellation handlers registered through the promise, but it cannot forcibly stop code already running. Producers must define safe cancellation points, release resources, and tolerate a race between normal completion and cancellation; use `TrySet` where either side may win.

`WithTimeout` creates a derived future that fails after a deadline and propagates cancellation according to its options. A timeout does not prove that remote or underlying work stopped. Keep cancellation errors structured, normally with an appropriate cancellation or timeout code, and make cleanup idempotent.

Do not use cancellation as object-lifetime management. Hold an explicit strong reference for work that must finish, or use a weak reference for work that may disappear during shutdown.

## Combining futures { #future-combining }

Use combiners rather than counters and hand-written promise fan-in:

- `AllSucceeded` completes successfully only if every input succeeds and fails according to combiner options when an input fails;
- `AllSet` waits for every input and returns every `TErrorOr<T>`, which is useful for best-effort batches and cleanup;
- `AnySet` completes when one input is set;
- `AnySetMatching` waits for an input selected by a predicate;
- timeout and bounded-concurrency variants should be preferred when the operation needs those policies.

An empty input, error propagation, input cancellation, and ordering are part of each helper's contract; consult its declaration rather than reproducing assumed behavior. Keep the input futures alive when required and avoid capturing a fan-in promise strongly from callbacks that it owns.

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

## Concurrency control { #concurrency-control }

Choose the narrowest primitive that expresses the invariant:

| Primitive | Use |
| --- | --- |
| Serialized invoker | Run callbacks one at a time and preserve component affinity without explicit locking. |
| Bounded-concurrency invoker | Cap callbacks admitted concurrently while retaining invoker composition. |
| `TAsyncSemaphore` | Account variable-sized slots; `AsyncAcquire` returns a future and the RAII guard releases slots. |
| `TAsyncReaderWriterLock` | Coordinate asynchronous readers and writers without blocking a worker thread. |
| Spin lock/mutex | Protect a very short non-suspending critical section. |

Never suspend a fiber, invoke arbitrary user code, or wait on a future while holding a conventional lock. Prefer serialized ownership for mutable component state. When using async semaphore or lock guards, define cancellation behavior while acquisition is pending and keep the guard's lifetime exactly equal to the protected operation.

Concurrency limits and memory limits solve different problems. Apply both when each admitted request can consume significant memory; otherwise a safe callback count can still exceed memory, or a safe byte count can still overload a downstream service.

## Thread-local storage { #thread-local-storage }

YTsaurus has ordinary C++ `thread_local` state and wrappers such as `YT_DEFINE_THREAD_LOCAL`. The wrapper intentionally hides direct access behind a function to reduce compiler caching problems in fiber-aware code.

This matters because fibers may switch on the same OS thread and may later resume elsewhere. A raw `thread_local` variable describes the current OS thread, not the logical request. Therefore:

- Use thread-local state only for data that is truly thread-affine: cached thread id, thread name, allocator state, profiling guards tied to the executor thread, etc.
- Do not store request identity, trace/span information, user, or cancellation scope in plain thread-local variables unless it is guarded by the framework's propagation mechanisms.
- Prefer RAII guards for ambient state and keep their scope small.
- Assume values not stored in propagating storage will not automatically follow `BIND` callbacks.

## Fiber-local context { #fiber-local-context }

`TFlsSlot<T>` stores a value in the currently executing fiber's `TFls` record. Unlike raw `thread_local`, the value follows that fiber when the scheduler resumes it on another worker thread. Slots are appropriate for scheduler and diagnostic state whose lifetime is exactly the current fiber; access should still be hidden behind a narrow API rather than spread through application code.

Fiber-local does not automatically mean callback-propagated. A new callback may execute in another fiber with a different `TFls`. State that must cross `BIND` boundaries belongs in `TPropagatingStorage`, or must be reinstalled explicitly by a guard or guarded invoker. The scheduler switches propagating storage with the fiber, while `BIND` additionally snapshots it at callback construction.

Use RAII for temporary ambient values. `TPropagatingValueGuard<T>` exchanges one typed value and restores it at scope exit; dedicated facilities such as trace guards, codicil guards, and logging setters may use their own fiber-local slots and switching hooks. Never retain a pointer or reference to a slot value across `Yield`, `WaitFor`, or `SwitchTo`: suspension can invalidate assumptions about the active fiber and execution thread.

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

### What `BIND` propagates and how { #propagated-context }

`BIND` does not maintain a hard-coded list of fields. It copies the current `TPropagatingStorage` into the callback's bind state. The storage is a fiber-local, copy-on-write map keyed by C++ type (`std::type_index`) with type-erased values (`std::any`). Copying a callback is therefore cheap until one copy changes a value. Immediately before the callback body runs, the callback installs its captured storage with `TPropagatingStorageGuard`; the guard restores the previous storage after the call. Fiber switches use the same storage-switch machinery.

The framework currently puts `TTraceContextPtr` in this map. Consequently, all state reachable from that trace-context object follows a normal `BIND` callback:

- trace, span, and parent-span identities, sampling/debug state, and span name;
- request id, target endpoint, logging tag, and baggage;
- trace tags and profiling tags;
- allocation tags used by allocator profiling;
- timing, CPU-time, and async-child accounting kept by the context.

A child trace context inherits request id, target endpoint, logging tag, baggage, profiling tags, and allocation tags from its parent. This is distinct from merely copying propagating storage: the latter shares the current `TTraceContextPtr`, while `CreateChild` constructs a new span linked to its parent.

Other subsystems may place their own type in propagating storage by using `TPropagatingValueGuard<T>`. A type has at most one value in a storage snapshot. The guard temporarily exchanges the value and restores or removes it on scope exit. Do not put mutable request state there merely for convenience: callback copies can share the stored object, and ambient dependencies are harder to audit than explicit parameters.

Not every fiber-local diagnostic is part of propagating storage. For example, codicil stacks, the minimum log level, and the thread-message tag are fiber-local facilities with their own switching behavior; do not assume that adding one of these guards before `BIND` makes it part of the callback's captured map. Use the subsystem's dedicated guarded invoker or install its guard in the callback when a value must cross that boundary.

`BIND_NO_PROPAGATE` stores no snapshot and installs no propagation guard. It preserves the callable and bound arguments, but does not bring any typed values from the bind-time storage. The callback therefore sees whatever ambient storage is already installed at its execution site; scheduler queues normally invoke detached callbacks with empty storage, but infrastructure that invokes one inline should not mistake “not captured” for “forced to null”.

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
