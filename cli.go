package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

var (
	buildVersion = "0.1.0-dev"
	buildCommit  = "local"
)

func printVersion() {
	fmt.Printf("serena-go %s (%s, %s/%s)\n", buildVersion, buildCommit, runtime.GOOS, runtime.GOARCH)
}

func printUsage() {
	fmt.Print(`serena-go — Pure-Go 3-Tier Code-Intelligence MCP Server

Usage:
  serena-go start-mcp-server [--project <dir>]   Start stdio MCP server (Serena-compatible)
  serena-go check-file <path>                    Fast AST + linter + warm-LSP diagnostics
  serena-go doctor                               Inspect AST, LSPs, remote offload, embeddings & RAM policy
  serena-go status                               List active shared LSP daemons
  serena-go stop-lsp                             Gracefully stop all active shared LSP daemons
  serena-go version                              Print version and build info

Environment & Config:
  SERENA_CONFIG                  Path to JSON config (default: ~/.config/serena-go/config.json)
  SERENA_MAX_LSPS                Max concurrent local LSP daemons (default: 2)
  SERENA_LSP_IDLE_SECS           Local LSP idle reap timeout in seconds (default: 180)
  SERENA_REMOTE_LSP              auto | off | force
  SERENA_REMOTE_LSP_HOST         Optional SSH host alias for remote LSP offload
  SERENA_REMOTE_LSP_PROBE_ADDR   Optional TCP probe host:port (250ms timeout)
  SERENA_REMOTE_LSP_ROOT_PREFIX  Remote workspace parent directory (e.g. /srv/work)
  SERENA_EMBED_URL               OpenAI-compatible /v1/embeddings endpoint or "off"
  SERENA_EMBED_MODEL             Embedding model name (default: nomic-embed-text)
`)
}

func runDoctor() int {
	fmt.Printf("serena-go %s (%s, %s/%s)\n", buildVersion, buildCommit, runtime.GOOS, runtime.GOARCH)
	cfg, cfgPath := loadUserConfig()
	if cfgPath != "" {
		fmt.Printf("Config:          %s\n", cfgPath)
	} else {
		fmt.Printf("Config:          defaults (create ~/.config/serena-go/config.json to customize)\n")
	}

	level := memoryPressureLevel()
	maxD, idleT := adaptiveLSPPolicy()
	fmt.Printf("Memory Pressure: level=%d (%s) -> local LSP cap=%d, idle=%s\n", level, memoryPressureName(level), maxD, idleT)

	fmt.Println("\nTier 1 (AST & Fast Linters):")
	fmt.Println("  [ok] gotreesitter (pure Go, zero CGO)")
	home, _ := os.UserHomeDir()
	for _, tool := range []struct {
		name      string
		fallbacks []string
	}{
		{"rg", nil},
		{"oxlint", []string{filepath.Join(home, ".bun/bin/oxlint"), filepath.Join(home, ".local/bin/oxlint")}},
		{"ty", []string{filepath.Join(home, ".local/bin/ty")}},
	} {
		if p := findToolBin(tool.name, tool.fallbacks...); p != "" {
			fmt.Printf("  [ok] %-10s %s\n", tool.name, p)
		} else {
			fmt.Printf("  [--] %-10s not found (optional)\n", tool.name)
		}
	}

	fmt.Println("\nTier 2 (Local Language Servers):")
	for _, lang := range []string{"ts", "go", "rust", "python", "swift", "cpp"} {
		if spec, err := resolveLSPSpec(lang); err == nil && len(spec.Cmd) > 0 {
			fmt.Printf("  [ok] %-8s %s\n", lang, strings.Join(spec.Cmd, " "))
		} else {
			fmt.Printf("  [--] %-8s %v\n", lang, err)
		}
	}

	fmt.Println("\nRemote LSP Offload:")
	host := remoteSSHHost()
	mode := remoteLSPMode()
	if mode == "off" || mode == "0" || mode == "false" || host == "" {
		fmt.Println("  [--] disabled (opt-in via SERENA_REMOTE_LSP_HOST or config.json)")
	} else {
		probe := remoteProbeAddr()
		prefix := remoteRootPrefix()
		probeStatus := "no TCP probe configured"
		if probe != "" {
			if tcpReachableFast(probe, 250*time.Millisecond) {
				probeStatus = "reachable (" + probe + ")"
			} else {
				probeStatus = "unreachable (" + probe + "; will fall back to local LSP)"
			}
		}
		fmt.Printf("  [ok] host=%s prefix=%s mode=%s [%s]\n", host, prefix, mode, probeStatus)
	}

	fmt.Println("\nHybrid GPU Semantic Embeddings:")
	if ep := resolveEmbedEndpoint(); ep != "" {
		model := strings.TrimSpace(os.Getenv("SERENA_EMBED_MODEL"))
		if model == "" {
			model = strings.TrimSpace(cfg.Embeddings.Model)
		}
		if model == "" {
			model = "nomic-embed-text"
		}
		fmt.Printf("  [ok] READY %s (model=%s)\n", ep, model)
	} else {
		fmt.Println("  [--] offline (using local Pure-Go AST-BM25; set SERENA_EMBED_URL to enable GPU vectors)")
	}
	return 0
}

func runStatus() int {
	stateDir := lspStateDir()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		fmt.Println("No active shared LSP daemons.")
		return 0
	}
	var live []*lspDaemonMeta
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fp := filepath.Join(stateDir, e.Name())
		m, err := readLSPMeta(fp)
		if err != nil {
			continue
		}
		if !pidAlive(m.PID) || !pidAlive(m.ChildPID) {
			_ = os.Remove(fp)
			_ = os.Remove(m.SockPath)
			continue
		}
		live = append(live, m)
	}
	if len(live) == 0 {
		fmt.Println("No active shared LSP daemons.")
		return 0
	}
	sort.Slice(live, func(i, j int) bool {
		return live[i].LastActivity > live[j].LastActivity
	})
	fmt.Printf("Active shared LSP daemons (%d):\n", len(live))
	now := time.Now()
	for _, m := range live {
		host := "local"
		if m.Remote && m.RemoteHost != "" {
			host = "remote@" + m.RemoteHost
		}
		idle := now.Sub(time.Unix(m.LastActivity, 0)).Round(time.Second)
		if idle < 0 {
			idle = 0
		}
		fmt.Printf("  [%s] %-16s pid=%-6d idle=%-6s repo=%s\n", m.Lang, host, m.PID, idle, m.RepoRoot)
	}
	return 0
}

func runStopLSP() int {
	stateDir := lspStateDir()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		fmt.Println("No active shared LSP daemons to stop.")
		return 0
	}
	stopped := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fp := filepath.Join(stateDir, e.Name())
		m, err := readLSPMeta(fp)
		if err != nil {
			_ = os.Remove(fp)
			continue
		}
		_, _ = dialLSPDaemon(m.SockPath, lspIPCReq{Action: "shutdown"}, 2*time.Second)
		killProcessGroup(m.PGID)
		if pidAlive(m.PID) {
			_ = syscall.Kill(m.PID, syscall.SIGTERM)
		}
		_ = os.Remove(m.SockPath)
		_ = os.Remove(fp)
		stopped++
	}
	fmt.Printf("Stopped %d shared LSP daemon(s).\n", stopped)
	return 0
}
