# Security Policy

## Trust Model

`serena-go` runs locally as a stdio MCP server under the invoking user's OS privileges. It is designed with defense-in-depth boundaries against prompt-injected or malformed MCP tool arguments:

1. **Workspace Path Containment:** All file and memory operations resolve symlinks up to the nearest existing ancestor (`evalSymlinksNearestAncestor`) and reject paths that escape the configured `--project` root (including `../` traversal and nested non-existent paths beneath symlinks pointing outside the repository).
2. **POSIX-Safe Remote SSH Quoting:** When optional remote SSH LSP offloading is configured, all remote paths and binary names are wrapped with POSIX single-quote escaping (`shellSingleQuote`), preventing `$(...)`, backtick, or `$VAR` expansion on the remote host.
3. **Opt-In Remote Network Access:** Remote SSH LSP offloading is disabled by default unless explicitly configured via environment variables or `~/.config/serena-go/config.json`. Default embedding discovery only probes loopback (`127.0.0.1:18081`) and never scans LAN subnets.
4. **Local IPC & Cache Isolation:** Shared LSP daemon sockets and metadata live in user-private `0700` directories; semantic index caches are written atomically with `0600` permissions.

## Reporting a Vulnerability

Please do not open a public issue for security vulnerabilities. Report suspected vulnerabilities privately via GitHub Security Advisories on `aklkbqx/serena-go` with reproduction steps and impact details.
