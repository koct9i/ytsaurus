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

`BIND` turns a function, method, or lambda plus bound arguments into a `TCallback` with framework lifetime and context semantics. Its developer-facing contract is:

- bound values are owned by the callback unless an explicit wrapper says otherwise;
- `MakeStrong(this)` keeps an object alive, while `MakeWeak(this)` skips a `void` method call when the object is already gone;
- `Unretained`, `ConstRef`, and similar non-owning wrappers require the referenced object to outlive every invocation;
- the propagating context that is current **when `BIND` is evaluated** is captured with the callback;
- on every invocation that captured context is installed for the callback body and the caller's ambient context is restored afterward;
- copying the callback copies its callable state and captured-context handle; it does not recapture the context of the copying site.

These semantics are the reason to use `BIND` rather than a raw stored lambda at framework boundaries. Whether an invoker runs the callback inline or later does not change which bind-time context its body observes.

```cpp
invoker->Invoke(BIND(&TComponent::DoWork, MakeStrong(this), requestId));

future.Subscribe(BIND([weakThis = MakeWeak(this)] (const TError& error) {
    if (auto this_ = weakThis.Lock()) {
        this_->OnFinished(error);
    }
}));
```

Source-location tracking may additionally record where the callback was bound. Ownership wrappers such as `Owned`, `Passed`, and `ConstRef` alter argument storage or delivery and should be used only when their one-shot or non-owning semantics are intentional.

## `BIND_NO_PROPAGATE` { #bind-no-propagate }

`BIND_NO_PROPAGATE` has the same callable, argument-ownership, reference-counting, and source-location behavior as `BIND`, but it does not capture or install the bind-time propagating context. Its body observes whatever ambient context exists at the invocation site. It therefore means **do not carry context from here**, not **force an empty context when called**.

Use it for component shutdown hooks, cancellation plumbing, timer internals, and long-lived maintenance loops where a request's trace must not escape its lifetime. If detached work needs tracing, create and install a fresh trace context inside that work. Use ordinary `BIND` for request-scoped work and continuations that logically remain part of the current operation.

## Futures and promises { #futures }

`TFuture<T>` and `TPromise<T>` are thread-safe handles to shared result state. The producer owns a promise and consumers receive futures. A result is `TErrorOr<T>` (`TError` for `void`), so every completion is exactly one value or one structured error. If the last promise disappears before setting a result, the state is completed with `EErrorCode::Canceled`; producers must retain a promise until they either complete or deliberately abandon the operation.

```cpp
auto promise = NewPromise<TValue>();
auto future = promise.ToFuture();
StartOperation(BIND([promise] (TErrorOr<TValue> result) {
    promise.TrySet(std::move(result));
}));
return future;
```

Use `Set` when the producer exclusively owns completion and a second completion is a bug. Use `TrySet` when normal completion races with cancellation, timeout, shutdown, or another producer. `MakeFuture(value)` and `MakeFuture(TError(...))` represent already-known results.

### Handling errors from futures { #future-errors }

Choose error handling according to the composition style:

- `Subscribe` is a terminal observer. Its callback receives the complete `TErrorOr<T>` and must inspect `IsOK()` before reading `Value()`.
- An `Apply` callback taking `const T&` runs only for a successful input; an input error bypasses it and completes the returned future with that error.
- An `Apply` callback taking `const TErrorOr<T>&` sees both success and failure and can recover, translate, or enrich an error.
- `WaitFor` returns `TErrorOr<T>`/`TError`; check it or call `ThrowOnError` when exception-style fiber code is intentional.
- At an API boundary, return the original error or wrap it with operation-specific context. Do not replace it with an unstructured message and do not log it at every intermediate layer.

```cpp
return FetchRow().Apply(BIND([] (const TErrorOr<TRow>& result) -> TErrorOr<TValue> {
    if (!result.IsOK()) {
        return TError("Failed to fetch source row") << result;
    }
    return Convert(result.Value());
}));
```

A continuation that throws is converted by the future machinery into a failed returned future. This is useful for local validation, but explicit `TErrorOr` flow makes recovery paths and expected failures clearer.

## Synchronous waiting { #synchronous-waiting }

