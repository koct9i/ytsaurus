## System subjects { #system_subjects }

{{product-name}} includes built-in subjects that are required for system operation. These subjects cannot be removed:

1. Users: `guest`, `root`, `scheduler`, and `job`.
2. Groups: `everyone`, `users`, and `superusers`.

## Subject attributes { #subject_attributes }

The following attributes are available for every subject.

| **Attribute** | **Type** | **Description** |
| ------------------- | --------------- | ------------------------------------------------------------ |
| `name` | `string` | Subject name. |
| `member_of` | `array<string>` | Groups to which the subject belongs directly. |
| `member_of_closure` | `array<string>` | Groups to which the subject belongs directly or indirectly. |
| `aliases` | `array<string>` | Alternative names that can be referenced from ACLs. |

## User attributes { #user_attributes }

In addition to common subject attributes, users have the attributes below.

| **Attribute** | **Type** | **Description** | **Mandatory** |
| -------------------------- | --------------- | ------------------------------------------------------------ | ------------------- |
| `banned` | `bool` | Whether the user is blocked. | No |
| `access_time` | `DateTime` | Time of the last request from the user. | Yes |
| `access_counter` | `integer` | Total number of requests made by the user. | Yes |
| `request_rate` | `double` | Current request rate in requests per second. | Yes |
| `request_rate_limit` | `double` | Limit for requests per second. The default value is 100. | Yes |
| `request_queue_size_limit` | `double` | Limit for the request queue size. The default value is 100. | Yes |
| `usable_accounts` | `array<string>` | Accounts that the user is allowed to use. | Yes |

## Group attributes { #group_attributes }

In addition to common subject attributes, groups have the attributes below.

| **Attribute** | **Type** | **Description** |
| ----------- | --------------- | --------------------------------------------------------- |
| `members` | `array<string>` | Group members: users and other groups. |

## Managing groups { #group_control }

{% note info "Note" %}

On large clusters, direct user and group management is usually restricted to {{product-name}} administrators.

{% endnote %}

To create a group, use the `create` command and set the `name` attribute:

```bash
yt create group --attributes '{name=my_group}'
```

To add a user or another group to a group, use `add-member`:

```bash
yt add-member my_user my_group
```

To remove a member from a group, use `remove-member`:

```bash
yt remove-member my_user my_group
```

To delete a group, remove the corresponding node from `//sys/groups`:

```bash
yt remove //sys/groups/my_group
```

When a group is deleted, it is removed automatically from all ACLs in which it is referenced.

## Authorization { #authorization }

The authorization module on the master server answers the following question: should user `U` be granted permission `P` for object `O`?

- `U` is a user account. Groups cannot act as request initiators, but group membership is considered during permission checks.
- `P` is a permission.
- `O` is a system object, for example a Cypress node, account, transaction, operation, user, or group.

If `U` is `root`, the request is always allowed.

The permissions supported by the general ACL model are listed below.

| Permission | Description |
| ------------ | ------------------------------------------------------------ |
| `read` | Read object data or attributes. |
| `write` | Modify object state or attributes. |
| `use` | Use an account, pool, or bundle. |
| `administer` | Modify the object access control descriptor. |
| `create` | Create objects of a given schema type. |
| `remove` | Remove an object. |
| `mount` | Mount, unmount, remount, or reshard a dynamic table. |
| `manage` | Manage an operation or its jobs. |

To make a decision, {{product-name}} builds an **effective ACL** for object `O`. The effective ACL is the combination of:

- the ACL defined directly on the object;
- inherited ACEs from parent objects, if `@inherit_acl` is `%true`.

The order of ACEs in an ACL does not matter.

Each ACE has the structure below.

| **Attribute** | **Type** | **Description** |
| ------------------ | ------------------- | ------------------------------------------------------------ |
| `action` | `SecurityAction` | `allow` or `deny`. |
| `subjects` | `array<string>` | Subjects to which the ACE applies. |
| `permissions` | `array<Permission>` | Permissions covered by the ACE. |
| `inheritance_mode` | `InheritanceMode` | ACE inheritance mode. The default value is `object_and_descendants`. |

`inheritance_mode` controls where an ACE applies.

| **Value** | **Effect** |
| --------- | ---------- |
| `object_only` | Applies only to the current object. |
| `object_and_descendants` | Applies to the current object and all descendants. |
| `descendants_only` | Applies only to descendants, including indirect ones. |
| `immediate_descendants_only` | Applies only to direct children. |

