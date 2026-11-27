# Changelog

All notable changes to `serena-go` are documented in this file.

## [0.2.0] - 2026-11-27

### Added
- **V2 Pure-Go Shared `mmap` Binary Index (`.sidx`):** Read-only `syscall.Mmap` shared across concurrent MCP sessions (0 duplicated RSS), precomputed inverted posting list (`<6 ms` BM25), `<1 ms` 100%-recall symbol table lookup, deterministic ordering, and full-dimension SQ8 (`int8`) vector matrix (`100% Recall@5`, `~2 ms` full-corpus scan).
- **100% Corpus GPU Indexing (`serena-go index --embed`):** 4-worker parallel `batch=32` embedder with `encoding_format: "base64"` (~300 chunks/s), model-aware prompt prefixes (`nomic`, `qwen3`, `e5`), and automatic vector cache invalidation on model/dimension switches.
- **Multilingual & Thai Tokenizer:** Preserves `unicode.Mn` combining marks, generates 3–4 grapheme Thai n-grams, adds lightweight English stemming, test-file dampener (`0.55×`), and parent/child span deduplication.
- **Privacy & Completeness Guardrails:** Refuses to crawl/embed `$HOME`, `/`, `/tmp`, `/var`, `/etc`, `/Volumes`; honors `.gitignore` via `rg --files`; reports skipped/capped counts transparently.
- **Upgraded `code_context_pack`:** Supports batch `symbols: [...]` with deduplicated types/diagnostics, parallel stage execution, `tsconfig.json` path aliases (`@/*`, `@app/*`), Go intra-repo imports, same-file definitions, compact `struct`/`interface` bodies (up to 15 lines), outgoing local callees, production-first callers, and warm LSP hover.
- **`changed_symbols` MCP Tool:** Maps `git diff`/staged hunks to affected AST symbols, callers, and live diagnostics in 1 call.
- **Cross-Client `serena-go hook` CLI:** Zero-`jq` `<6 ms` `post-edit` (including Codex `apply_patch`), `session-brief`, and `remind` hooks for Claude Code, Antigravity, Codex, and Grok.
- **Token Economy & Build Size:** `--profile slim` (15 high-leverage tools), MCP tool `annotations` (`readOnlyHint`, `destructiveHint`, `idempotentHint`), tiered disclosure in `semantic_code_search` (~45% fewer output tokens), compact `get_symbols_overview`, and curated `grammar_subset` build tags shrinking the static binary from `33 MB` to `19 MB` (-42%).

### Fixed
- **Safety & UTF-8:** Eliminated mid-character UTF-8 byte slicing (`truncateUTF8`), removed fuzzy-substring fallback from mutating symbol tools (`replace`/`insert`/`delete`/`rename`), added per-file write mutex for concurrent edits, and added exact UTF-16↔UTF-8 LSP offset conversion + `positionEncodings` negotiation.
- **AST & Search Accuracy:** Fixed `find_symbol(..., substring_matching=true)` case-insensitive ripgrep (`-i`) and non-empty miss output; fixed Go generic `[K, V]` receivers, Go `var_spec`/struct fields/interface methods/single `const`/`var` spans, TS `abstract`/`namespace`/`enum`, Python `@decorator` signatures & top-level constants, Rust `impl<T>` & trait methods, and Swift `struct`/`enum` classification.
- **Speed:** `check-file` now uses Go stdlib `go/parser` (`<1 ms`) and skips redundant TS tree-sitter parsing when `oxlint` runs (`~50 ms`); LSP `syncFile` deduplicates unchanged writes via SHA-256 and invalidates stale diagnostics on edits; remote SSH preflight cached 5 min with 20-min remote idle TTL.
- **OSS Hygiene:** Removed stray personal hardware string from `semantic_code_search` schema description, pruned dead schema params, and made `Makefile install` symlink-safe.

## [0.1.0] - 2026-11-27

### Added
- Initial Pure-Go (`CGO_ENABLED=0`) 3-Tier Code-Intelligence MCP Server compatible with Serena MCP tools and `.serena/memories/`.
