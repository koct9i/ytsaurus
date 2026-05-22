# General information

This section describes the general access control model for tables and other [Cypress](../../../user-guide/storage/cypress.md) nodes, as well as for other system objects such as users, groups, accounts, chunks, and transactions.

Any request in {{product-name}} goes through two consecutive stages:

1. **Authentication.** A proxy verifies the user identity based on the token passed in the request. The token is typically issued via [OAuth](https://{{lang}}.wikipedia.org/wiki/OAuth). If the request does not contain a token, the request is treated as coming from the `guest` user. After authentication, the token is no longer used during further request processing. For details, see [Authentication](../../../user-guide/storage/auth.md).
2. **Authorization.** The Cypress master server checks whether the authenticated user is allowed to perform the requested action on the target object.

The authorization result depends on three inputs:

1. The user who initiated the request.
2. The requested permission, such as `read` or `write`.
3. The object to which access is requested.

If access is denied, {{product-name}} returns an error that identifies the user, the object, and the permission that caused the denial.

If a proxy sends a request on behalf of a non-existent user, the master server returns the `No such user` error. This situation is possible because token issuance and user registration are independent procedures performed by different services.

## Key concepts

- **Subject** is a user or a group.
- **Object** is an entity for which permissions are checked: a Cypress node, account, transaction, operation, and so on.
- **Access control descriptor** (**ACD**) is the set of object attributes that define access rules: `@acl`, `@inherit_acl`, and `@owner`.
- **Access control list** (**ACL**) is the value stored in `@acl`.
- **Access control entry** (**ACE**) is an individual rule inside an ACL.

This article covers general ACLs. For more specific mechanisms, see [Managing access to table columns](../../../user-guide/storage/columnar-acl.md) and [Row-level security](../../../user-guide/storage/row-level-security.md).

## Users and groups { #users_groups }

{{product-name}} stores the subject registry in Cypress:

- All users are stored in `//sys/users`, which is a `user_map` node.
- All groups are stored in `//sys/groups`, which is a `group_map` node.

{% note warning "Attention" %}

Users and groups share one namespace. A user name and a group name cannot be the same.

{% endnote %}

Group membership is transitive. A group may contain users and other groups, while cyclic membership is forbidden. As a result, if user `A` belongs to group `B`, and group `B` belongs to group `C`, then user `A` is considered a member of group `C` during authorization.

#### How to view the user and group lists

{% list tabs %}

- In the web interface

  - To view users, open **Users** in the side panel or go to `{{cluster-ui}}/users`.
  - To view groups, open **Groups** or go to `{{cluster-ui}}/groups`.

- Via the CLI

  - List users:
    ```bash
    yt --proxy <cluster-name> list //sys/users
    ```
  - List groups:
    ```bash
    yt --proxy <cluster-name> list //sys/groups
    ```

{% endlist %}

{% note warning %}

The `//sys/users` and `//sys/groups` nodes are not tables, so they cannot be queried with `SELECT`.

{% endnote %}

Users have the `user` system object type. Groups have the `group` type.

#### How to inspect a user or group

{% list tabs %}

- In the web interface

  Open the page of the user or group and click ![](../../../../images/attrs-icon.png =20x20) on the right.

- Via the CLI

  - Get one user attribute:
    ```bash
    $ yt --proxy <cluster-name> get //sys/users/john/@type
    "user"
    ```

  - List all user attributes:
    ```bash
    $ yt --proxy <cluster-name> get //sys/users/john/@
    {
      "id" = "ffffffff-fffffffe-101f5-1407420e";
      "type" = "user";
      "builtin" = %true;
      ...
    }
    ```

  - Get one group attribute:
    ```bash
    $ yt --proxy <cluster-name> get //sys/groups/admins/@type
    "group"
    ```

{% endlist %}
