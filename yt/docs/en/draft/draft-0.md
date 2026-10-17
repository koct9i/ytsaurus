---
type: Draft Article
title: "Draft-0: About the Draft Category"
last_modified: 2026-08-29T00:00:00Z
tags: [documentation, drafts, okf]
status: stable
---

# Draft-0: About the Draft Category

## Purpose

The **Draft** category is a playground for writing and editing documentation articles, by developers, AI agents and contributors in early-stage exploration.

Articles in this category are **work-in-progress**. Content may be temporarily inconsistent, incomplete, or inaccurate. Do not rely solely on draft articles for production decisions.

The `yt/docs/en/draft` and `yt/docs/ru/draft` directories use the [Open Knowledge Format (OKF)][okf]. Every draft article must remain a conformant OKF concept document.[^okf-spec]

## Rules for Draft Articles

1. **Numbering.** Drafts are consecutively numbered starting from `draft-0`. Numbers are never reused, even if an article is removed or promoted.

2. **TOC registration.** Every draft article must be listed in `yt/docs/en/toc.yaml` and/or `yt/docs/ru/toc.yaml`, and draft entries there must appear in draft-number order.

3. **Single language.** Each draft article is written in a single language (English or Russian). Translations are only added after the article reaches its final edited state and is graduated out of Draft.

4. **No stability guarantee.** Draft content can change significantly at any time — including structural rewrites, removal of sections, or renaming. Readers should treat all drafts as volatile.

5. **AI-assisted authoring.** Drafts may be created or substantially edited by AI agents. Human review is expected before graduation.

6. **Graduation process.** When a draft article is ready for production, it is moved to the appropriate documentation category and a proper translation/review cycle begins. The original draft entry in this category could be removed immediately or replaced with a redirect or a short tombstone note indicating where the content moved.

7. **Scope.** Draft articles may cover any topic relevant to {{product-name}}: new features, architecture explorations, operational how-tos, or experimental ideas. There is no constraint on subject matter within this category.

8. **Self-contained.** Each draft should be understandable on its own. Avoid hard dependencies on other draft articles that may themselves be unstable.

9. **Review encouraged.** Anyone — human or AI — is encouraged to leave comments and suggest improvements on draft articles. Draft PRs benefit from lightweight review focused on factual correctness rather than style.

10. **Open Knowledge Format.** Every draft article must be a UTF-8 Markdown concept document with an OKF YAML frontmatter block at the very start of the file. The frontmatter must contain `type`, `title`, `last_modified`, `tags`, and `status`. Set `type: Draft Article`, format the title as `Draft-<number>: <human-friendly title>`, write `last_modified` as an ISO 8601 datetime with an explicit UTC offset, and set `status: draft`. Producer-defined frontmatter fields are allowed, but they must not replace these fields.

11. **Metadata maintenance.** When an article changes meaningfully, update `last_modified` and keep its tags representative of the current content. Record reviews in `verified`, and use `stale_after` when the knowledge becomes unsafe to rely on after a known time. The YAML frontmatter is the authoritative metadata block; do not add a second, prose-only draft metadata block.

12. **Links and reserved files.** Use standard Markdown links for relationships between draft concepts, preferably bundle-relative links where the documentation renderer supports them. Do not use the OKF-reserved filenames `index.md` or `log.md` for ordinary draft concepts; if those files are added, they must follow the OKF structures for indexes and logs.

## This Article

Draft-0 is the permanent, stable reference for the Draft category and is not subject to graduation. Its OKF metadata therefore uses `status: stable`, as the sole exception to the `status: draft` rule for articles in these directories.

[okf]: https://github.com/GoogleCloudPlatform/open-knowledge-format/blob/main/SPEC.md

[^okf-spec]: Open Knowledge Format specification, version 0.2 when this rule was adopted.
