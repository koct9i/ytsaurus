<!--
Draft number: 13
Author: AI agent (GitHub Copilot)
Created: 2026-06-01
Status: In progress
Target: user-guide/storage/yson
-->

# YSON formats and SDK guide

This article describes the three YSON encoding formats and the streaming types (Node/ListFragment/MapFragment), documents every format option available on the server side, and compares support and behaviour across the four official SDK implementations (C++, Python, Go, and Java).

## YSON encoding formats { #formats }

Every YSON value can be serialised in one of three encoding formats.

### Binary { #binary }

Binary YSON is the most compact representation.  Strings, integers, unsigned integers, and doubles are stored with a 1-byte type tag followed by a variable-length encoding of the value.

| Type | Tag byte | Encoding |
|------|----------|---------|
| String | `\x01` | ZigZag-encoded `sint32` length (protobuf wire format) followed by raw bytes |
| Int64 | `\x02` | ZigZag-encoded `sint64` (protobuf wire format) |
| Double | `\x03` | 8-byte little-endian IEEE 754 (`double`) |
| Boolean `%false` | `\x04` | (no payload) |
| Boolean `%true` | `\x05` | (no payload) |
| Uint64 | `\x06` | Unsigned `uint64` (protobuf wire format) |

Collection delimiters (`[`, `]`, `{`, `}`, `<`, `>`, `;`, `=`, `#`) are identical to the text format. Only scalar *values* are compacted.

Use binary format when bandwidth or storage space matters and human-readability is not required. It is the default format used internally by {{product-name}} and the default output format in the Python SDK's `YsonFormat` and the C++ writer.

### Text { #text }

Text YSON produces a compact, human-readable stream with no extra whitespace. Numbers appear as plain decimal literals; strings are quoted with C-style escaping when necessary. There is no indentation.

Example:

```yson
{"name"="Elena";"uid"=95792365232151958}
```

This is the default format produced by the Python pure-Python writer and by the Go `Marshal` and `MarshalFormat` helpers.

### Pretty { #pretty }

Pretty YSON is an indented version of text YSON, intended for display and debugging. Each nested value is placed on its own line with an indent of 4 spaces by default.

Example:

```yson
{
    "name" = "Elena";
    "uid" = 95792365232151958;
}
```

The indentation width is configurable in Python and Java (default 4). In Go it is hardcoded to 4; in C++ it is configurable in the YT-internal writer (`yt/yt/core/yson`), but not in the standalone `library/cpp/yson` writer.

### Choosing a format { #choosing-format }

| Scenario | Recommended format |
|----------|--------------------|
| Wire protocol / IPC | `binary` |
| Log files | `text` |
| Human inspection / debugging | `pretty` |
| Config files (small payloads) | `text` or `pretty` |

## Streaming types { #streaming-types }

A YSON stream can encode three distinct *stream types*:

| Name | Description | Grammar reference |
|------|-------------|-------------------|
| `Node` | A single complete value (the default) | `<tree>` |
| `ListFragment` | A semicolon-separated stream of values (no enclosing `[…]`) | `<list-fragment>` |
| `MapFragment` | A semicolon-separated stream of `key=value` pairs (no enclosing `{…}`) | `<map-fragment>` |

`ListFragment` is the most common streaming type in {{product-name}}: table rows, operation input/output, and `read_table` / `write_table` commands all use it.

`MapFragment` is used in configuration files and Cypress responses that return multiple top-level key-value pairs.

C++, Python, and Go expose APIs for all three streaming types. Java supports `Node` and `ListFragment` in `YsonParser`, but does not currently provide a dedicated `MapFragment` parsing API.

## YSON as a table format: options { #format-options }

When YSON is used as the format for reading or writing table data (via the CLI, HTTP proxy, or an SDK), the format string can carry options in YSON attribute syntax:

```bash
yt read --format '<format=pretty;skip_null_values=%true>yson' //path/to/table
```

