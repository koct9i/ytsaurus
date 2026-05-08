# User impersonation in HTTP proxy

## Overview

**User impersonation** allows a privileged caller to issue HTTP requests on behalf of another user — the *impersonated user* — without possessing that user's credentials. The proxy authenticates the *real* caller, but all access-control checks and audit records reference the impersonated identity.

This document describes the design of the impersonation mechanism, its authorization model, and its client-side API.

---

## Motivation

Several internal services (such as `yql_agent`) need to execute cluster operations on behalf of the end user who submitted a query. Without impersonation these services would either:

* run all operations under their own service account, losing per-user accounting and ACL enforcement; or
* receive and forward user tokens, which is a security anti-pattern.

Previously impersonation was limited to a **hardcoded whitelist** of service accounts (`yql_agent`). This list was baked into the server binary, which made it impossible to grant impersonation rights to new services without a binary upgrade, and impossible to revoke rights without removing the account from the whitelist and redeploying.

---

## Goals

* Allow any member of the built-in `superusers` group to use impersonation over HTTP.
* Block impersonation for users whose account has been **banned**, even if they are whitelisted or are superusers.
* Preserve backward compatibility: existing deployments where `yql_agent` is not yet a superuser continue to work via the legacy whitelist.
* Expose `impersonation_user` as a first-class configuration option in the Python SDK wrapper.
* Cache user attributes (group membership and ban status) efficiently so that impersonation checks add no measurable latency.

## Non-goals

* Impersonation for the **RPC** protocol is not in scope (the native driver already has a separate `driver_user_name` option).
* Auditing impersonated requests beyond recording `real_login` in the authentication response is not in scope for this feature.
* Exposing impersonation in the Go SDK is deferred to a separate PR.

---

## Background: HTTP authentication flow

Every HTTP request to the proxy carries an `Authorization: OAuth <token>` header. The HTTP authenticator:

1. Resolves the token to a `login` (username) using the configured token authenticator.
2. Optionally inspects the `X-YT-User-Name` header for an impersonated identity.
3. Returns a `TAuthenticationResult` that the rest of the proxy uses for authorization and audit.

Before this feature the check at step 2 was:

```
if login ∈ UserImpersonationWhitelist → allow impersonation
else                                   → reject with InvalidCredentials
```

---

## Design

### Authorization model

A user is allowed to impersonate another user if and only if **all** of the following conditions hold:

| Condition | Source |
|-----------|--------|
| The user is a member of the `superusers` group (transitive closure) **or** is in the legacy whitelist | `TUserAttributeCache` |
| The user's `@banned` attribute is `false` | `TUserAttributeCache` |

The logical expression evaluated by the authenticator:

```
allowed = !banned && (isSuperuser || isWhitelisted)
```

The whitelist is retained for backward compatibility only. It should be removed once all deployments have added the relevant service accounts to `superusers`.

### TUserAttributeCache

A new cache, `TUserAttributeCache`, is added to the native connection (`NApi::NNative::IConnection`). It is modeled after the existing `TPermissionCache` and stores a `TUserAttributes` struct for each username:

```
TUserAttributes {
    bool                      Banned;
    THashSet<std::string>     MemberOfClosure;   // transitive group membership
}
```

The cache fetches the `@banned` and `@member_of_closure` Cypress attributes of the user node (`//sys/users/<name>`) in a single batched request. It is built on top of the generic `TObjectAttributeAsYsonStructCacheBase` infrastructure and therefore inherits:

* configurable TTL and refresh policy;
* background proactive refresh;
* per-item profiling;
* safe concurrent access.

The cache is exposed on the connection interface as `GetUserAttributeCache()` and is cleared together with the permission cache in `ClearCaches()`.

Two free functions are provided for consumers:

```cpp
// Returns true iff the user is a member of the "superusers" group.
TFuture<bool> IsSuperuser(const IConnectionPtr& connection, const std::string& user);

// Returns true iff the user's @banned attribute is set.
TFuture<bool> IsUserBanned(const IConnectionPtr& connection, const std::string& user);
```

### HTTP authenticator changes

`THttpAuthenticator::Authenticate` is updated as follows:

1. After resolving the token to `authenticationResult.Login`, check whether `X-YT-User-Name` is present.
2. If it is, resolve both `IsSuperuser` and `IsUserBanned` from the cache (using `WaitFor` / `WaitForFast` — the ban check almost always hits the cache).
3. Apply the authorization expression above.
4. On success: set `authenticationResult.Login` to the impersonated username, append `:impersonation` to `Realm`, and store the original login in `authenticationResult.RealLogin`.
5. On failure: return `NRpc::EErrorCode::InvalidCredentials` with diagnostic attributes `is_superuser`, `is_banned`, `is_whitelisted`.

Error message:

```
Client has provided X-YT-User-Name header but authenticated user <user> is not whitelisted, or a superuser (or is banned)
```

### TAuthenticationResult changes

Two fields are added to `TAuthenticationResult`:

| Field | Type | Meaning |
|-------|------|---------|
| `Login` | `std::string` | **Effective** login. Equals the impersonated user when impersonation was performed. |
| `RealLogin` | `std::optional<std::string>` | Set to the **original** authenticated login only when impersonation was performed; absent otherwise. |

