package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type remoteLSPTarget struct {
	HostAlias  string // SSH host alias, e.g. "build-box"
	ProbeAddr  string // optional TCP probe host:port, e.g. "192.0.2.10:22"
	RemoteRoot string // remote repo root, e.g. "/srv/work/sample-repo"
	RemoteCmd  string // remote shell command to exec LSP
}

func remoteLSPMode() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("SERENA_REMOTE_LSP"))); v != "" {
		return v
	}
	cfg, _ := loadUserConfig()
	if v := strings.ToLower(strings.TrimSpace(cfg.RemoteLSP.Mode)); v != "" {
		return v
	}
	return "auto"
}

func remoteProbeAddr() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_REMOTE_LSP_PROBE_ADDR")); v != "" {
		return v
	}
	cfg, _ := loadUserConfig()
	return strings.TrimSpace(cfg.RemoteLSP.ProbeAddr)
}

func remoteSSHHost() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_REMOTE_LSP_HOST")); v != "" {
		return v
	}
	cfg, _ := loadUserConfig()
	return strings.TrimSpace(cfg.RemoteLSP.Host)
}

func remoteRootPrefix() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_REMOTE_LSP_ROOT_PREFIX")); v != "" {
		return strings.TrimRight(v, "/")
	}
	cfg, _ := loadUserConfig()
	if v := strings.TrimSpace(cfg.RemoteLSP.RootPrefix); v != "" {
		return strings.TrimRight(v, "/")
	}
	return ""
}

