<!--
Draft number: 16
Author: AI agent (OpenAI)
Created: 2026-06-10
Status: In progress
Target: user-guide/storage/cypress-local-filesystem-format
-->

# Local filesystem representation of Cypress trees

This draft describes a simple local format for exporting a Cypress subtree into a regular filesystem directory and uploading it back to Cypress. The goal is to keep each Cypress object in the most native representation available on the local machine:

- Cypress map nodes are represented as directories.
- Cypress attributes are represented as small sidecar files.
- Cypress tables are represented as data files in a table format selected by file extension.
- Cypress files are represented as ordinary files, preserving their byte content.
- Names visible in Cypress are preserved as local filenames as much as possible.

The format is intended for backup, review, editing, testing, and moving small or medium Cypress subtrees between clusters. It is not intended to replace native chunk-level backup mechanisms for very large production datasets.

## Basic layout

A downloaded Cypress subtree is stored in a local directory called the export root. Each Cypress child node becomes one local entry in the corresponding directory.

```text
local-export/
  @.yson
  config/
    service.yaml
    service@.yson
  config@.yson
  logs.parquet
  logs@.yson
  payload.bin
  payload@.json
```

In this example:

- `local-export/` represents the downloaded Cypress root.
- `@.yson` stores attributes of the downloaded root node.
- `config/` represents a Cypress map node named `config`.
- `config@.yson` stores attributes of the `config` map node.
- `config/service.yaml` represents a Cypress document or file named `service` serialized as YAML.
- `logs.parquet` represents a Cypress table named `logs` serialized as Parquet.
- `payload.bin` represents a Cypress file named `payload` stored as raw bytes.

## Node names and extensions

For payload-bearing nodes, the local filename is:

```text
<cypress-name>.<payload-format-extension>
```

The extension describes the local payload format and is not part of the Cypress node name. On upload, the uploader strips one recognized payload extension from the local filename to recover the Cypress name.

Examples:

| Cypress node | Cypress type | Local file | Meaning |
|--------------|--------------|------------|---------|
| `events` | table | `events.parquet` | Table rows in Parquet format |
| `events` | table | `events.yson` | Table rows in YSON list-fragment format |
| `readme` | file | `readme.txt` | File bytes interpreted as text by humans, but uploaded as a Cypress file |
| `archive` | file | `archive.bin` | Opaque file bytes |
| `settings` | document | `settings.json` | Document value in JSON format |

If the original Cypress name already contains dots, only the final recognized payload extension is removed. For example, the Cypress table `events.2026` downloaded as Parquet becomes `events.2026.parquet`.

If two Cypress children would map to the same local filename, the downloader must fail by default. Implementations may provide an escaping mode later, but the simple format should prefer explicit failure over surprising renames.

## Attributes

Attributes are stored in sidecar files next to the node payload:

```text
<cypress-name>@.<attribute-format-extension>
```

For the export root, attributes are stored as:

```text
@.<attribute-format-extension>
```

Supported attribute sidecar extensions are:

| Extension | Attribute format | Notes |
|-----------|------------------|-------|
| `.yson` | YSON node | Preferred lossless format for arbitrary Cypress attributes |
| `.json` | JSON object | Convenient for tools that do not understand YSON; cannot represent all YSON scalars losslessly |
| `.yaml` | YAML mapping | Convenient for human editing; should be used only when the attribute values are simple enough |

For a table named `events`, the data may be stored as `events.parquet` and the attributes as `events@.yson`. For a map node named `config`, the directory is `config/` and the sidecar is `config@.yson` in the parent directory.

The sidecar file contains only user-visible attributes that should be restored on upload. System-maintained attributes such as object id, revision, resource usage, chunk ids, and locks should not be restored by default.

## Persisting the local representation format

When a local tree is uploaded to Cypress, the uploader should store the selected local representation as Cypress attributes. This makes a later recursive download able to reproduce the same local shape and formats without requiring the user to repeat all format options.

Recommended attribute:

```yson
local_filesystem_format = {
    version = 1;
    node_format = "directory";
    attributes_format = "yson";
    payload_format = "parquet";
}
```

The attribute should be written on every uploaded node whose local representation is not fully implied by the node type, and may be written on all uploaded nodes for simplicity. The downloader should use this attribute as a hint, not as an absolute requirement: explicit command-line options may override it.

For tables, `payload_format` records the table data format used locally. For attributes, `attributes_format` records whether the sidecar was `yson`, `json`, or `yaml`. For files, `payload_format` records the local file extension used for the bytes, for example `bin`, `txt`, or `json`.

## Tables

Tables should be downloaded using a format that is natural for the data and common local tooling. The file extension selects the table format.

Recommended table extensions:

| Extension | Table format | Best for |
|-----------|--------------|----------|
| `.parquet` | Apache Parquet | Default for schematized analytical tables; compact, typed, widely supported |
| `.orc` | Apache ORC | Interoperability with Hadoop/Hive ecosystems |
| `.yson` | YSON list fragment | Lossless {{product-name}} representation; nested values and YSON-specific types |
| `.jsonl` or `.ndjson` | Newline-delimited JSON | Logs, simple row objects, integration with common command-line tools |
| `.json` | JSON array or JSON stream | Small tables and examples |
| `.csv` | Comma-separated values | Flat tables with scalar columns and a header row |
| `.tsv` | Tab-separated values | Flat tables where values often contain commas |
| `.dsv` | Delimiter-separated values | Compatibility with delimiter-separated {{product-name}} workflows |
| `.skiff` | Skiff | Typed internal or performance-sensitive transfers when schema is available |
| `.protobuf` | Protobuf | Pipelines that already use protobuf schemas |

Suggested defaults:

1. Use `.parquet` for schematized static tables when all column types are representable.
2. Use `.yson` when losslessness is more important than external tooling compatibility.
3. Use `.jsonl` for schemaless or log-like tables that need easy local inspection.
4. Use `.csv` or `.tsv` only for flat scalar tables.

The table schema should be preserved in the attribute sidecar. For formats that do not carry a full {{product-name}} schema, such as CSV and JSONL, the schema in `name@.yson` is required for faithful upload.

## Files

Cypress file nodes should be stored as ordinary files with the original bytes unchanged. The extension is only a local hint for tools and for recursive download.

Common file extensions:

| Extension | Use |
|-----------|-----|
| `.bin` | Default for opaque binary files |
| `.txt` | UTF-8 or human-readable text |
| `.json` | JSON file content |
| `.yaml` or `.yml` | YAML file content |
| `.yson` | YSON file content |
| `.csv`, `.tsv`, `.parquet`, `.orc` | File objects that already contain data in those formats and should remain Cypress files, not tables |

The uploader must know from the local metadata or explicit user options whether `data.parquet` is a Cypress table encoded as Parquet or a Cypress file whose bytes happen to be Parquet. If this is ambiguous, it should require an explicit choice instead of guessing.

## Documents and scalar nodes

Cypress document nodes and scalar-valued nodes should be stored as one payload file using one of:

| Extension | Format |
|-----------|--------|
| `.yson` | Preferred lossless representation |
| `.json` | Common tooling representation |
| `.yaml` or `.yml` | Human-editable representation |

Attributes, if present, still go into the sidecar file. The payload file contains the node value only.

## Upload rules

An uploader should process a local directory as follows:

1. Read the root attribute sidecar, if present.
2. For each regular entry that is not an attribute sidecar, determine the Cypress node name by stripping one recognized payload extension.
3. Determine the Cypress node type from explicit options, stored `local_filesystem_format` metadata, or the payload extension.
4. Create or replace the Cypress node.
5. Upload the payload in the selected format.
6. Apply attributes from the sidecar file.
7. Store or update `local_filesystem_format` so that a later recursive download can reproduce the same local representation.
8. Recurse into directories as Cypress map nodes.

Attribute sidecars are matched by exact Cypress name, not by payload filename. For example, `events.parquet` is matched with `events@.yson`, not with `events.parquet@.yson`, unless the Cypress node name is actually `events.parquet`.

## Download rules

A downloader should process a Cypress subtree as follows:

1. Create a local directory for every Cypress map node.
2. Choose an attribute sidecar format, preferably from `local_filesystem_format.attributes_format`, otherwise from user options, otherwise `.yson`.
3. Write node attributes into the sidecar file.
4. For tables, choose a table payload format from `local_filesystem_format.payload_format`, explicit user options, or the default table format.
5. For files, preserve bytes and use the stored or requested local extension.
6. For documents and scalar nodes, write the value as YSON, JSON, or YAML.
7. Recurse into map nodes.

A download immediately after an upload should produce the same filenames and sidecar formats, unless cluster-side data or attributes changed.

## Minimal example

Cypress tree:

```text
//home/project
  @owner = "analytics"
  users                  table
  config                 map_node
    limits               document
  script                 file
```

Local representation:

```text
project/
  @.yson
  users.parquet
  users@.yson
  config/
    limits.yaml
    limits@.yson
  config@.yson
  script.py
  script@.yson
```

The `users@.yson` sidecar stores the table schema and the `local_filesystem_format` attribute recording that the table payload was stored as Parquet and its attributes were stored as YSON. If this local tree is uploaded and later downloaded recursively, the downloader can write `users.parquet` again instead of falling back to a cluster-wide default such as `users.yson`.

## Non-goals

This draft does not define:

- a complete escaping scheme for every possible Cypress name;
- atomic multi-node upload semantics;
- chunk-level backup or restore;
- preservation of transient system attributes;
- automatic conversion between incompatible table schemas and local formats.

Those can be added later without changing the basic rule: payloads are stored under original names plus a format extension, and attributes are stored in `name@.yson`, `name@.json`, or `name@.yaml` sidecars.