`BlockingGet` and `BlockingWait` park the current OS thread. Reserve them for process edges such as tests, command-line tools, startup before scheduler activity, or foreign blocking APIs. They can deadlock on an invoker whose queue must run the callback that completes the future.

Inside scheduler-managed code use fiber-aware `WaitFor`, not a thread-blocking wait. Even then, returning and composing a future usually scales better than retaining a suspended fiber stack.

## Asynchronous waiting and continuations { #asynchronous-waiting }

`Subscribe` observes completion without producing another future. `Apply` transforms a result and returns a future; when the callback returns a future, `Apply` flattens it rather than creating a nested future.

```cpp
return Fetch().Apply(BIND([] (const TValue& value) {
    return Store(Convert(value));
}).AsyncVia(invoker));
```

Use `Via`/`AsyncVia` deliberately to select execution affinity. Without an explicit invoker, completion callbacks may run inline on the thread that sets the promise. Keep callback bodies short and never rely on accidental producer affinity.

## Future cancellation and timeouts { #future-cancellation }

Cancellation is a request flowing from consumer to producer, not forced interruption. `future.Cancel(error)` returns whether it won the race to cancel an unset state. With no promise cancellation handler, cancellation sets the shared state to a cancellation error. When the producer registered `promise.OnCanceled(handler)`, cancellation invokes that handler instead; the handler must stop or detach underlying work as appropriate and eventually call `TrySet` on the promise. Merely returning from the handler leaves the future unresolved.

Cancellation races with success and failure. Producer cleanup must be idempotent, and both the normal callback and cancellation handler should use `TrySet`. Code already executing may continue after consumers observe cancellation. `ToUncancelable` shields an operation from downstream cancellation; `ToImmediatelyCancelable` gives consumers prompt cancellation while optionally forwarding the request to the source.

Cancellation also follows continuation relationships. Canceling a derived future normally asks the upstream cancelable state to cancel; if a continuation has already produced an inner future, cancellation can be forwarded to that active operation. Treat this as cooperative propagation: each layer still owns its cleanup and may finish concurrently.

`WithTimeout`/`WithDeadline` create derived futures. When the deadline wins, the derived future receives a timeout error and cancellation is forwarded to the underlying operation by the helper. This does not prove that remote work stopped. Use `TFutureTimeoutOptions::Error` to wrap timeout/cancellation with operation context and `Invoker` when timeout handling needs explicit affinity.

## Combining futures { #future-combining }

Combiners encode both result and cancellation policy:

- `AllSucceeded` preserves input order, but the first failed input fails the combined future immediately;
- `AllSet` waits for every input and returns each `TErrorOr<T>`, so individual errors are data rather than a shortcut;
- `AnySucceeded` ignores individual failures until one input succeeds, and reports a combined error if all fail;
- `AnySet` returns the first completion, whether value or error;
- `AnySetMatching` returns when its thread-safe, side-effect-free predicate accepts a result, or after all inputs complete without a match;
- `AnyNSucceeded` and `AnyNSet` provide the corresponding N-result forms.

`TFutureCombinerOptions` controls two separate directions. `PropagateCancelationToInput` (true by default) means canceling the **combined output** cancels all still-relevant input futures. `CancelInputOnShortcut` (also true by default) means that once a shortcut determines the output—an `Any*` winner or an `AllSucceeded` failure—the combiner cancels inputs whose results are no longer needed. Disable the first when the inputs are shared with other consumers; disable the second when losing operations must still finish for side effects or cache warming.

Input cancellation is otherwise just an input result. For `AllSucceeded` it is a failure and may shortcut the combiner; for `AllSet` it is collected and the combiner still waits; for `AnySet` it can win as the first set result; for `AnySucceeded` it is accumulated as an error while other inputs may still succeed. Cancellation generated by a shortcut uses a structured combiner-shortcut error, so producers can distinguish lost fan-in races from external cancellation.

Empty-input behavior differs: all-of combiners complete with an empty success, while any-of combiners fail because no winner can exist. Prefer these helpers over hand-written counters so result races, callback unsubscription, and bidirectional cancellation remain consistent.

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