A helper `GetRealLogin(result)` returns `RealLogin` if set, or `Login` otherwise, simplifying audit logging.

The `/auth` endpoint response now includes a `real_login` field:

```json
{
  "login": "alice",
  "realm": "blackbox:impersonation",
  "real_login": "yql_agent",
  "csrf_token": "..."
}
```

### Configuration

`TConnectionDynamicConfig` gains a new sub-config:

```yaml
user_attribute_cache:
  expire_after_successful_update_time: 300s   # default, configurable
  expire_after_failed_update_time:     60s    # default, configurable
  refresh_time:                        60s    # proactive refresh interval
```

The config key is `user_attribute_cache` and is of type `TUserAttributeCacheConfig`, which extends `TAsyncExpiringCacheConfig`.

CHYT uses a dedicated service account (`YT_CHYT_CACHE_USER`) to populate attribute caches. The `TableAttributeCache` in CHYT is configured to use this account (`config->TableAttributeCache->UserName = CacheUserName`) to ensure consistent caching behavior.

---

## Python SDK

### New configuration option

A new top-level option `impersonation_user` is added to the Python wrapper default config:

```python
"impersonation_user": None,
```

When set, every HTTP request adds the `X-YT-User-Name` header with the specified value. This applies both to regular API calls (via `http_driver.py`) and to the `/auth/whoami` endpoint (`http_helpers.py`).

The option is **HTTP-only**. With the RPC driver it is silently ignored (users should use `driver_user_name` instead). With the native C++ driver, use `driver_user_name`.

### Usage example

```python
import yt.wrapper as yt
from yt.common import update

# Create a client that will authenticate as 'root_service' but
# perform all operations as 'alice'.
client = yt.YtClient(
    config=update(yt.config.config, {"impersonation_user": "alice"})
)

# The authenticated user must be a superuser and must not be banned.
assert client.get_user_name() == "alice"

node = "//tmp/created_by_alice"
client.create("map_node", node)
assert client.get(node + "/@owner") == "alice"
```

### `get_user_name()` with impersonation

When `impersonation_user` is set, `get_user_name()` returns the **impersonated** user name because the `X-YT-User-Name` header is forwarded to the `/auth/whoami` endpoint. This is the intended behavior: from the cluster's perspective, operations are performed by the impersonated user.

---

## Security considerations

### Why require superuser membership?

Impersonation is a powerful capability: it can be used to act as `root`. Requiring `superusers` membership gives administrators a single, well-known place to manage who can impersonate. `superusers` is always managed at the cluster level and its membership is audited.

### Why also check the ban flag?

Banning a user is the standard emergency response when credentials are suspected to be compromised. Without the ban check, a banned superuser could still impersonate others. The ban check closes this gap.

### Legacy whitelist

The whitelist (`yql_agent`) is kept for backward compatibility. It is subject to the same ban check as superusers — a banned whitelisted user cannot impersonate. The whitelist will be removed in a future release once all clusters have migrated `yql_agent` (and similar services) to the `superusers` group.

### Cache TTL and consistency

The `TUserAttributeCache` has a positive TTL (default: 5 minutes). This means there is a window during which:

* a newly-banned superuser may still be allowed to impersonate;
* a user removed from `superusers` may still be allowed to impersonate.

This is acceptable because:
1. The TTL is configurable and can be reduced for security-sensitive deployments.
2. For the ban case, the window is bounded and the Cypress change is the authoritative signal.
3. The `ClearCaches()` API can be called to force an immediate refresh if needed.

---

## Testing

### Integration tests (`test_http_proxy.py`)

The integration test `TestHttpProxy::test_user_impersonation` covers:

1. `yql_agent` (whitelisted) can impersonate before changes.
2. A normal user **cannot** impersonate after gaining a token but before being added to `superusers`.
3. After `add_member("test_user", "superusers")`, the user **can** impersonate.
4. Banning a whitelisted user (`yql_agent`) removes their ability to impersonate.
5. Banning a superuser removes their ability to impersonate.

### Python wrapper tests (`test_authentication.py`)

The `TestImpersonation` test class covers:

1. A superuser can impersonate: `get_user_name()` returns the impersonated name and created objects are owned by the impersonated user.
2. A non-superuser cannot impersonate (raises `YtError`).
3. After being added to `superusers` (with cache propagation), the previously-blocked user can impersonate.
4. After being banned, the superuser can no longer impersonate (with cache propagation).

---

## Backward compatibility

* The legacy whitelist is preserved. Existing deployments where `yql_agent` is not a superuser continue to work without any configuration changes.
* The new `RealLogin` field in `TAuthenticationResult` is optional (`std::optional`). Code that does not need impersonation audit information is unaffected.
* The `real_login` field in the `/auth` JSON response is always present (set to `login` when no impersonation occurred, via `GetRealLogin`).
* The new `user_attribute_cache` sub-config has sensible defaults and requires no operator action on upgrade.

---

## Future work

* **Remove the legacy whitelist** once all production clusters have migrated relevant service accounts to `superusers`.
* **Go SDK support**: add `ImpersonationUser` to the Go HTTP client config (tracked separately).
* **Audit logging**: emit a structured log entry for every impersonated request, including `real_login`, `impersonated_login`, and the request path.
* **RPC driver impersonation**: align the RPC protocol so that `impersonation_user` works uniformly across all drivers.
