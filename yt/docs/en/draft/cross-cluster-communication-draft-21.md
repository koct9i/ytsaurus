---
type: Draft Article
title: "Draft-21: Configuring cross-cluster communication"
last_modified: 2026-08-21T00:00:00Z
tags: [cross-cluster, configuration, networking]
status: draft
---

# Configuring cross-cluster communication

{% note warning %}

This article is a draft. Configuration names and the operational procedure must be
verified against the {{product-name}} version deployed on each cluster.

{% endnote %}

This article explains how native {{product-name}} components learn about other
clusters, where cluster connection configurations are stored in Cypress, and how
to find HTTP entry points for a connected cluster.

## Terminology

A **cluster connection** is the native-client configuration required to reach a
cluster's masters and supporting services. A **cluster directory** is a mapping
from cluster names to cluster connections. A cluster is *connected* when its
connection is present in the directory used by the component that needs to make
the cross-cluster request.

Cluster connections describe native RPC connectivity. They are not a directory
of user-facing HTTP proxy URLs. Discover HTTP entry points separately after a
native connection to the target cluster has been established.

## Cluster connection in Cypress

The connection published by the local cluster is stored in the
`cluster_connection` attribute of the `//sys` node:

```bash
yt get //sys/@cluster_connection
```

The value normally contains the primary and secondary master cell descriptions
and native-client settings for services such as timestamp providers, clocks, and
discovery servers. The exact fields depend on the deployment and server version.
Treat the complete document as a versioned connection configuration: copy it
first, then make only intentional patches such as network-specific addresses or
transport security settings.

The cluster directory visible from the local cluster is stored at
`//sys/clusters`. Its keys are cluster names and its values are cluster connection
documents:

```bash
yt get //sys/clusters
yt get //sys/clusters/remote-cluster
```

The two locations have different purposes:

* `//sys/@cluster_connection` publishes the local cluster's own connection.
* `//sys/clusters/<name>` tells native clients how to connect to `<name>`.

Consequently, changing `//sys/@cluster_connection` alone does not connect two
clusters. The published connection must also reach every cluster directory from
which clients need to find it.

## Establishing connectivity

The following example connects `cluster-a` to `cluster-b`. Run the commands with
an administrative account and keep backups of the original values.

1. Export the connection published by `cluster-b`:

   ```bash
   yt --proxy cluster-b get //sys/@cluster_connection > cluster-b.yson
   ```

2. Review the exported document. In particular, check that addresses are
   reachable from `cluster-a`, that the selected network names exist on both
   sides, and that TLS or mTLS client settings refer to files installed on
   `cluster-a` hosts.

3. Add the document to the directory on `cluster-a`:

   ```bash
   yt --proxy cluster-a set //sys/clusters/cluster-b < cluster-b.yson
   ```

4. If communication must be bidirectional, repeat the procedure in the other
   direction. A directory entry is directional: adding `cluster-b` on
   `cluster-a` does not add `cluster-a` on `cluster-b`.

5. Wait for the cluster-directory synchronizers of the relevant components to
   refresh. Validate the effective connection in the component's Orchid, not
   only the Cypress source. For example, inspect
   `//sys/http_proxies/<proxy>/orchid/cluster_connection` for an HTTP proxy.

Components periodically fetch the directory through `GetClusterMeta`. Therefore,
updates are not necessarily visible immediately. Moreover, a component can be
configured to use its static cluster connection, the directory value, or a
directory value patched by static configuration. Consult the component's
`cluster_connection_dynamic_config_mode` and its effective Orchid configuration
when Cypress and runtime behavior differ.

{% note warning %}

Do not replace the entire `//sys/clusters` map merely to add one cluster. A stale
full-map write can accidentally remove other connections. Prefer a write to the
individual `//sys/clusters/<name>` child and retain a backup for rollback.

{% endnote %}

## Cross-cluster settings checklist

Before enabling production traffic, verify the following settings.

### Identity and topology

* The directory key is the cluster name or alias expected by operations and rich
  YPaths such as `remote-cluster://path/to/table`.
* If the connection has no `cluster_name`, the native cluster directory supplies
  the directory key as its name. If `cluster_name` is present, avoid a surprising
  mismatch unless the directory key is deliberately an alias.
* Master cell IDs and cell tags are correct and do not collide across federated
  clusters.
* Primary and secondary master lists are current.

### Networks and name resolution

* Every advertised hostname resolves from the source cluster's servers and job
  environment, where applicable.
* The requested `network_name` is advertised by the target components.
* Firewalls allow the native RPC, discovery, timestamp-provider, and data-node
  traffic required by the workload.
* Inter-cluster throttlers and bandwidth limits are configured for remote reads
  and RemoteCopy operations.

### Authentication and encryption

* The same user identity has the required permissions on the target cluster.
* Token, service-ticket, or other credentials can be delegated across the
  boundary where the selected feature requires it.
* For TLS or mTLS, CA files, client certificates, and private keys exist on every
  component host that creates the remote connection.
* Certificate names match the advertised hostnames, and the certificate-rotation
  procedure is tested for every component that creates a remote connection.

### Time and availability

* Clock or timestamp-provider settings are suitable for the cross-cluster
  feature. Do not combine incompatible timestamp domains for operations that
  require comparable timestamps.
* Synchronization periods and expiration times balance prompt updates against
  control-plane load.
