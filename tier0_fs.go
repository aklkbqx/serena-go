package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const defaultMaxAnswerChars = 16000

func truncateAnswer(s string, maxChars int) string {
	if maxChars <= 0 {
		maxChars = defaultMaxAnswerChars
	}
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + fmt.Sprintf("\n... [truncated at %d chars; narrow relative_path or increase max_answer_chars]", maxChars)
}

func evalSymlinksNearestAncestor(path string) string {
	cur := filepath.Clean(path)
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

func resolveRepoPath(repoRoot, rel string) (string, error) {
	if repoRoot == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		repoRoot = canonicalPath(cwd)
	}
	rootClean := filepath.Clean(repoRoot)
	var target string
	if rel == "" || rel == "." {
		target = rootClean
	} else if filepath.IsAbs(rel) {
		target = filepath.Clean(rel)
	} else {
		target = filepath.Clean(filepath.Join(rootClean, rel))
	}
	// Resolve symlinks up to nearest existing ancestor to prevent escapes via nested paths.
	evalTarget := evalSymlinksNearestAncestor(target)
	evalRoot := evalSymlinksNearestAncestor(rootClean)
	relCheck, err := filepath.Rel(evalRoot, evalTarget)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes repository root %q", rel, repoRoot)
	}
	return target, nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".serena-edit-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if perm == 0 {
		perm = 0o644
	}
	_ = os.Chmod(tmpName, perm)
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func memoryFilePath(repoRoot, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("memory_name is required")
	}
	name = strings.TrimPrefix(name, "/")
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	memRoot := filepath.Join(repoRoot, ".serena", "memories")
	full := filepath.Clean(filepath.Join(memRoot, name))
	evalRoot := evalSymlinksNearestAncestor(memRoot)
	evalFull := evalSymlinksNearestAncestor(full)
	rel, err := filepath.Rel(evalRoot, evalFull)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid memory_name %q", name)
	}
	return full, nil
}

func toolInitialInstructions() string {
	return strings.TrimSpace(`Serena Pure-Go 3-Tier Engine (0-RAM idle, zero Python):
1. Prefer code_context_pack(symbol, relative_path) for 1-shot definition + imported types + callers + diagnostics.
2. Tier 1 AST (instant, 0 MB idle): get_symbols_overview, find_symbol, replace_symbol_body, insert_before/after_symbol.
3. Tier 2 Semantic LSP (lazy per-language, shared across agents): find_referencing_symbols, find_declaration, find_implementations, rename_symbol, get_diagnostics_for_file, safe_delete_symbol.
4. Tier 0 FS (instant, 0 MB): list/read/write/edit/delete/rename_memory, search_for_pattern, replace_content/in_files.`)
}

func toolOnboarding(repoRoot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s (%s)\n", filepath.Base(repoRoot), repoRoot)
	var stacks []string
	checks := []struct{ file, label string }{
		{"package.json", "node/ts"},
		{"frontend/package.json", "frontend:ts"},
		{"backend/package.json", "backend:ts"},
		{"go.mod", "go"},
		{"backend/go.mod", "backend:go"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{".fleet.json", "fleet"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(repoRoot, c.file)); err == nil {
			stacks = append(stacks, c.label)
		}
	}
	if len(stacks) > 0 {
		fmt.Fprintf(&b, "Stacks: %s\n", strings.Join(stacks, ", "))
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "graft")); err == nil {
		b.WriteString("Graft: indexed (prefer graft_find_code / graft_file_api / graft_trace_calls)\n")
	}
	mems := listMemoryNames(repoRoot, "")
	if len(mems) > 0 {
		fmt.Fprintf(&b, "Memories (%d): %s\n", len(mems), strings.Join(mems, ", "))
	} else {
		b.WriteString("Memories: none\n")
	}
	return strings.TrimSpace(b.String())
}

func listMemoryNames(repoRoot, topic string) []string {
	memRoot := filepath.Join(repoRoot, ".serena", "memories")
	var out []string
	_ = filepath.WalkDir(memRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(memRoot, path)
		if err != nil {
			return nil
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		if topic != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(topic)) {
			return nil
		}
		out = append(out, name)
		return nil
	})
	sort.Strings(out)
	return out
}

func toolListMemories(repoRoot string, args map[string]any) (string, error) {
	topic := argString(args, "topic")
	mems := listMemoryNames(repoRoot, topic)
	raw, _ := json.Marshal(mems)
	return string(raw), nil
}

func toolReadMemory(repoRoot string, args map[string]any) (string, error) {
	p, err := memoryFilePath(repoRoot, argString(args, "memory_name"))
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("memory %q not found (available: %s)", argString(args, "memory_name"), strings.Join(listMemoryNames(repoRoot, ""), ", "))
		}
		return "", err
	}
	return string(data), nil
}