The default values are shown in parentheses.

### format (`binary`) { #opt-format }

Selects the encoding variant used when *writing* data. Readers always accept all three formats transparently.

- `binary` — binary YSON
- `text` — text YSON
- `pretty` — indented text YSON

### skip_null_values (`%false`) { #opt-skip-null }

When `%true`, columns whose value is `null` (entity `#`) are omitted from the output entirely. Useful for sparse schematized tables to reduce output volume.

### complex_type_mode (`named`) { #opt-complex-type }

Controls how composite schema types (`struct`, `variant`, `tuple`) are represented.

- `named` — struct fields are emitted as a YSON map `{field_name=value;…}`.
- `positional` — struct fields are emitted as a YSON list `[value;…]` in declaration order.

See [Data types](../user-guide/storage/data-types.md#yson) for full details.

### string_keyed_dict_mode (`positional`) { #opt-dict-mode }

Controls the representation of `Dict<string, V>` typed columns.

- `positional` — each entry is `[key;value]` inside a list.
- `named` — each entry is a single-key map `{key=value}`.

### decimal_mode (`binary`) { #opt-decimal }

Selects the encoding for `Decimal` typed columns.

- `binary` — compact binary encoding.
- `text` — human-readable decimal string, e.g. `"3.14"`.

### time_mode (`binary`) { #opt-time }

Selects the encoding for `Date`, `Datetime`, and `Timestamp` typed columns.

- `binary` — integer (number of days/seconds/microseconds since epoch).
- `text` — ISO 8601 string, e.g. `"2024-01-15"`.

### uuid_mode (`binary`) { #opt-uuid }

Selects the encoding for `UUID` typed columns.

- `binary` — 16-byte raw binary string.
- `text_yt` — YT canonical string representation (big-endian pairs, e.g. `"aabbccdd-eeff-0011-2233-445566778899"`).
- `text_yql` — YQL canonical string representation.

### sort_keys (`%false`) { #opt-sort-keys }

When `%true`, map/struct keys are emitted in sorted (lexicographic) order. Useful for reproducible output in tests or diffs.

### Type-conversion options { #opt-type-conversion }

These options control how {{product-name}} converts values between types when reading or writing:

| Option | Default | Description |
|--------|---------|-------------|
| `enable_string_to_all_conversion` | `%false` | Parse string `"42u"` → `42u`, `"false"` → `%false` |
| `enable_all_to_string_conversion` | `%false` | Serialise `3.14` → `"3.14"`, `%true` → `"true"` |
| `enable_integral_type_conversion` | `%true` | Convert between `int64` and `uint64`; error on overflow |
| `enable_integral_to_double_conversion` | `%false` | Convert `42` → `42.0` |
| `enable_type_conversion` | `%false` | Enable all four conversion options at once |

## SDK comparison { #sdk-comparison }

### Format support

| Feature | C++ | Python | Go | Java |
|---------|:---:|:------:|:--:|:----:|
| Binary format | ✓ | ✓ | ✓ | ✓ |
| Text format | ✓ | ✓ | ✓ | ✓ |
| Pretty format | ✓ | ✓ | ✓ | ✓ |
| Configurable indent | ✓ (YT-internal) | ✓ | ✗ | ✓ |
| Node stream type | ✓ | ✓ | ✓ | ✓ |
| ListFragment stream type | ✓ | ✓ | ✓ | ✓ |
| MapFragment stream type | ✓ | ✓ | ✓ | ✗ |
| Attributes on any value | ✓ | ✓ | ✓ | ✓ |
| Entity value (`#`) | ✓ | ✓ | ✓ | ✓ |
| Raw YSON pass-through | ✓ | ✗ | ✓ | ✗ |
| Nesting-depth limit | ✓ | ✗ | ✓ | ✗ |
| Memory limit | ✓ | ✗ | ✗ | ✗ |
| Circular reference detection | ✗ | ✓ | ✗ | ✗ |
| C++ bindings | — | optional | ✗ | ✗ |

### High-level serialisation

| Feature | C++ | Python | Go | Java |
|---------|:---:|:------:|:--:|:----:|
| Reflection-based (de)serialisation | ✓ | ✓ | ✓ | ✓ |
| Custom marshaler interface | ✓ | ✗ | ✓ | ✗ |
| Struct field tags | ✗ | ✗ | ✓ | ✗ |
| Lazy/deferred parsing | ✗ | ✓ | ✗ | ✗ |
| Sort keys on output | ✗ | ✓ | maps only¹ | ✗ |
| Encoding option for strings | ✗ | ✓ | ✗ | ✗ |
| Unsigned integer type | ✓ | ✓ | ✓ | ✓ |

¹ Go sorts `map` keys lexicographically on output, but struct fields are always written in their declaration order.

---

## C++ { #cpp }

**Package:** `library/cpp/yson` (standalone) and `yt/yt/core/yson` (YT-internal).

### Formats

The `EYsonFormat` enum (defined in `library/cpp/yt/yson_string/public.h`):

```cpp
enum class EYsonFormat {
    Binary,  // compact binary
    Text,    // compact text
    Pretty,  // indented text
};
```

The `EYsonType` enum selects the stream type:

```cpp
enum class EYsonType : i8 {
    Node         = 0,
    ListFragment = 1,
    MapFragment  = 2,
};
```

### Writing

```cpp
#include <library/cpp/yson/writer.h>

// Create a text-format node writer
NYson::TYsonWriter writer(&outputStream, NYson::EYsonFormat::Text);

writer.OnBeginMap();
writer.OnKeyedItem("name");
writer.OnStringScalar("Alice");
writer.OnKeyedItem("age");
writer.OnInt64Scalar(30);
writer.OnEndMap();
```

Constructor signature:

```cpp
TYsonWriter(
    IOutputStream* stream,
    EYsonFormat format = EYsonFormat::Binary,
    EYsonType type = EYsonType::Node,
    bool enableRaw = false);  // enable OnRaw() pass-through
```

For high-throughput binary writing in `yt/yt/core/yson`, prefer `TBufferedBinaryYsonWriter`, which uses an internal buffer to reduce system-call overhead.

### Parsing

```cpp
#include <library/cpp/yson/parser.h>

// Parsing with a custom consumer
MyConsumer consumer;
NYson::TYsonParser parser(&consumer, &inputStream, NYson::EYsonType::ListFragment);
parser.Parse();
```

Advanced parser options (via `TYsonParserConfig`):

| Option | Default | Description |
|--------|---------|-------------|
| `EnableLinePositionInfo` | `false` | Include line and column in error messages |
| `MemoryLimit` | unlimited | Abort if internal buffers exceed this size |
| `EnableContext` | `true` | Include surrounding context in parse errors |
| `NestingLevelLimit` | 256 | Maximum nesting depth before a parse error |

{% note info %}

Two nesting-level limits are defined:
- `CypressWriteNestingLevelLimit = 128` — used for commands that write to Cypress (e.g. `set`, `create`) to avoid writing values that cannot be safely read back by older clients.
- `NewNestingLevelLimit = 256` — used everywhere else.

{% endnote %}

### DOM (node library)

```cpp
#include <library/cpp/yson/node/node.h>

NYT::TNode node = NYT::TNode::CreateMap();
node["key"] = "value";
node["count"] = 42;

// Attributes
node.SetAttributes(NYT::TNode::CreateMap());
node.GetAttributes()["meta"] = "info";
```

The `TNode` class provides dynamic DOM-like access to YSON trees with full attribute support.

---

## Python { #python }

**Package:** `yt.yson` (pure Python) or `yt_yson_bindings` (optional C++ extension).

When the C++ extension is installed, `yt.yson.TYPE == "BINARY"` and the extension's `load`/`loads`/`dump`/`dumps` are used automatically. The pure Python implementation is the fallback.

### Formats and types

```python
import yt.yson as yson

# Text format (default for dumps)
text = yson.dumps({"key": "value"})

# Binary format
binary = yson.dumps({"key": "value"}, yson_format="binary")

# Pretty format
pretty = yson.dumps({"key": "value"}, yson_format="pretty", indent=2)

# List fragment (sequence of top-level values)
fragment = yson.dumps([1, 2, 3], yson_type="list_fragment")
```

### Serialisation options (`dump` / `dumps`)

| Parameter | Default | Description |
|-----------|---------|-------------|
| `yson_format` | `"text"` | `"binary"`, `"text"`, or `"pretty"` |
| `yson_type` | `"node"` | `"node"`, `"list_fragment"`, or `"map_fragment"` |
| `indent` | `4` | Number of spaces per level for pretty format |
| `encoding` | `"utf-8"` | Encoding used to convert `str` to bytes |
| `sort_keys` | `False` | Sort map keys lexicographically |
| `ignore_inner_attributes` | `False` | Skip YSON attributes except on the top-level value |
| `check_circular` | `True` | Raise an error on circular object references |

### Deserialisation options (`load` / `loads`)

| Parameter | Default | Description |
|-----------|---------|-------------|
| `yson_type` | `None` (auto) | `"node"`, `"list_fragment"`, or `"map_fragment"` |
| `always_create_attributes` | `True` | Attach an empty `.attributes` dict to every value |
| `encoding` | `"utf-8"` | Encoding used to decode byte strings |
| `lazy` | `False` | Defer attribute parsing (requires C++ bindings) |
| `raw` | `False` | (deprecated) Return raw bytes instead of parsed values |

### YSON type classes

Every Python value returned by the YSON parser is a subclass of the corresponding built-in type with an added `.attributes` dict:

| YSON type | Python class | Base type |
|-----------|-------------|-----------|
| String | `YsonString` | `bytes` |
| Int64 | `YsonInt64` | `int` |
| Uint64 | `YsonUint64` | `int` |
| Double | `YsonDouble` | `float` |
| Boolean | `YsonBoolean` | `int` |
| List | `YsonList` | `list` |
| Map | `YsonMap` | `dict` |
| Entity | `YsonEntity` | `object` |

```python
node = yson.loads(b'<type="table">#')
assert isinstance(node, yson.YsonEntity)
assert node.attributes["type"] == "table"
```

### YsonFormat (wrapper)

When used as a table format in operations or `read_table`/`write_table`, use `YsonFormat`:

```python
from yt.wrapper import YsonFormat

fmt = YsonFormat(
    format="binary",                    # "binary" (default), "text", "pretty"
    control_attributes_mode="iterator", # "row_fields", "iterator", "none"
    always_create_attributes=False,
    encoding="utf-8",                   # None disables string decoding
    sort_keys=False,
    lazy=False,                         # requires C++ bindings
)
```

---

## Go { #go }

**Package:** `go.ytsaurus.tech/yt/go/yson`

### Formats and stream kinds

```go
import "go.ytsaurus.tech/yt/go/yson"

// High-level: Marshal / Unmarshal
data, err := yson.Marshal(myStruct)               // text format, node type
data, err := yson.MarshalFormat(myStruct, yson.FormatBinary)

var out MyStruct
err = yson.Unmarshal(data, &out)

// With options
opts := &yson.EncoderOptions{SupportYPAPIMaps: true}
data, err = yson.MarshalOptions(myStruct, opts)
```

### Low-level Writer

```go
cfg := yson.WriterConfig{
    Format: yson.FormatPretty,
    Kind:   yson.StreamListFragment,
}
w := yson.NewWriterConfig(os.Stdout, cfg)

w.BeginMap()
w.MapKeyString("name")
w.String("Alice")
w.EndMap()

if err := w.Finish(); err != nil {
    // handle error
}
```

### Low-level Reader

```go
r := yson.NewReaderKind(inputStream, yson.StreamListFragment)

for {
    event, err := r.Next(false)
    if err != nil {
        break
    }
    switch event {
    case yson.EventLiteral:
        switch r.Type() {
        case yson.TypeString:
            fmt.Println("string", r.String())
        case yson.TypeInt64:
            fmt.Println("int64", r.Int64())
        case yson.TypeUint64:
            fmt.Println("uint64", r.Uint64())
        case yson.TypeBool:
            fmt.Println("bool", r.Bool())
        case yson.TypeFloat64:
            fmt.Println("float64", r.Float64())
        case yson.TypeEntity:
            fmt.Println("entity", nil)
        default:
            fmt.Println(r.Type())
        }
        return
    }
}
```

### Format constants

| Constant | Value | Description |
|----------|-------|-------------|
| `FormatBinary` | 0 | Compact binary |
| `FormatText` | 1 | Compact text |
| `FormatPretty` | 2 | Indented text |

Default for `NewWriter` and `Marshal` is `FormatText`.

### Stream kind constants

| Constant | Value |
|----------|-------|
| `StreamNode` | 0 |
| `StreamListFragment` | 1 |
| `StreamMapFragment` | 2 |

Default for `NewWriter` and `NewReader` is `StreamNode`.

### Struct tags

The Go SDK uses struct field tags to control YSON serialisation:

```go
type Node struct {
    // Regular map key
    Name string `yson:"name"`

    // Omit field when zero
    Age int `yson:"age,omitempty"`

    // Encode as a YSON attribute preceding the map body
    Format string `yson:"format,attr"`

    // Encode this field's value as the entire YSON value for the struct;
    // all ",attr" fields are written as attributes before it.
    // All other fields are ignored.
    Data any `yson:",value"`

    // Decode/encode the entire YSON attribute section into/from this map field.
    // Note: ignored if the struct has any other fields tagged with ",attr".
    Attrs map[string]any `yson:",attrs"`

    // Skip field entirely
    Internal string `yson:"-"`
}
```

| Tag option | Effect |
|------------|--------|
| `yson:"key"` | Use `key` as the YSON map key |
| `yson:",omitempty"` | Omit field when its value is the zero value |
| `yson:"key,attr"` | Encode field as a YSON attribute named `key` (written before the map/value body) |
| `yson:",value"` | Encode this field as the entire YSON value of the struct; other non-attr fields are ignored |
| `yson:",attrs"` | Map field that captures or provides the entire YSON attribute section; field type must be `map[K]V` |
| `yson:"-"` | Skip field entirely |
| `yson:"-,"` | Use `"-"` as the YSON map key (the trailing comma distinguishes it from the skip marker) |

**Key ordering.** The encoder sorts map keys lexicographically but encodes struct fields in declaration order. If reproducible output is important for a struct, declare its fields in the desired order.

### Custom interfaces

```go
// Custom encoding
type Marshaler interface {
    MarshalYSON() ([]byte, error)
}

// Custom streaming encoding
type StreamMarshaler interface {
    MarshalYSON(*Writer) error
}
```

### Notable differences from other SDKs

- **Binary is not the default.** `Marshal` defaults to `FormatText`, unlike the Python `YsonFormat` which defaults to `binary`.
- **No attribute dict on decoded values.** Unlike Python, decoded Go values are native Go types; attributes are only available when decoding into a `map[string]any` or a struct with an `attrs` field.
- **No configurable indent.** The Go SDK does not expose an `indent` parameter; the 4-space indent for pretty format is hardcoded.
- **Auto-detect on reading.** The Reader handles binary and text YSON transparently; no format hint is needed when reading.

---

## Java { #java }

**Package:** `tech.ytsaurus.yson` (`yt/java/yson`)

### Writing

Java provides two separate writer classes rather than a single class with a format parameter:

```java
// Binary writer
try (YsonBinaryWriter writer = new YsonBinaryWriter(outputStream)) {
    writer.onBeginMap();
    writer.onKeyedItem("name");
    writer.onString("Alice");
    writer.onEndMap();
}

// Text writer (compact)
YsonTextWriter textWriter = new YsonTextWriter(System.out);
textWriter.onBeginMap();
textWriter.onKeyedItem("name");
textWriter.onString("Alice");
textWriter.onEndMap();
// Text writer (pretty)
YsonTextWriter prettyWriter = YsonTextWriter.builder()
    .setOutputStream(outputStream)
    .setPrettyPrinting()    // enables indentation
    .setIndent(2)           // optional, default 4
    .build();
```

### Parsing

```java
YsonParser parser = new YsonParser(inputStream);

// Parse a single node
parser.parseNode(myConsumer);

// Parse a list fragment (stream of values)
parser.parseListFragment(myConsumer);

// Parse one item at a time from a list fragment
while (parser.parseListFragmentItem(myConsumer)) {
    // process item
}
```

### YsonConsumer interface

All writers implement `YsonConsumer`. Implement this interface to build custom processing pipelines:

```java
public interface YsonConsumer {
    void onEntity();
    void onInteger(long value);
    void onUnsignedInteger(long value);
    void onBoolean(boolean value);
    void onDouble(double value);
    void onString(byte[] value, int offset, int length);

    void onBeginList();
    void onListItem();
    void onEndList();

    void onBeginMap();
    void onKeyedItem(byte[] value, int offset, int length);
    void onEndMap();

    void onBeginAttributes();
    void onEndAttributes();
}
```

### Notable differences from other SDKs

- **No format enum.** Format is implicit in the choice of writer class (`YsonBinaryWriter` vs `YsonTextWriter`).
- **MapFragment not in the parser API.** `YsonParser` exposes `parseNode` and `parseListFragment`; map-fragment streams are handled by calling `parseNode` for each key-value pair inside an outer map context.
- **Unsigned integers as `long`.** Java has no unsigned `long` type; `uint64` values are stored as `long` with the sign bit used as the most-significant bit. Values larger than `Long.MAX_VALUE` will appear negative.
- **Strings as byte arrays.** String and key values are passed as `byte[]` with an offset and length rather than `String` or `ByteBuffer`, avoiding intermediate allocations in hot paths.
- **No DOM node class in this package.** The DOM representation (`YTreeNode`, `YTreeMapNode`, etc.) lives in the separate `yt/java/yson-tree` package.

---

## Summary of differences { #summary }

| Aspect | C++ | Python | Go | Java |
|--------|-----|--------|----|----|
| **Default write format** | binary | text (dump) / binary (YsonFormat) | text | binary (YsonBinaryWriter) / text (YsonTextWriter) |
| **Format selection** | enum `EYsonFormat` | string parameter | constant `Format` | class choice |
| **Configurable indent** | ✓ (YT-internal writer) | ✓ (`indent=N`) | ✗ (hardcoded 4) | ✓ (`setIndent(N)`) |
| **Reader auto-detects format** | ✓ | ✓ | ✓ | ✓ |
| **DOM library** | `TNode` (same package) | `YsonType` subclasses | none (use `any`) | `YTreeNode` (separate package) |
| **Nesting depth limit** | 256 (configurable) | none | 256 (hardcoded) | none |
| **Memory limit on parse** | configurable | none | none | configurable buffer size |
| **Attributes on DOM values** | ✓ | ✓ | partial (struct tag) | ✓ (YTreeNode) |
| **uint64 representation** | `ui64` | `YsonUint64` | `uint64` | `long` (bit-compatible) |
| **Custom (de)serialiser** | ✓ (consumer pattern) | ✗ | ✓ (`Marshaler` interface) | ✗ |
| **Struct tags** | ✗ | ✗ | ✓ | ✗ |
| **Lazy parsing** | ✗ | ✓ (bindings only) | ✗ | ✗ |