A trace groups related spans under one trace id. Each span has a fresh span id and normally refers to its parent span. The current `TTraceContext` also carries sampling/debug state, request id, target endpoint, logging tag, baggage, trace/profiling tags, allocation tags, and timing/accounting state.

### When identifiers are created { #trace-id-creation }

- `TTraceContext::NewRoot(name)` starts a trace and generates a new trace id unless the caller supplies one. It also generates the root span id.
- `CreateTraceContextFromCurrent(name)` creates a child of the current context when one exists: the child keeps the trace id and gets a new span id. With no current context it creates a new root and therefore a new trace id.
- `GetOrCreateTraceContext(name)` returns the current context unchanged when present; only the no-current-context case creates a new root and trace id. It does **not** create a child span for every call.
- An incoming RPC trace extension supplies the trace id, parent span id, sampled/debug flags, baggage, and target endpoint for a server-side child. If no trace id arrives, tracing can remain absent; when RPC code forces tracing, it creates a new recorded root trace.
- Detached work that is not logically part of a request should create a root rather than a child. Ordinary nested operations should create a child so spans remain in the same trace.

Installing a context with `TCurrentTraceContextGuard` makes it current for the dynamic scope. `TNullTraceContextGuard` temporarily removes it. Creating a context does not make it current by itself.

```cpp
auto traceContext = NTracing::CreateTraceContextFromCurrent("MyOperation");
NTracing::TCurrentTraceContextGuard traceGuard(traceContext);

return asyncStep.Apply(BIND([] (const TStepResult& step) {
    YT_LOG_DEBUG("Step finished");
    return MakeNextRequest(step);
}).AsyncVia(invoker));
```

### Capture points { #trace-capture-points }

Trace data is observed at several different times; confusing these points causes accidental parentage and stale request context:

| Boundary | What is captured or read | When |
| --- | --- | --- |
| `BIND` | The current propagating-storage snapshot, including its `TTraceContextPtr`. | When the `BIND` expression executes, not when the callback is queued or invoked. |
| Callback invocation | The bind-time snapshot is installed for the body and the previous invocation-site context is restored afterward. | On every call. |
| `BIND_NO_PROPAGATE` | Nothing from the bind site; the body sees the invocation site's current context. | At invocation. |
| Child-span creation | Parent span context and inheritable fields from the current/explicit parent; a new span id is generated. | When `CreateChild`/`CreateTraceContextFromCurrent` runs. |
| Logging | Trace id, request id, and logging tag from the then-current context. | At each log call. |
| Outgoing RPC serialization | The trace/span identity, sampled/debug flags, endpoint, and optionally baggage from the context attached to that request. | When RPC tracing metadata is populated for the request. |
| Allocator profiling | Allocation tags reachable from the then-current trace context. | At allocation-hook execution. |

Thus moving a prebuilt callback to another invoker does not recapture the destination's trace. Conversely, constructing `BIND` before installing a guard captures the old context. Put the guard around callback construction when the callback should belong to the new span.

### Propagating storage behind the contract { #propagated-context }

The public semantic contract is the bind-time capture described above. Internally, `BIND` stores the current `TPropagatingStorage`, a fiber-local copy-on-write map keyed by C++ type with type-erased values. Before invoking the callable it installs that snapshot with `TPropagatingStorageGuard`, then restores the previous snapshot. The trace entry is a `TTraceContextPtr`, so callbacks share that context object rather than copying each field separately.

Other subsystems may propagate one value per type with `TPropagatingValueGuard<T>`. Values reachable through a snapshot may therefore be shared and mutable; prefer explicit parameters for business state. Codicil stacks, minimum log level, and thread-message tags use dedicated fiber-local facilities and are not automatically entries in this map.

`BIND_NO_PROPAGATE` omits the snapshot and guard. Scheduler queues commonly have empty ambient storage for detached callbacks, but inline or custom invocation can have a context; “not propagated” must not be interpreted as “guaranteed null”.

A child context inherits request id, target endpoint, logging tag, baggage, profiling tags, and allocation tags from its parent, while retaining a parent link and receiving a new span id. Trace tags and timing data then evolve on the relevant context. When starting long-lived background work, use `BIND_NO_PROPAGATE` and explicitly install a new root if the work needs its own trace.

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
