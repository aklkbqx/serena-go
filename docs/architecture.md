# Architecture Deep Dive

`serena-go` avoids spawning heavy Python runtimes or cold-booting Language Servers for routine code navigation by routing every MCP tool call to the cheapest tier capable of answering accurately.

## Three-Tier Execution Model

### Tier 0 — Native Go Filesystem & Ripgrep (`tier0_fs.go`)
- Handles `.serena/memories/*.md` CRUD, regex search (`rg` with Go fallback), and atomic content edits.
- Consumes `0 MB` extra RAM and completes in `<3 ms`.
- Enforces symlink-resolved workspace containment (`evalSymlinksNearestAncestor`) on every path.

### Tier 1 — Stateless AST & Hybrid Search (`tier1_ast.go`, `context_pack.go`, `semantic_search.go`)
- Uses `github.com/odvcencio/gotreesitter` (pure Go, `CGO_ENABLED=0`) to parse files on demand and release AST memory immediately after returning.
- **`code_context_pack`** combines symbol definition body, 1-hop imported definitions/signatures, callers (warm LSP if available, else fast `rg`), and quick diagnostics into one compact response.
- **`semantic_code_search`** maintains an incremental content-hashed AST chunk cache in `os.UserCacheDir()/serena-index/<repoHash>.json`. It ranks chunks using code-aware CamelCase/snake_case BM25 + engineering domain synonyms, and when an OpenAI-compatible `/v1/embeddings` endpoint is live, embeds the query and uncached top-40 candidate chunks in safe sub-batches (`<=8`) and merges ranks via Reciprocal Rank Fusion (RRF).

### Tier 2 — Shared Lazy LSP Supervisor (`tier2_lsp.go`, `remote_lsp.go`)
- Spawns at most one background supervisor per `(canonicalRepoRoot, lang)` over a user-private Unix domain socket (`0700`). Multiple concurrent AI sessions share the exact same warm LSP instance.
- **OS Memory-Pressure Governance (`pressure_darwin.go`, `pressure_linux.go`, `pressure_common.go`):**
  - `NORMAL (1)`: up to `2` local LSPs, `180s` idle reap timeout.
  - `WARN (2)`: capped at `1` local LSP, `45s` idle reap timeout.
  - `CRITICAL (4)`: `0` local LSPs (active local LSPs evicted immediately; Tier 0/1 AST & remote LSPs keep running).
- **Opt-In Remote SSH LSP Offload:** When configured via env or `config.json`, `resolveRemoteLSPTarget` performs a `<250 ms` TCP reachability probe and fast SSH preflight. File contents are synced directly from local disk via `textDocument/didOpen` / `didChange`, and all returned `Location` and `WorkspaceEdit` URIs are translated bidirectionally between remote and local paths.
