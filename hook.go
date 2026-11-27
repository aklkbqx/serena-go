package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var reApplyPatchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add) File:\s*(.+?)\s*$`)

func runHookCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: serena-go hook <post-edit|session-brief|remind> [--client=claude|agy|codex|grok]")
		return 2
	}
	sub := args[0]
	client := "claude"
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--client=") {
			client = strings.ToLower(strings.TrimPrefix(a, "--client="))
		}
	}
	switch sub {
	case "post-edit":
		return runHookPostEdit(client)
	case "session-brief":
		return runHookSessionBrief(client)
	case "remind":
		return runHookRemind(client)
	default:
		fmt.Fprintf(os.Stderr, "unknown hook %q\n", sub)
		return 2
	}
}

func emitHookContext(client, eventName, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	switch client {
	case "claude", "codex", "grok":
		b, _ := json.Marshal(map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":     eventName,
				"additionalContext": text,
			},
		})
		fmt.Println(string(b))
	default:
		// Agy / generic plain stdout
		fmt.Println(text)
	}
}

func extractPathsFromHookPayload(raw []byte, cwd string) []string {
	var paths []string
	seen := map[string]bool{}
	addPath := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if c, ok := obj["cwd"].(string); ok && c != "" {
			cwd = c
		}
		inspectMap := func(m map[string]any) {
			for _, k := range []string{"file_path", "TargetFile", "path", "Path", "relative_path"} {
				if v, ok := m[k].(string); ok && v != "" {
					addPath(v)
				}
			}
			for _, k := range []string{"patch", "input", "diff", "content"} {
				if v, ok := m[k].(string); ok && strings.Contains(v, "*** ") {
					for _, pm := range reApplyPatchFile.FindAllStringSubmatch(v, 10) {
						addPath(pm[1])
					}
				}
			}
		}
		inspectMap(obj)
		if ti, ok := obj["tool_input"].(map[string]any); ok {
			inspectMap(ti)
		}
		if args, ok := obj["arguments"].(map[string]any); ok {
			inspectMap(args)
		}
	}
	// Raw fallback for non-JSON patch strings
	if len(paths) == 0 && strings.Contains(string(raw), "*** ") {
		for _, pm := range reApplyPatchFile.FindAllStringSubmatch(string(raw), 10) {
			addPath(pm[1])
		}
	}
	return paths
}

func isHookCheckableExt(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts",
		".go", ".py", ".rs", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".m", ".mm":
		return true
	}
	return false
}

func runHookPostEdit(client string) int {
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 512*1024))
	if len(raw) == 0 {
		return 0
	}
	cwd, _ := os.Getwd()
	paths := extractPathsFromHookPayload(raw, cwd)
	if len(paths) == 0 {
		return 0
	}
	var diags []string
	checked := 0
	for _, p := range paths {
		if !isHookCheckableExt(p) {
			continue
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			continue
		}
		if d := checkFileQuick(canonicalPath(p)); d != "" {
			diags = append(diags, d)
		}
		checked++
		if checked >= 3 {
			break
		}
	}
	if len(diags) == 0 {
		return 0
	}
	msg := "[serena-go instant diagnostics]\n" + strings.Join(diags, "\n")
	emitHookContext(client, "PostToolUse", msg)
	return 0
}

func runHookSessionBrief(client string) int {
	cwd, _ := os.Getwd()
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if len(raw) > 0 {
		var obj map[string]any
		if json.Unmarshal(raw, &obj) == nil {
			if c, ok := obj["cwd"].(string); ok && c != "" {
				cwd = c
			}
		}
	}
	repoRoot := findGitRoot(cwd)
	if repoRoot == "" {
		return 0
	}
	// Never emit repo brief for home directory
	if isForbiddenIndexRoot(repoRoot) {
		return 0
	}

	var parts []string
	name := filepath.Base(repoRoot)
	branch := ""
	if out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(out))
	}
	dirtyCnt := 0
	if out, err := exec.Command("git", "-C", repoRoot, "status", "--porcelain").Output(); err == nil {
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(ln) != "" {
				dirtyCnt++
			}
		}
	}
	header := fmt.Sprintf("[Repo Brief: %s", name)
	if branch != "" {
		header += fmt.Sprintf(" | branch=%s dirty=%d", branch, dirtyCnt)
	}
	header += "]"
	parts = append(parts, header)

	// Detect stack & test commands
	var stack, cmds []string
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(repoRoot, rel)); return err == nil }
	if exists("go.mod") {
		stack = append(stack, "Go")
		cmds = append(cmds, "go test ./...")
	}
	if exists("package.json") {
		stack = append(stack, "TS/Node")
		if exists("bun.lock") || exists("bun.lockb") {
			cmds = append(cmds, "bun test")
		}
	}
	if exists("Cargo.toml") {
		stack = append(stack, "Rust")
	}
	if exists("pyproject.toml") || exists("requirements.txt") {
		stack = append(stack, "Python")
	}
	if exists("Makefile") {
		cmds = append(cmds, "Makefile")
	}
	if len(stack) > 0 {
		parts = append(parts, "Stack: "+strings.Join(stack, ", "))
	}
	if len(cmds) > 0 {
		parts = append(parts, "Checks: "+strings.Join(cmds, " | "))
	}

	// Memories list
	memDir := filepath.Join(repoRoot, ".serena", "memories")
	if entries, err := os.ReadDir(memDir); err == nil {
		var mems []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				mems = append(mems, strings.TrimSuffix(e.Name(), ".md"))
			}
		}
		sort.Strings(mems)
		if len(mems) > 0 {
			if len(mems) > 8 {
				mems = append(mems[:8], fmt.Sprintf("+%d more", len(mems)-8))
			}
			parts = append(parts, "Memories: "+strings.Join(mems, ", "))
		}
	}

	parts = append(parts, "Serena-Go: prefer code_context_pack (supports batch symbols), semantic_code_search (intent queries), changed_symbols (git diff), and get_symbols_overview before reading full files.")
	emitHookContext(client, "SessionStart", strings.Join(parts, "\n"))
	return 0
}

func runHookRemind(client string) int {
	raw, _ := io.ReadAll(io.LimitReader(os.Stdin, 64*1024))
	if len(raw) == 0 {
		return 0
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return 0
	}
	toolName := strings.ToLower(strings.TrimSpace(argString(obj, "tool_name")))
	if toolName != "read" && toolName != "read_file" && toolName != "view_file" {
		return 0
	}
	cwd, _ := os.Getwd()
	if c, ok := obj["cwd"].(string); ok && c != "" {
		cwd = c
	}
	// If the caller already passed a bounded offset/limit or line range, do not nudge.
	if ti, ok := obj["tool_input"].(map[string]any); ok {
		if argInt(ti, "limit", 0) > 0 || argInt(ti, "EndLine", 0) > 0 || argInt(ti, "end_line", 0) > 0 {
			return 0
		}
	}
	paths := extractPathsFromHookPayload(raw, cwd)
	if len(paths) == 0 {
		return 0
	}
	p := paths[0]
	if !isHookCheckableExt(p) {
		return 0
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || fi.Size() < 8*1024 {
		return 0
	}
	// Throttle nudges to at most once every 90s per repo to avoid spamming context.
	repo := findGitRoot(filepath.Dir(p))
	if repo == "" {
		repo = filepath.Dir(p)
	}
	h := sha256.Sum256([]byte(repo))
	stamp := filepath.Join(os.TempDir(), fmt.Sprintf("serena-remind-%d-%s.ts", os.Getuid(), hex.EncodeToString(h[:])[:8]))
	if st, serr := os.Stat(stamp); serr == nil && time.Since(st.ModTime()) < 90*time.Second {
		return 0
	}
	_ = os.WriteFile(stamp, []byte("1"), 0o600)

	emitHookContext(client, "PreToolUse", fmt.Sprintf("Tip: %s is %.1f KB. Consider serena-go get_symbols_overview, code_context_pack, or semantic_code_search to save tokens instead of reading the whole file.", filepath.Base(p), float64(fi.Size())/1024))
	return 0
}
