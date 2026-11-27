package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type remoteLSPConfig struct {
	Mode       string `json:"mode,omitempty"`        // "auto" | "off" | "force"
	Host       string `json:"host,omitempty"`        // SSH host alias, e.g. "build-box"
	ProbeAddr  string `json:"probe_addr,omitempty"`  // fast TCP probe host:port, e.g. "192.0.2.10:22"
	RootPrefix string `json:"root_prefix,omitempty"` // remote workspace parent directory, e.g. "/srv/work"
}

type embeddingsConfig struct {
	URL          string `json:"url,omitempty"`
	Model        string `json:"model,omitempty"`
	AutoStartCmd string `json:"auto_start_cmd,omitempty"` // optional shell command to launch embedding server on demand
	AutoIndexBG  *bool  `json:"auto_index_bg,omitempty"`  // default true: auto-embed remaining chunks in background
	ActivityFile string `json:"activity_file,omitempty"`  // optional timestamp file touched on embed calls (for idle reaper)
}

type userConfig struct {
	RemoteLSP  remoteLSPConfig  `json:"remote_lsp,omitempty"`
	Embeddings embeddingsConfig `json:"embeddings,omitempty"`
	LegacyBin  string           `json:"legacy_bin,omitempty"`
}

func configFileCandidates() []string {
	if explicit := strings.TrimSpace(os.Getenv("SERENA_CONFIG")); explicit != "" {
		if strings.EqualFold(explicit, "none") || strings.EqualFold(explicit, "off") {
			return nil
		}
		return []string{explicit}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	return []string{
		filepath.Join(xdg, "serena-go", "config.json"),
		filepath.Join(xdg, "serena-shim", "config.json"),
	}
}

func loadUserConfig() (userConfig, string) {
	var cfg userConfig
	for _, p := range configFileCandidates() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if json.Unmarshal(raw, &cfg) == nil {
			return cfg, p
		}
	}
	return cfg, ""
}

// shellSingleQuote safely wraps an arbitrary string for POSIX sh execution,
// preventing $(...), backticks, $VAR expansion, and whitespace word-splitting.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
