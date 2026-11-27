# Architecture Deep Dive

`serena-go` avoids spawning heavy Python runtimes or cold-booting Language Servers for routine code navigation by routing every MCP tool call to the cheapest tier capable of answering accurately.

## Three-Tier Execution Model

### Tier 0 — Native Go Filesystem & Ripgrep (`tier0_fs.go`)
- Handles `.serena/memories/*.md` CRUD, regex search (`rg` with Go fallback), and atomic content edits.
- Consumes `0 MB` extra RAM and completes in `<3 ms`.
- Enforces symlink-resolved workspace containment (`evalSymlinksNearestAncestor`) on every path.

### Tier 1 — Stateless AST, V2 `mmap` Index & Diff Symbols (`tier1_ast.go`, `context_pack.go`, `sidx_mmap.go`, `semantic_search.go`, `changed_symbols.go`)
- Uses `github.com/odvcencio/gotreesitter` (`ParseFilePooled`, pure Go, `CGO_ENABLED=0`, compiled with curated `grammar_subset` tags for a lean `~19 MB` binary) with a tiny 32-file mtime cache and idle `PurgeEmbeddedLanguageCache()` (lock-free tokenization hot path).
- **`code_context_pack`** (supports single or batch `symbols: [...]`) executes stages in parallel: definition body, same-file + imported type bodies (resolving `tsconfig.json` `@/*` aliases and Go intra-repo module imports), outgoing local callees, production-first callers, warm LSP `hover`, and quick diagnostics.
- **`changed_symbols`** parses `git diff`/staged hunks and maps them to affected AST symbols, callers, and live diagnostics.
- **`semantic_code_search`** uses a V2 Pure-Go read-only `syscall.Mmap` binary index (`os.UserCacheDir()/serena-index/<repoHash>.sidx`) shared across concurrent MCP sessions (`0` duplicated RSS):
  - Precomputed inverted posting list + symbol table (`<6 ms` BM25, `<1 ms` exact `find_symbol`) with Thai `unicode.Mn` + 3–4 grapheme n-gram tokenization and English stemming.
  - Full-dimension SQ8 (`int8`) flat vector matrix (`100% Recall@5`, `~2 ms` full-corpus scan) populated on the fly or 100% via `serena-go index --embed` (4-worker `batch=32` `base64`), merged with BM25 via Dual-Channel Reciprocal Rank Fusion (RRF).
  - Refuses to crawl `$HOME` / `/` / `/tmp` / `/Volumes`, honors `.gitignore` via `rg --files`, and reports skipped/capped file counts transparently.

### Tier 2 — Shared Lazy LSP Supervisor (`tier2_lsp.go`, `remote_lsp.go`)
- Spawns at most one background supervisor per `(canonicalRepoRoot, lang)` over a user-private Unix domain socket (`0700`). Multiple concurrent AI sessions share the exact same warm LSP instance.
- **OS Memory-Pressure Governance (`pressure_darwin.go`, `pressure_linux.go`, `pressure_common.go`):**
  - `NORMAL (1)`: up to `2` local LSPs, `180s` idle reap timeout.
  - `WARN (2)`: capped at `1` local LSP, `45s` idle reap timeout.
  - `CRITICAL (4)`: `0` local LSPs (active local LSPs evicted immediately; Tier 0/1 AST & remote LSPs keep running).
- **Opt-In Remote SSH LSP Offload:** When configured via env or `config.json`, `resolveRemoteLSPTarget` performs a `<250 ms` TCP reachability probe and fast SSH preflight. File contents are synced directly from local disk via `textDocument/didOpen` / `didChange`, and all returned `Location` and `WorkspaceEdit` URIs are translated bidirectionally between remote and local paths.
