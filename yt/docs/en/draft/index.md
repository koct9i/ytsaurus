# Draft-0: About the Draft Category

## Purpose

The **Draft** category is a playground for writing and editing documentation articles, primarily by AI agents and contributors in early-stage exploration.

Articles in this category are **work-in-progress**. Content may be temporarily inconsistent, incomplete, or inaccurate. Do not rely on draft articles for production decisions.

## Rules for Draft Articles

1. **Numbering.** Drafts are consecutively numbered starting from `draft-0`. Numbers are never reused, even if an article is removed or promoted.

2. **Single language.** Each draft article is written in a single language (English or Russian). Translations are only added after the article reaches its final edited state and is graduated out of Draft.

3. **No stability guarantee.** Draft content can change significantly at any time — including structural rewrites, removal of sections, or renaming. Readers should treat all drafts as volatile.

4. **AI-assisted authoring.** Drafts may be created or substantially edited by AI agents. Human review is expected before graduation.

5. **Graduation process.** When a draft article is ready for production, it is moved to the appropriate documentation category and a proper translation/review cycle begins. The original draft entry in this category is then replaced with a redirect or a short tombstone note indicating where the content moved.

6. **Scope.** Draft articles may cover any topic relevant to {{product-name}}: new features, architecture explorations, operational how-tos, or experimental ideas. There is no constraint on subject matter within this category.

7. **Self-contained.** Each draft should be understandable on its own. Avoid hard dependencies on other draft articles that may themselves be unstable.

8. **Review encouraged.** Anyone — human or AI — is encouraged to leave comments and suggest improvements on draft articles. Draft PRs benefit from lightweight review focused on factual correctness rather than style.

9. **Metadata header.** Each draft article should begin with a short metadata block (in a comment or as a leading paragraph) noting: the draft number, the author or agent that created it, the creation date, and the current status (e.g., *in progress*, *ready for review*, *ready for graduation*).

## This Article

- **Draft number:** 0
- **Author:** AI agent (GitHub Copilot)
- **Created:** 2026-05-27
- **Status:** Final — this article is the permanent description of the Draft category and is not subject to graduation.

## Current draft articles

- [Draft-1: Master server architecture — Hydra and persistence foundations](./master-architecture-draft-1.md)
- [Draft-2: Master server architecture — cell topology and inter-cell communication](./master-architecture-draft-2.md)
- [Draft-3: Master server architecture — transaction lifecycle and mutation pipeline](./master-architecture-draft-3.md)
- [Draft-4: Master server architecture — read request execution](./master-architecture-draft-4.md)
- [Draft-5: Master server architecture — performance and administration](./master-architecture-draft-5.md)
- [Draft-6: Dynamic tables — performance profiling and bottleneck analysis](./dynamic-tables-profiling-draft-6.md)
- [Draft-7: Dynamic tables — capacity planning and scaling](./dynamic-tables-capacity-scaling-draft-7.md)
- [Draft-8: Dynamic tables — operating in production](./dynamic-tables-operations-topic-draft-8.md)
- [Draft-9: Dynamic tables (sorted) — operational corner cases and administration notes](./dynamic-tables-sorted-ops-corner-cases-draft-9.md)
- [Draft-10: Dynamic tables (ordered) — operational corner cases and administration notes](./dynamic-tables-ordered-ops-corner-cases-draft-10.md)
