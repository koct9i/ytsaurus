# Flow source/documentation consistency report

Scope inspected:

- `yt/yt_proto/yt/flow`
- `yt/yt/flow`
- `yt/docs/ru/flow`
- `yt/docs/ru/_includes/flow`

Generated: 2026-07-23.

## Methodology

This is a static repository audit. I checked file presence under the four requested trees, compared public Flow documentation wrappers with `_includes`, scanned local Markdown/include/code references, and spot-checked documentation references against the source tree.

## Summary of findings

| Severity | Problem area | Impact |
| --- | --- | --- |
| High | Public docs advertise Java/Kotlin Flow material, but Java/Kotlin sources are absent from `yt/yt/flow` | Many pages render links and code includes that cannot resolve in this checkout |
| High | `toc-flow.yaml` points to pages that do not exist in `yt/docs/ru/flow` | Navigation contains broken entries |
| High | Generated YSON documentation is referenced without the expected index page | Many generated-struct cross-links point to a missing `all_yson_structs.md` |
| Medium | Several docs link to moved/renamed source paths | Readers cannot reach examples/tools from docs |
| Medium | NoOp quickstart structure does not match checked-in files | The primary quickstart cannot be followed verbatim from this checkout |
| Medium | `_includes` has partials without public pages | Some reusable content is not directly reachable from the Flow TOC/public docs |
| Medium | Internal/Yandex-specific docs and sources are referenced from the open tree | Open-source builds may contain dangling links or hidden documentation branches |
| Low | Naming inconsistencies (`wordcount` vs `word_count`, `yt_sync` vs `yt_sync_mini`) | Searchability and maintenance are worse, and some links look stale |

## Detailed problems

### 1. Java/Kotlin documentation is present, but corresponding source trees are missing

The Flow docs contain a complete Java section and many Java/Kotlin example pages. However, under `yt/yt/flow/examples` only `cpp` and `python` example directories exist; there is no `java` or `kotlin` example tree. The docs also reference `yt/java/flow`, which is not present in this checkout.

Examples:

- `yt/docs/ru/flow/toc-flow.yaml` contains a Java section and Java example entries such as `java/examples/wordcount.md`.
- `yt/docs/ru/_includes/flow/java/getting-started.md` links to `{{source-root}}/yt/java/flow` and `{{source-root}}/yt/yt/flow/examples/java`.
- `yt/docs/ru/_includes/flow/java/examples/*.md` contains code includes for paths like `yt/yt/flow/examples/java/word_count/...` and `yt/yt/flow/examples/kotlin/word_count/...`.

Observed source tree:

- Present: `yt/yt/flow/examples/cpp`
- Present: `yt/yt/flow/examples/python`
- Missing: `yt/yt/flow/examples/java`
- Missing: `yt/yt/flow/examples/kotlin`
- Missing: `yt/java/flow`

Impact: the Java/Kotlin documentation appears publishable, but many links and `{% code %}` snippets cannot resolve from the source tree available in this repository.

### 2. Flow TOC references missing public pages

`yt/docs/ru/flow/toc-flow.yaml` contains navigation entries for pages that are not present under `yt/docs/ru/flow`.

Missing pages under `yt/docs/ru/flow` include:

- `yql/external-provider-auth.md`
- `release/launch-infractl.md`
- `release/monitoring.md`
- `release/problems.md`
- `tools/draw-pipeline-graph.md`
- `tools/job-investigation.md`
- `tools/pipeline-chaos-monkey.md`

The same TOC also references many `../yandex-specific/flow/...` pages that are outside the requested Flow docs tree and are absent in this checkout, including extension pages, internal release pages, and additional examples.

Impact: rendered navigation will contain broken links unless those pages are injected from a private/internal documentation tree during publishing.

### 3. Generated YSON docs are inconsistent: index page is missing

The TOC references `generated_docs/all_yson_structs.md`, and many docs link to anchors in `flow/generated_docs/all_yson_structs.md`. The directory `yt/docs/ru/flow/generated_docs` contains many per-struct generated Markdown files, but the expected `all_yson_structs.md` file is absent.

Examples of references:

- `yt/docs/ru/flow/toc-flow.yaml` has `href: generated_docs/all_yson_structs.md`.
- `yt/docs/ru/_includes/flow/python/getting-started.md` links to `../../../flow/generated_docs/all_yson_structs.md#NYT_NFlow_TVanillaConfig`.
- Generated files such as `yt/docs/ru/flow/generated_docs/NYT_NFlow_TFlowServerConfig.md` link back to `./all_yson_structs#...`.