func tcpReachableFast(addr string, timeout time.Duration) bool {
	if addr == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

const remoteExportPATH = `export PATH="$HOME/.local/bin:$HOME/.bun/bin:$HOME/.cargo/bin:/usr/local/cargo/bin:/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"`

func remoteLSPCommandForLang(lang string) (binName string, execCmd string, ok bool) {
	switch lang {
	case "go":
		return "gopls", `exec gopls serve`, true
	case "ts":
		return "vtsls", `exec env NODE_OPTIONS=--max-old-space-size=2048 vtsls --stdio`, true
	case "rust":
		return "rust-analyzer", `exec rust-analyzer`, true
	case "python":
		return "ty", `exec ty server`, true
	default:
		// swift (sourcekit-lsp) and cpp stay local by default
		return "", "", false
	}
}

// resolveRemoteLSPTarget checks <250ms whether a configured remote SSH LSP host is reachable
// and has a synced workspace + LSP binary. Opt-in via env vars or config.json; returns nil
// immediately when unconfigured so Tier 2 runs locally with zero network probes.
func resolveRemoteLSPTarget(localRepoRoot, lang string) *remoteLSPTarget {
	mode := remoteLSPMode()
	if mode == "off" || mode == "0" || mode == "false" {
		return nil
	}
	// Never hijack tests or users that explicitly inject a local LSP binary override.
	if strings.TrimSpace(os.Getenv("SERENA_LSP_CMD_"+strings.ToUpper(lang))) != "" && mode != "force" {
		return nil
	}
	host := remoteSSHHost()
	if host == "" {
		return nil
	}
	binName, remoteCmd, ok := remoteLSPCommandForLang(lang)
	if !ok {
		return nil
	}
	probeAddr := remoteProbeAddr()
	if probeAddr != "" && !tcpReachableFast(probeAddr, 250*time.Millisecond) {
		return nil
	}

	remoteRoot := strings.TrimSpace(os.Getenv("SERENA_REMOTE_LSP_ROOT"))
	if remoteRoot == "" {
		prefix := remoteRootPrefix()
		if prefix == "" {
			return nil
		}
		base := filepath.Base(canonicalPath(localRepoRoot))
		if base == "" || base == "." || base == "/" {
			return nil
		}
		remoteRoot = prefix + "/" + base
	}

	// Check 5-minute disk cache for SSH preflight to avoid redundant SSH roundtrips.
	preflightKey := fmt.Sprintf("%s\n%s\n%s", host, remoteRoot, lang)
	if ok, cached := readRemotePreflightCache(preflightKey, 5*time.Minute); cached {
		if !ok {
			return nil
		}
		return &remoteLSPTarget{
			HostAlias:  host,
			ProbeAddr:  probeAddr,
			RemoteRoot: remoteRoot,
			RemoteCmd:  remoteCmd,
		}
	}

	// Single fast SSH preflight (<2s) confirming remote dir exists and binary is executable.
	checkScript := fmt.Sprintf(`%s; [ -d %s ] && command -v %s >/dev/null 2>&1 && echo OK`,
		remoteExportPATH,
		shellSingleQuote(remoteRoot),
		shellSingleQuote(binName),
	)
	cmd := exec.Command("ssh",
		"-o", "ConnectTimeout=2",
		"-o", "BatchMode=yes",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-T", host, checkScript,
	)
	out, err := cmd.Output()
	preflightOK := err == nil && strings.Contains(string(out), "OK")
	writeRemotePreflightCache(preflightKey, preflightOK)
	if !preflightOK {
		return nil
	}
	return &remoteLSPTarget{
		HostAlias:  host,
		ProbeAddr:  probeAddr,
		RemoteRoot: remoteRoot,
		RemoteCmd:  remoteCmd,
	}
}

func remotePreflightCachePath(key string) string {
	dir := lspStateDir()
	h := sha256.Sum256([]byte(key))
	return filepath.Join(dir, "_preflight_"+hex.EncodeToString(h[:])[:12]+".cache")
}

func readRemotePreflightCache(key string, ttl time.Duration) (ok bool, hit bool) {
	p := remotePreflightCachePath(key)
	info, err := os.Stat(p)
	if err != nil || time.Since(info.ModTime()) > ttl {
		return false, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return false, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "1" {
		return true, true
	}
	if s == "0" {
		return false, true
	}
	return false, false
}

func writeRemotePreflightCache(key string, ok bool) {
	dir := lspStateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	val := "0"
	if ok {
		val = "1"
	}
	_ = os.WriteFile(remotePreflightCachePath(key), []byte(val), 0o600)
}

func buildRemoteLSPSpec(lang string, target *remoteLSPTarget) *lspLangSpec {
	var initOpts map[string]any
	switch lang {
	case "go":
		initOpts = map[string]any{
			"memoryMode":       "DegradeClosed",
			"staticcheck":      false,
			"directoryFilters": []string{"-node_modules", "-.git", "-dist", "-build", "-.next", "-graft", "-Pods"},
		}
	case "ts":
		initOpts = map[string]any{
			"hostInfo": "serena-go-remote",
			"typescript": map[string]any{
				"tsserver": map[string]any{"maxTsServerMemory": 2048},
			},
			"vtsls": map[string]any{"autoUseWorkspaceTsdk": true},
		}
	case "rust":
		initOpts = map[string]any{
			"lru":         map[string]any{"capacity": 128},
			"checkOnSave": map[string]any{"enable": false},
		}
	}
	shellLine := fmt.Sprintf(`%s; cd %s && %s`, remoteExportPATH, shellSingleQuote(target.RemoteRoot), target.RemoteCmd)
	return &lspLangSpec{
		Lang: lang,
		Cmd: []string{
			"ssh",
			"-o", "ConnectTimeout=3",
			"-o", "BatchMode=yes",
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
			"-o", "ServerAliveInterval=15",
			"-o", "ServerAliveCountMax=2",
			"-T", target.HostAlias, shellLine,
		},
		InitOptions: initOpts,
	}
}

// --- Bidirectional Local <-> Remote Path & URI Translation ---

func mapLocalPathToRemote(localAbs, localRoot, remoteRoot string) string {
	if remoteRoot == "" {
		return localAbs
	}
	cLocal := canonicalPath(localAbs)
	cRoot := canonicalPath(localRoot)
	rel, err := filepath.Rel(cRoot, cLocal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return localAbs
	}
	if rel == "." {
		return remoteRoot
	}
	return strings.TrimRight(remoteRoot, "/") + "/" + filepath.ToSlash(rel)
}

func mapRemoteURIToLocalURI(rawURI, localRoot, remoteRoot string) string {
	if remoteRoot == "" || !strings.HasPrefix(rawURI, "file://") {
		return rawURI
	}
	p := filepath.ToSlash(uriToFile(rawURI))
	rRoot := strings.TrimRight(filepath.ToSlash(remoteRoot), "/")
	if p == rRoot {
		return fileToURI(canonicalPath(localRoot))
	}
	if strings.HasPrefix(p, rRoot+"/") {
		rel := strings.TrimPrefix(p, rRoot+"/")
		return fileToURI(filepath.Join(canonicalPath(localRoot), filepath.FromSlash(rel)))
	}
	return rawURI
}

func translateLocationsToLocal(locs []lspLocation, localRoot, remoteRoot string) []lspLocation {
	if remoteRoot == "" || len(locs) == 0 {
		return locs
	}
	out := make([]lspLocation, len(locs))
	for i, loc := range locs {
		loc.URI = mapRemoteURIToLocalURI(loc.URI, localRoot, remoteRoot)
		out[i] = loc
	}
	return out
}

func translateWorkspaceEditToLocal(raw json.RawMessage, localRoot, remoteRoot string) json.RawMessage {
	if remoteRoot == "" || len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return raw
	}
	if ch, ok := generic["changes"].(map[string]any); ok {
		newCh := make(map[string]any, len(ch))
		for k, v := range ch {
			newCh[mapRemoteURIToLocalURI(k, localRoot, remoteRoot)] = v
		}
		generic["changes"] = newCh
	}
	if dcs, ok := generic["documentChanges"].([]any); ok {
		for _, item := range dcs {
			if m, ok := item.(map[string]any); ok {
				if td, ok := m["textDocument"].(map[string]any); ok {
					if u, ok := td["uri"].(string); ok {
						td["uri"] = mapRemoteURIToLocalURI(u, localRoot, remoteRoot)
					}
				}
			}
		}
	}
	b, err := json.Marshal(generic)
	if err != nil {
		return raw
	}
	return b
}
