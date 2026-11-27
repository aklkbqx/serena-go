# Changelog

All notable changes to `serena-go` are documented in this file.

## [0.1.0] - 2026-11-27

### Added
- Pure-Go (`CGO_ENABLED=0`) 3-Tier Code-Intelligence MCP Server compatible with Serena MCP tools and `.serena/memories/`.
- **Tier 0:** Zero-RAM native Go memory CRUD, `rg` pattern search, and atomic file replacement.
- **Tier 1:** Stateless `gotreesitter` AST symbol overview, symbol lookup, surgical body replacement, relative insertion, 1-shot `code_context_pack`, and `check-file` fast diagnostics (`oxlint` + `ty` + warm-only LSP).
- **Tier 1+ Hybrid Semantic Search:** Incremental AST-chunked BM25 with code-aware tokenization + optional OpenAI-compatible `/v1/embeddings` GPU Vector Reciprocal Rank Fusion (RRF) with automatic sub-batching.
- **Tier 2:** Shared per-`(repo, lang)` lazy Unix-socket LSP supervisor (`vtsls`, `gopls`, `rust-analyzer`, `ty`, `sourcekit-lsp`, `clangd`) with OS memory-pressure LRU governance (macOS `memorystatus` & Linux `/proc/meminfo`).
- **Opt-In Remote SSH LSP Offload:** Fast TCP probe, POSIX single-quoted SSH invocation, bidirectional local/remote URI translation, and live local buffer synchronization.
- **CLI Utilities:** `doctor`, `status`, `stop-lsp`, `check-file`, `version`, and JSON configuration (`~/.config/serena-go/config.json`).