Impact: the generated documentation tree is internally cross-linked as if an aggregate index exists, but that index is missing. Users landing in generated struct pages cannot follow type links reliably.

### 4. Documentation links to missing source directories for examples and test helpers

Several docs point to source directories/files that are absent or have a different name.

Notable examples:

- `yt/docs/ru/_includes/flow/concepts/pipeline-object.md` links to `yt/yt/flow/examples/cpp/noop/yt_sync_mini`, but the actual directory is `yt/yt/flow/examples/cpp/noop/yt_sync`.
- `yt/docs/ru/_includes/flow/testing-integration-body.md` links to `yt/yt/flow/examples/cpp/wait_click_join/test/test_wait_click_join.py`, but `yt/yt/flow/examples/cpp/wait_click_join` has no `test` directory in this checkout.
- `yt/docs/ru/_includes/flow/testing-integration-body.md` says `yt/yt/flow/library/python/integration_test_base` has a `README.md`, but no `README.md` is present in that directory.
- `yt/docs/ru/_includes/flow/contributor/testing-framework.md` links to `yt/yt/flow/cpp/misc/error_backtrace_enricher.h`, while the Flow C++ library path in this checkout is under `yt/yt/flow/library/cpp/...`.

Impact: these look like stale paths caused by source moves or partial open-source export.

### 5. NoOp quickstart does not match the checked-in NoOp example

The quickstart tells readers to run `yt/yt/flow/examples/cpp/noop` as a ready-made minimal NoOp project. The files shown in the documentation do not match the files currently present in that directory.

Documentation claims:

- `run_noop_pipeline.sh` exists and starts the Controller, Worker, and pipeline.
- Root-level `README.md`, `controller.config.yson`, and `worker.config.yson` exist.
- `pipeline/pipeline.yson` exists as the pipeline topology/spec file.
- In open-source mode the helper directory is named `yt_sync_mini`.

Checked-in files under `yt/yt/flow/examples/cpp/noop`:

- Root contains `CMakeLists.txt`, `ya.make`, `pipeline/`, and `yt_sync/`.
- No root-level `README.md`.
- No `run_noop_pipeline.sh`.
- No root-level `controller.config.yson` or `worker.config.yson`.
- No `pipeline/pipeline.yson`; `pipeline/main.cpp` registers `TNoopFunction` and starts `TSimpleRunnerProgram`.
- Helper directory is `yt_sync`, not `yt_sync_mini`. Its `ya.make` builds `PY3_PROGRAM(yt_sync)` and conditionally depends on `yt/yt/flow/library/python/yt_sync_mini` only when `OPENSOURCE` is set.

Impact: the introductory NoOp quickstart appears stale. A user following the open-source instructions will fail at the first run command because the documented script and helper path are missing. The docs also describe a static `pipeline.yson` topology file, while the checked-in C++ example uses the simple runner entry point.

### 6. Connector docs mostly match connector source directories, but coverage is incomplete

Connector documentation exists for:

- Queue
- Static Table
- Sorted Dynamic Table
- Service Log

Matching source directories exist for those connectors:

- `yt/yt/flow/library/cpp/connectors/queue`
- `yt/yt/flow/library/cpp/connectors/static_table`
- `yt/yt/flow/library/cpp/connectors/sorted_dynamic_table`
- `yt/yt/flow/library/cpp/connectors/servicelog`

However, the source tree also contains connector directories that are not documented as top-level connector pages:

- `yt/yt/flow/library/cpp/connectors/random`
- `yt/yt/flow/library/cpp/connectors/static_table_v2`
- `yt/yt/flow/library/cpp/connectors/common`

Impact: this may be intentional for internal/common/test-only connectors, but from source-vs-docs comparison it is unclear whether `random` and `static_table_v2` should be documented or marked internal/experimental.

### 7. Public Flow docs are wrappers around `_includes`, but a few include files have no public page

Most public Markdown pages in `yt/docs/ru/flow` are thin wrappers that include the corresponding file from `yt/docs/ru/_includes/flow`. The reverse mapping is not complete: the following include files do not have a matching public page under `yt/docs/ru/flow`:

- `java/_field_order_warning.md`
- `language-choice.md`
- `testing-integration-body.md`
- `testing-test-param-body.md`

Impact: these may be intentional reusable partials. If `language-choice.md` is meant as a standalone article, it is currently unreachable as a public Flow page.