* Both clusters have compatible server versions for the feature being enabled.

## Discovering HTTP entry points

HTTP proxies register themselves below `//sys/http_proxies` on their own cluster.
The registry contains liveness state and an `addresses` attribute grouped first
by listener type and then by network name. Common listener types include `http`,
`https`, and `monitoring_http`; a deployment may expose additional types.

There are two discovery approaches. The first is the application-facing API; the
second is an administrative diagnostic. Neither means that HTTP proxy addresses
are embedded in `//sys/@cluster_connection`.

### Use the `discover_proxies` command

Prefer the API v4 `discover_proxies` command. Execute it through a driver backed
by the **target** native connection and specify:

* `type = "http"`;
* the required `network_name`;
* the required `address_type`, for example `http` or `https`;
* an HTTP proxy `role` if the deployment separates traffic by role.

Conceptually, the request parameters are:

```yson
{
    type = "http";
    address_type = "https";
    network_name = "backbone";
    role = "data";
}
```

`discover_proxies` belongs to the driver command API, while the cluster directory
provides native connections. The exact SDK call therefore depends on how the
application constructs its driver. The invariant is that the driver must use the
connection returned for `remote-cluster`; a caller must not run the command on its
local connection and then label the result as remote.

Discovery filters out dead and banned proxies and applies the requested role. If
balancers are configured for the role, discovery can return their addresses
instead of individual proxies. This is usually desirable for applications. Use
`ignore_balancers = %true` only for diagnostics or when a caller explicitly needs
individual proxy addresses.

The important bootstrap order for a connected cluster is:

1. Look up `//sys/clusters/<target>` through the local cluster directory.
2. Create a native connection from that document.
3. Run `discover_proxies` on the target connection with `type = "http"`.
4. Construct URLs with the scheme matching `address_type`; the returned values
   are addresses, not necessarily complete URLs.

Do not send `discover_proxies` to the source cluster and assume that it returns
the target cluster's proxies. Unless an explicitly configured gateway or
multiproxy feature says otherwise, discovery describes the cluster on which the
command executes.

### Inspect the target proxy registry

For troubleshooting, use a native client connected to the target cluster and
inspect the registry directly. In the following commands, `<target-proxy>` is an
already known bootstrap endpoint for the target cluster; it is not the name of
the source cluster:

```bash
yt --proxy <target-proxy> get //sys/http_proxies
yt --proxy <target-proxy> get //sys/http_proxies/<proxy>/@addresses
yt --proxy <target-proxy> get //sys/http_proxies/<proxy>/@banned
yt --proxy <target-proxy> get //sys/http_proxies/<proxy>/@role
```

This registry method is therefore useful for verifying discovery, not for solving
the initial HTTP bootstrap problem by itself. When no target HTTP endpoint is
known, start with the native connection from `//sys/clusters/<target>` and the
driver command described above.

A representative address map looks like this:

```yson
{
    http = {
        default = "proxy-1.example.net:80";
        backbone = "proxy-1.backbone.example.net:80";
    };
    https = {
        default = "proxy-1.example.net:443";
    };
    monitoring_http = {
        default = "proxy-1.example.net:10080";
    };
}
```

Only select entries that are alive, not banned, have the required role, and
advertise the requested listener and network. Reimplementing all these rules is
error-prone, which is why applications should use `discover_proxies` rather than
parsing `//sys/http_proxies` themselves.

The legacy HTTP `/hosts` endpoint also discovers HTTP proxies, but new tooling
should use the API v4 command because it supports explicit proxy types, listener
address types, network names, roles, and balancer-aware discovery.

## Validation and troubleshooting

Use this sequence to isolate failures:

1. Confirm that `//sys/clusters/<target>` exists on the directory cluster.
2. Compare it with `//sys/@cluster_connection` on the target cluster.
3. Inspect the consuming component's Orchid and wait at least one configured
   synchronization period.
4. Test DNS resolution and TCP connectivity from the consuming component's host,
   not from an administrator's workstation.
5. Run HTTP proxy discovery for each required `(role, address_type,
   network_name)` tuple.
6. Check `//sys/http_proxies` for missing `alive` children, `@banned = %true`,
   role mismatches, or absent address types and networks.
7. Test `/ping` on one returned HTTP or HTTPS address, using the expected TLS
   trust roots.

Typical symptoms have distinct causes:

| Symptom | Likely cause |
| --- | --- |
| `Unknown cluster` | Missing directory entry, wrong name, or a synchronizer that has not refreshed. |
| Native connection timeout | Unreachable master/discovery address, firewall, DNS, or transport-security mismatch. |
| Discovery returns an empty list | Wrong role, address type, or network; all matching proxies are dead or banned. |
| Returned names do not resolve | Proxies advertise an internal network; request the correct network or fix proxy `addresses`. |
| Light HTTP requests work but streaming requests fail | The balancer is reachable but the directly discovered data proxies are not. |
| Cypress value and runtime behavior differ | Static policy or static patch overrides the directory, or the runtime cache is stale. |

## Operational recommendations

* Manage directory entries as configuration artifacts and review diffs before
  applying them.
* Roll out one direction and one workload at a time.
* Monitor cluster-directory synchronization errors and remote-connection health.
* Discover proxies periodically; do not persist individual proxy addresses as
  permanent application configuration.
* Remove a directory entry only after all dependent operations, replicas, queues,
  and services have stopped using it.
