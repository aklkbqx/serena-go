# Optional SSH Remote LSP Offload

Offloading Tier 2 Language Servers (`gopls`, `vtsls`, `rust-analyzer`, `ty`) to a higher-RAM workstation or Linux/WSL2 server reduces local LSP memory usage to **`0 MB`** while keeping `<250 ms` automatic fallback to local LSP whenever the remote machine is offline.

## Prerequisites

1. Passwordless SSH alias configured in `~/.ssh/config` (e.g. `Host build-box`).
2. Your repositories synced to a predictable parent directory on the remote host (e.g. `/srv/work/<repo-name>` via Mutagen, Syncthing, or rsync).
3. Desired LSP binaries installed on the remote host (`gopls`, `vtsls`, `rust-analyzer`, `ty`).

## Configuration

Add to `~/.config/serena-go/config.json`:

```json
{
  "remote_lsp": {
    "mode": "auto",
    "host": "build-box",
    "probe_addr": "192.0.2.10:22",
    "root_prefix": "/srv/work"
  }
}
```

Or use environment variables:

```bash
export SERENA_REMOTE_LSP=auto
export SERENA_REMOTE_LSP_HOST=build-box
export SERENA_REMOTE_LSP_PROBE_ADDR=192.0.2.10:22
export SERENA_REMOTE_LSP_ROOT_PREFIX=/srv/work
```

## Verification & Failover

Run `serena-go doctor` to confirm remote reachability. During operation:
- If `probe_addr` fails to connect within `250 ms`, or the remote repo directory / LSP binary is missing, `serena-go` transparently falls back to local LSP.
- Local unsaved or newly edited files are sent directly via LSP `didOpen`/`didChange`, preventing stale results even if file sync lags.
- Set `SERENA_REMOTE_LSP=off` at any time to force local execution.