### 8. Internal/Yandex-specific links are mixed into open documentation

Many docs include links and code snippets guarded by `{% if audience == "internal" %}`, but `toc-flow.yaml` also contains unconditional links to `../yandex-specific/flow/...`. In this checkout, those targets are missing.

Examples:

- `../yandex-specific/flow/extensions/about.md`
- `../yandex-specific/flow/extensions/logbroker.md`
- `../yandex-specific/flow/release/ui.md`
- `../yandex-specific/flow/other/history.md`
- `../yandex-specific/flow/cpp/examples/yql_protobuf.md`

Impact: if the same TOC is used for open-source docs, the open build will expose broken internal links. If a private overlay supplies these files, that dependency should be documented or conditioned in the TOC.

### 9. Python docs mention internal/Yandex-specific examples as if they provide reusable code

Python state docs reference Logbroker examples under `yt/yt/flow/yandex/examples/python/lb_wait_click_join` and even reference proto imports from a Java/Yandex example namespace. Those source paths are absent in the inspected tree.

Impact: open-source readers cannot run or inspect these examples, and the import path shown in the docs cannot work without private/yandex-specific sources.

### 10. Documentation naming is inconsistent for Word Count examples

The source directories use `word_count` for C++ and Python examples. Public doc page names use:

- C++: `cpp/examples/word_count.md`
- Python: `python/examples/wordcount.md`
- Java: `java/examples/wordcount.md`

Impact: links currently work for the public Python page, but the mixed `wordcount`/`word_count` spelling makes source/doc mapping harder and increases the chance of stale links.

## Suggested follow-up actions

1. Decide whether Java/Kotlin Flow docs are intended for this open-source tree.
   - If yes, add the missing `yt/java/flow` and `yt/yt/flow/examples/{java,kotlin}` sources.
   - If no, remove or condition Java/Kotlin pages and TOC entries for open-source builds.
2. Restore or generate `yt/docs/ru/flow/generated_docs/all_yson_structs.md`, or update all links to point to the existing per-struct generated pages.
3. Fix stale source paths:
   - `examples/cpp/noop/yt_sync_mini` -> `examples/cpp/noop/yt_sync`, unless the directory should be renamed.
   - `yt/yt/flow/cpp/misc/...` -> the actual `yt/yt/flow/library/cpp/...` location if applicable.
   - Add or update missing README/reference docs for `integration_test_base`.
4. Refresh the NoOp quickstart against the checked-in example:
   - either add the documented `run_noop_pipeline.sh`, configs, `README.md`, `pipeline.yson`, and `yt_sync_mini` wrapper;
   - or rewrite the quickstart to use the actual `pipeline/` binary and `yt_sync/yt_sync` helper.
5. Split/condition internal-only TOC entries and internal source links so open-source documentation does not advertise absent private pages.
6. Document the status of `random` and `static_table_v2` connectors: public, experimental, internal, or intentionally undocumented.
7. Normalize example naming (`word_count` vs `wordcount`) or add a short convention note to avoid future broken links.

## Commands used for the audit

```bash
find .. -name AGENTS.md -print
rg --files yt/yt_proto/yt/flow yt/yt/flow yt/docs/ru/flow yt/docs/ru/_includes/flow
find yt/yt/flow -maxdepth 4 -type d | sort
find yt/yt/flow/examples -maxdepth 3 -type d | sort
find yt/yt/flow/examples/cpp/noop -maxdepth 3 -type f -o -type d | sort
rg -n "Java|Kotlin|examples/java|examples/kotlin|wordcount|word_count|integration_test_base|static_table_join|external_state_join|yt_sync_mini|launch-infractl|monitoring|problems|pipeline-chaos-monkey|generated_docs/all_yson_structs" yt/docs/ru/_includes/flow yt/docs/ru/flow/toc-flow.yaml yt/yt/flow/README.md yt/yt/flow -g '*.md' -g '*.yaml' -g 'ya.make'
rg -n "noop|yt_sync_mini|yt_sync|flow_noop|examples/cpp/noop" yt/docs/ru/flow yt/docs/ru/_includes/flow yt/yt/flow/examples/cpp/noop -g '*.md' -g '*.yaml' -g '*.py' -g '*.yson' -g 'ya.make'
python3 - <<'PY'
from pathlib import Path
import re
# Scanned Markdown include/link/code directives and checked whether local targets exist.
PY
```