An ACE is considered applicable to `U` and `P` if:

1. `P` is listed in `permissions`.
2. `subjects` contains either user `U` or a group to which `U` belongs directly or indirectly.

After the effective ACL is built, the result is determined as follows:

1. Access is granted if there is at least one applicable `allow` ACE and no applicable `deny` ACE.
2. In all other cases, access is denied.

This means that an empty effective ACL denies access by default.

## Common ACL tasks { #acl_tasks }

Use the object attributes below to inspect the access control descriptor:

```bash
yt get //home/project/@acl
yt get //home/project/@inherit_acl
yt get //home/project/@owner
```

To append an ACE to an existing ACL, use the special `end` element:

```bash
yt set //home/project/@acl/end '{action=allow; subjects=[analysts]; permissions=[read]}'
```

To replace the entire ACL, set the full list explicitly:

```bash
yt set //home/project/@acl '[{action=allow; subjects=[analysts]; permissions=[read]}; {action=allow; subjects=[admins]; permissions=[read; write; administer; remove]}]'
```

To stop inheriting ACLs from parent objects, set `@inherit_acl` to `%false`:

```bash
yt set //home/project/@inherit_acl %false
```

To check whether a user has a permission for a Cypress node, use `check-permission`:

```bash
yt check-permission <user_name> <permission> <path>
```

Example:

```bash
$ yt check-permission pavel-kulenov write //tmp
{
  "action" = "allow";
  "object_id" = "1-3-411012f-1888ce1f";
  "object_name" = "node //tmp";
  "subject_id" = "c4-8aaa-41101f6-bec6113b";
  "subject_name" = "YTsaurus";
}
```

## Object owner and the `owner` subject { #owner}

When user `U` creates an object, `U` becomes its owner. The owner name is stored in the `@owner` attribute.

{% note info "Note" %}

Only a superuser, that is, a member of the `superusers` group, can change the owner.

{% endnote %}

{{product-name}} also provides a special pseudo-subject named `owner`. You cannot authenticate as `owner`, but you can reference it in ACLs. During authorization, `owner` is substituted with the actual object owner.

This subject is useful for rules such as “only the creator may remove objects in this directory”:

```bash
{action=allow; permissions=[remove]; subjects=[owner]; inheritance_mode=descendants_only}
```

In scenarios like this, disable ACL inheritance if parent rules must not affect descendants:

```bash
yt set //home/project/@inherit_acl %false
```

## Managing operations

Operations have ACLs just like Cypress nodes. You can inspect an operation ACL in the `runtime_parameters/acl` attribute.

The main operation permissions have the following meaning:

- `read` allows access to live preview data, intermediate data, stderr, fail context, and job input data.
- `manage` allows state-changing actions for operations and jobs, for example `Abort`, `Complete`, `Suspend`, `Resume`, `Abandon`, and `Send signal`.
- [Job Shell](../../../user-guide/problems/jobshell-and-slowjobs.md) requires both `read` and `manage`, because shell access allows both observation and state changes.

You can provide an ACL for an operation when starting it:

```python
yt.run_map(..., spec={"acl": [{
 "action": "allow",
 "subjects": ["u1", "u2"],
 "permissions": ["read", "manage"],
}]})
```

The user who starts the operation and the default administrators are added to the resulting operation ACL automatically.

## How to use ACLs correctly { #acl_usage}

Use ACLs at the highest practical level in the Cypress tree and rely on inheritance for descendants. This keeps the access model predictable and reduces repetitive rules on individual tables.

Follow these recommendations:

1. Grant permissions to groups rather than to individual users.
2. Use `deny` ACEs only as an exception, for example for urgent mitigation. Afterwards, revise the ACL structure and return to an allow-based model whenever possible.
3. Disable ACL inheritance only where the access model changes substantially.
4. Validate important changes with `check-permission` before applying them to production paths.

For security-sensitive subtrees, such as directories with tokens or other confidential metadata, define access rules explicitly and do not rely on defaults inherited from higher-level nodes.

## Requesting access { #request_access }

To obtain access to an existing account or directory in {{product-name}}, {% if audience == "public" %}contact your system administrator{% else %}see [Configuring access to data](../../../user-guide/storage/acl-manage.md){% endif %}.