func toolWriteMemory(repoRoot string, args map[string]any) (string, error) {
	p, err := memoryFilePath(repoRoot, argString(args, "memory_name"))
	if err != nil {
		return "", err
	}
	content := argString(args, "content")
	if maxC := argInt(args, "max_chars", 0); maxC > 0 && len(content) > maxC {
		return "", fmt.Errorf("content length %d exceeds max_chars %d", len(content), maxC)
	}
	if err := atomicWriteFile(p, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Memory %q written (%d bytes).", argString(args, "memory_name"), len(content)), nil
}

func applyTextReplace(src, needle, repl, mode string, allowMulti bool) (string, int, error) {
	if needle == "" {
		return "", 0, errors.New("needle must be non-empty")
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "literal":
		cnt := strings.Count(src, needle)
		if cnt == 0 {
			return "", 0, errors.New("needle not found")
		}
		if cnt > 1 && !allowMulti {
			return "", cnt, fmt.Errorf("needle matched %d times; set allow_multiple_occurrences=true or narrow needle", cnt)
		}
		return strings.ReplaceAll(src, needle, repl), cnt, nil
	case "regex":
		re, err := regexp.Compile(needle)
		if err != nil {
			return "", 0, fmt.Errorf("invalid regex: %w", err)
		}
		locs := re.FindAllStringIndex(src, -1)
		cnt := len(locs)
		if cnt == 0 {
			return "", 0, errors.New("regex needle did not match")
		}
		if cnt > 1 && !allowMulti {
			return "", cnt, fmt.Errorf("regex matched %d times; set allow_multiple_occurrences=true or narrow regex", cnt)
		}
		return re.ReplaceAllString(src, repl), cnt, nil
	default:
		return "", 0, fmt.Errorf("unsupported mode %q (expected literal or regex)", mode)
	}
}

func toolEditMemory(repoRoot string, args map[string]any) (string, error) {
	p, err := memoryFilePath(repoRoot, argString(args, "memory_name"))
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	updated, cnt, err := applyTextReplace(string(data), argString(args, "needle"), argString(args, "repl"), argString(args, "mode"), argBool(args, "allow_multiple_occurrences", false))
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(p, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Memory %q updated (%d replacement(s)).", argString(args, "memory_name"), cnt), nil
}

func toolDeleteMemory(repoRoot string, args map[string]any) (string, error) {
	p, err := memoryFilePath(repoRoot, argString(args, "memory_name"))
	if err != nil {
		return "", err
	}
	if err := os.Remove(p); err != nil {
		return "", err
	}
	return fmt.Sprintf("Memory %q deleted.", argString(args, "memory_name")), nil
}

func toolRenameMemory(repoRoot string, args map[string]any) (string, error) {
	oldP, err := memoryFilePath(repoRoot, argString(args, "old_name"))
	if err != nil {
		return "", err
	}
	newP, err := memoryFilePath(repoRoot, argString(args, "new_name"))
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(newP), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(oldP, newP); err != nil {
		return "", err
	}
	return fmt.Sprintf("Memory renamed %q -> %q.", argString(args, "old_name"), argString(args, "new_name")), nil
}

func findRipgrep() string {
	for _, c := range []string{"/opt/homebrew/bin/rg", "/usr/local/bin/rg", "rg"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

func toolSearchForPattern(repoRoot string, args map[string]any) (string, error) {
	pat := argString(args, "substring_pattern")
	if pat == "" {
		return "", errors.New("substring_pattern is required")
	}
	target, err := resolveRepoPath(repoRoot, argString(args, "relative_path"))
	if err != nil {
		return "", err
	}
	rgBin := findRipgrep()
	if rgBin == "" {
		return "", errors.New("ripgrep (rg) not found in PATH")
	}

	rgArgs := []string{"--line-number", "--no-heading", "--color", "never", "--max-count", "50", "--max-filesize", "1M"}
	if !argBool(args, "skip_ignored_files", true) {
		rgArgs = append(rgArgs, "--no-ignore")
	}
	if argBool(args, "multiline", false) {
		rgArgs = append(rgArgs, "-U", "--multiline-dotall")
	}
	if cb := argInt(args, "context_lines_before", 0); cb > 0 {
		rgArgs = append(rgArgs, "-B", fmt.Sprintf("%d", cb))
	}
	if ca := argInt(args, "context_lines_after", 0); ca > 0 {
		rgArgs = append(rgArgs, "-A", fmt.Sprintf("%d", ca))
	}
	if inc := argString(args, "paths_include_glob"); inc != "" {
		rgArgs = append(rgArgs, "-g", inc)
	}
	if exc := argString(args, "paths_exclude_glob"); exc != "" {
		rgArgs = append(rgArgs, "-g", "!"+strings.TrimPrefix(exc, "!"))
	}
	if argBool(args, "restrict_search_to_code_files", false) {
		for _, g := range []string{"*.go", "*.ts", "*.tsx", "*.js", "*.jsx", "*.rs", "*.py", "*.swift", "*.c", "*.cpp", "*.h", "*.hpp", "*.java", "*.kt", "*.sql"} {
			rgArgs = append(rgArgs, "-g", g)
		}
	}
	rgArgs = append(rgArgs, "-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*", "-e", pat, target)

	cmd := exec.Command(rgBin, rgArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "No matches found.", nil
		}
		return "", fmt.Errorf("rg failed: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	// Make paths relative to repoRoot for compact token output.
	out := strings.ReplaceAll(stdout.String(), repoRoot+string(filepath.Separator), "")
	return truncateAnswer(strings.TrimSpace(out), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

func toolReplaceContent(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	target, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	updated, cnt, err := applyTextReplace(string(data), argString(args, "needle"), argString(args, "repl"), argString(args, "mode"), argBool(args, "allow_multiple_occurrences", false))
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(target, []byte(updated), info.Mode().Perm()); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Replaced %d occurrence(s) in %s.", cnt, rel)
	if diag := checkSyntaxQuick(target, []byte(updated)); diag != "" {
		msg += "\n[Syntax Warning]\n" + diag
	}
	return msg, nil
}

func toolReplaceInFiles(repoRoot string, args map[string]any) (string, error) {
	needle := argString(args, "needle")
	repl := argString(args, "repl")
	mode := argString(args, "mode")
	dryRun := argBool(args, "dry_run", false)
	expected := argInt(args, "expected_count", -1)
	target, err := resolveRepoPath(repoRoot, argString(args, "relative_path"))
	if err != nil {
		return "", err
	}
	rgBin := findRipgrep()
	if rgBin == "" {
		return "", errors.New("ripgrep (rg) not found in PATH")
	}
	rgArgs := []string{"-l", "--color", "never", "-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*"}
	if strings.ToLower(strings.TrimSpace(mode)) == "literal" || mode == "" {
		rgArgs = append(rgArgs, "-F")
	}
	if inc := argString(args, "paths_include_glob"); inc != "" {
		rgArgs = append(rgArgs, "-g", inc)
	}
	if exc := argString(args, "paths_exclude_glob"); exc != "" {
		rgArgs = append(rgArgs, "-g", "!"+strings.TrimPrefix(exc, "!"))
	}
	rgArgs = append(rgArgs, "-e", needle, target)
	out, err := exec.Command(rgBin, rgArgs...).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "No matching files found.", nil
		}
		return "", err
	}
	files := strings.Split(strings.TrimSpace(string(out)), "\n")
	type plan struct {
		path    string
		rel     string
		perm    os.FileMode
		updated string
		count   int
	}
	var plans []plan
	total := 0
	for _, f := range files {
		if strings.TrimSpace(f) == "" {
			continue
		}
		info, err := os.Stat(f)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		updated, cnt, err := applyTextReplace(string(data), needle, repl, mode, true)
		if err != nil || cnt == 0 {
			continue
		}
		rel, _ := filepath.Rel(repoRoot, f)
		plans = append(plans, plan{path: f, rel: rel, perm: info.Mode().Perm(), updated: updated, count: cnt})
		total += cnt
	}
	if expected >= 0 && total != expected {
		return "", fmt.Errorf("expected_count=%d but found %d occurrence(s) across %d file(s); aborted", expected, total, len(plans))
	}
	var b strings.Builder
	if dryRun {
		fmt.Fprintf(&b, "Dry run: %d occurrence(s) across %d file(s):\n", total, len(plans))
		for _, p := range plans {
			fmt.Fprintf(&b, "- %s (%d)\n", p.rel, p.count)
		}
		return truncateAnswer(strings.TrimSpace(b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
	}
	for _, p := range plans {
		if err := atomicWriteFile(p.path, []byte(p.updated), p.perm); err != nil {
			return "", fmt.Errorf("write %s: %w", p.rel, err)
		}
		fmt.Fprintf(&b, "- %s (%d)\n", p.rel, p.count)
	}
	header := fmt.Sprintf("Replaced %d occurrence(s) across %d file(s):\n", total, len(plans))
	return truncateAnswer(strings.TrimSpace(header+b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func argBool(args map[string]any, key string, def bool) bool {
	if args == nil {
		return def
	}
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

func argIntSet(args map[string]any, key string) map[int]bool {
	if args == nil {
		return nil
	}
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	slice, ok := v.([]any)
	if !ok || len(slice) == 0 {
		return nil
	}
	res := make(map[int]bool, len(slice))
	for _, item := range slice {
		if f, ok := item.(float64); ok {
			res[int(f)] = true
		}
	}
	return res
}
