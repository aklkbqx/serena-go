# Contributing to `serena-go`

Thank you for your interest in contributing!

## Development Requirements

- Go 1.24+
- Optional for local LSP integration testing: `gopls`, `vtsls`, `ty`, `rust-analyzer`, `oxlint`

## Local Verification Workflow

Before opening a pull request, run the full local hygiene, test, and cross-compilation check:

```bash
make check
```

This runs `scripts/check-oss-hygiene.sh`, which verifies:
1. No hardcoded personal paths, LAN IPs, or secrets
2. `go vet ./...`
3. `go test -race -count=1 ./...`
4. Clean `CGO_ENABLED=0` cross-compilation across `darwin/arm64`, `darwin/amd64`, `linux/amd64`, and `linux/arm64`

## Guidelines

- Keep `CGO_ENABLED=0` intact (no CGO dependencies).
- Add regression tests for any AST, LSP URI translation, path containment, or hybrid search change.
- Follow Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `test:`).
- Keep default behavior zero-config and strictly local (all remote offload features must remain opt-in).
