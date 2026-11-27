package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
)

var (
	reTSImport = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?(?:\{([^}]+)\}|([A-Za-z0-9_$]+))\s+from\s+['"]([^'"]+)['"]`)
	rePyImport = regexp.MustCompile(`(?m)^\s*from\s+([.A-Za-z0-9_]+)\s+import\s+([^\n#]+)`)
	reGoImport = regexp.MustCompile(`(?m)^\s*(?:[A-Za-z0-9_.]+\s+)?"([^"]+)"`)
	reCallExpr = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

// tokenizeIdents extracts exact identifier words from source text (avoiding substring false positives).
func tokenizeIdents(s string) map[string]bool {
	out := map[string]bool{}
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			out[string(cur)] = true
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$' {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func resolveRelativeImportFile(fromFile, spec string) string {
	base := filepath.Join(filepath.Dir(fromFile), spec)
	return probeCodeFileCandidates(base)
}

func probeCodeFileCandidates(base string) string {
	candidates := []string{
		base,
		base + ".ts", base + ".tsx", base + ".mts", base + ".js", base + ".jsx", base + ".py",
		filepath.Join(base, "index.ts"),
		filepath.Join(base, "index.tsx"),
		filepath.Join(base, "index.js"),
		filepath.Join(base, "__init__.py"),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

type tsPathMapping struct {
	baseDir string
	paths   map[string][]string
}

var (
	tsConfigMu    sync.Mutex
	tsConfigCache = map[string]tsPathMapping{}
)

// stripJSONComments removes // and /* */ comments + trailing commas so tsconfig.json parses cleanly.
func stripJSONComments(raw string) string {
	var out strings.Builder
	inStr := false
	esc := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inStr {
			out.WriteByte(c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(raw) && raw[i+1] == '/' {
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			if i < len(raw) {
				out.WriteByte('\n')
			}
			continue
		}
		if c == '/' && i+1 < len(raw) && raw[i+1] == '*' {
			i += 2
			for i+1 < len(raw) && !(raw[i] == '*' && raw[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(c)
	}
	s := out.String()
	reTrailing := regexp.MustCompile(`,(\s*[}\]])`)
	return reTrailing.ReplaceAllString(s, "$1")
}

func loadNearestTSConfig(repoRoot, absFile string) tsPathMapping {
	dir := filepath.Dir(absFile)
	cRoot := canonicalPath(repoRoot)
	for {
		tsConfigMu.Lock()
		if cached, ok := tsConfigCache[dir]; ok {
			tsConfigMu.Unlock()
			return cached
		}
		tsConfigMu.Unlock()

		for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
			cfgPath := filepath.Join(dir, name)
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				continue
			}
			var parsed struct {
				CompilerOptions struct {
					BaseURL string              `json:"baseUrl"`
					Paths   map[string][]string `json:"paths"`
				} `json:"compilerOptions"`
			}
			if json.Unmarshal([]byte(stripJSONComments(string(raw))), &parsed) == nil {
				base := dir
				if parsed.CompilerOptions.BaseURL != "" {
					base = filepath.Join(dir, parsed.CompilerOptions.BaseURL)
				}
				m := tsPathMapping{baseDir: base, paths: parsed.CompilerOptions.Paths}
				tsConfigMu.Lock()
				tsConfigCache[dir] = m
				tsConfigMu.Unlock()
				return m
			}
		}
		if canonicalPath(dir) == cRoot || dir == "/" || dir == "." {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return tsPathMapping{}
}

func resolveTSImportFile(repoRoot, fromFile, spec string) string {
	if strings.HasPrefix(spec, ".") {
		return resolveRelativeImportFile(fromFile, spec)
	}
	// Resolve tsconfig paths aliases (e.g. @/*, @app/*, @components/*, @org/pkg/*)
	cfg := loadNearestTSConfig(repoRoot, fromFile)
	for pat, targets := range cfg.paths {
		if strings.HasSuffix(pat, "*") {
			prefix := strings.TrimSuffix(pat, "*")
			if strings.HasPrefix(spec, prefix) {
				rest := strings.TrimPrefix(spec, prefix)
				for _, t := range targets {
					tBase := strings.Replace(t, "*", rest, 1)
					if f := probeCodeFileCandidates(filepath.Join(cfg.baseDir, tBase)); f != "" {
						return f
					}
				}
			}
		} else if pat == spec {
			for _, t := range targets {
				if f := probeCodeFileCandidates(filepath.Join(cfg.baseDir, t)); f != "" {
					return f
				}
			}
		}
	}
	// Monorepo package fallback: packages/<pkg>/src/...
	if strings.HasPrefix(spec, "@") {
		parts := strings.Split(spec, "/")
		if len(parts) >= 2 {
			pkgName := parts[1]
			sub := "src"
			if len(parts) > 2 {
				sub = filepath.Join(append([]string{"src"}, parts[2:]...)...)
			}
			for _, rootDir := range []string{"packages", "libs", "apps"} {
				if f := probeCodeFileCandidates(filepath.Join(repoRoot, rootDir, pkgName, sub)); f != "" {
					return f
				}
			}
		}
	}
	return ""
}

func readGoModulePath(repoRoot, absFile string) (modDir, modPath string) {
	dir := filepath.Dir(absFile)
	cRoot := canonicalPath(repoRoot)
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for _, ln := range strings.Split(string(raw), "\n") {
				ln = strings.TrimSpace(ln)
				if strings.HasPrefix(ln, "module ") {
					return dir, strings.TrimSpace(strings.TrimPrefix(ln, "module "))
				}
			}
		}
		if canonicalPath(dir) == cRoot || dir == "/" || dir == "." {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", ""
}

// formatTypeEntry renders a compact body (up to 15 lines) for struct/interface/type definitions,
// or the signature for functions/consts, so the AI sees actual fields and method shapes.
func formatTypeEntry(rel string, s astSymbol, src []byte) string {
	if (s.Kind == "struct" || s.Kind == "interface" || s.Kind == "type" || s.Kind == "enum") &&
		int(s.EndByte) <= len(src) && s.StartByte < s.EndByte {
		body := strings.TrimSpace(string(src[s.StartByte:s.EndByte]))
		lines := strings.Split(body, "\n")
		if len(lines) > 15 {
			lines = append(lines[:14], "  // ...")
		}
		compact := strings.Join(lines, "\n")
		if len(compact) > 650 {
			compact = truncateUTF8(compact, 647) + "..."
		}
		return fmt.Sprintf("- %s:L%d [%s] %s\n  ```\n  %s\n  ```", rel, s.StartLine, s.Kind, s.NamePath, strings.ReplaceAll(compact, "\n", "\n  "))
	}
	return fmt.Sprintf("- %s:L%d [%s] %s", rel, s.StartLine, s.Kind, s.Signature)
}

func isTestCodePath(rel string) bool {
	low := strings.ToLower(rel)
	base := filepath.Base(low)
	return strings.HasSuffix(base, "_test.go") ||
		strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") ||
		strings.HasPrefix(base, "test_") ||
		strings.Contains(low, "/__tests__/") ||
		strings.Contains(low, "/tests/") ||
		strings.Contains(low, "/test/") ||
		strings.Contains(low, "/fixtures/") ||
		strings.Contains(low, "/mocks/")
}

func isNonCodePath(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".md", ".txt", ".json", ".yaml", ".yml", ".toml", ".lock", ".log", ".csv", ".svg", ".html", ".css":
		return true
	}
	return false
}

// extractImportedTypeSignatures resolves same-file definitions, TS relative & tsconfig aliases,
// Python relative imports, and Go same-package + intra-repo module imports using exact identifier matching.
func extractImportedTypeSignatures(repoRoot, absFile string, src []byte, selfSym astSymbol, selfFileSyms []astSymbol, bodyText string) []string {
	var out []string
	seen := map[string]bool{}
	bodyIdents := tokenizeIdents(bodyText)
	ext := strings.ToLower(filepath.Ext(absFile))

	selfRel, _ := filepath.Rel(repoRoot, absFile)
	selfRel = filepath.ToSlash(selfRel)

	// 0. Same-file definitions referenced inside bodyText (structs, interfaces, types, helpers)
	for _, s := range flattenSymbols(selfFileSyms) {
		if s.NamePath == selfSym.NamePath || len(s.Name) < 2 || !bodyIdents[s.Name] {
			continue
		}
		if s.Kind == "struct" || s.Kind == "interface" || s.Kind == "type" || s.Kind == "enum" || s.Kind == "class" || s.Kind == "function" {
			k := selfRel + ":" + s.NamePath
			if !seen[k] {
				seen[k] = true
				out = append(out, formatTypeEntry(selfRel, s, src))
			}
		}
	}

	if ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" || ext == ".mts" || ext == ".cts" {
		matches := reTSImport.FindAllStringSubmatch(string(src), 50)
		for _, m := range matches {
			spec := m[3]
			impFile := resolveTSImportFile(repoRoot, absFile, spec)
			if impFile == "" {
				continue
			}
			var idents []string
			if m[1] != "" {
				for _, part := range strings.Split(m[1], ",") {
					part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "type "))
					if part == "" {
						continue
					}
					localName := part
					origName := part
					if idx := strings.Index(part, " as "); idx >= 0 {
						origName = strings.TrimSpace(part[:idx])
						localName = strings.TrimSpace(part[idx+4:])
					}
					if localName != "" && bodyIdents[localName] {
						idents = append(idents, origName)
					}
				}
			} else if m[2] != "" && bodyIdents[m[2]] {
				idents = append(idents, m[2])
			}
			if len(idents) == 0 {
				continue
			}
			impSrc, err := os.ReadFile(impFile)
			if err != nil {
				continue
			}
			impRel, _ := filepath.Rel(repoRoot, impFile)
			impRel = filepath.ToSlash(impRel)
			syms, _, err := extractFileSymbolsCached(impFile, impRel, impSrc)
			if err != nil {
				continue
			}
			flat := flattenSymbols(syms)
			for _, id := range idents {
				for _, s := range flat {
					if s.Name == id && !seen[impRel+":"+s.Name] {
						seen[impRel+":"+s.Name] = true
						out = append(out, formatTypeEntry(impRel, s, impSrc))
					}
				}
			}
		}
	} else if ext == ".py" {
		matches := rePyImport.FindAllStringSubmatch(string(src), 30)
		for _, m := range matches {
			mod := m[1]
			dots := 0
			for dots < len(mod) && mod[dots] == '.' {
				dots++
			}
			rest := strings.ReplaceAll(mod[dots:], ".", string(filepath.Separator))
			baseDir := filepath.Dir(absFile)
			for i := 1; i < dots; i++ {
				baseDir = filepath.Dir(baseDir)
			}
			impFile := resolveRelativeImportFile(filepath.Join(baseDir, "_"), rest)
			if impFile == "" {
				continue
			}
			impSrc, err := os.ReadFile(impFile)
			if err != nil {
				continue
			}
			impRel, _ := filepath.Rel(repoRoot, impFile)
			impRel = filepath.ToSlash(impRel)
			syms, _, err := extractFileSymbolsCached(impFile, impRel, impSrc)
			if err != nil {
				continue
			}
			flat := flattenSymbols(syms)
			for _, part := range strings.Split(m[2], ",") {
				orig := strings.TrimSpace(part)
				local := orig
				if idx := strings.Index(orig, " as "); idx >= 0 {
					local = strings.TrimSpace(orig[idx+4:])
					orig = strings.TrimSpace(orig[:idx])
				}
				if orig == "" || !bodyIdents[local] {
					continue
				}
				for _, s := range flat {
					if s.Name == orig && !seen[impRel+":"+s.Name] {
						seen[impRel+":"+s.Name] = true
						out = append(out, formatTypeEntry(impRel, s, impSrc))
					}
				}
			}
		}
	} else if ext == ".go" {
		scanGoDir := func(dir string) {
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				sib := filepath.Join(dir, e.Name())
				if sib == absFile {
					continue
				}
				sibSrc, err := os.ReadFile(sib)
				if err != nil {
					continue
				}
				sibRel, _ := filepath.Rel(repoRoot, sib)
				sibRel = filepath.ToSlash(sibRel)
				syms, _, err := extractFileSymbolsCached(sib, sibRel, sibSrc)
				if err != nil {
					continue
				}
				for _, s := range flattenSymbols(syms) {
					if len(s.Name) < 3 || !bodyIdents[s.Name] {
						continue
					}
					if s.Kind == "struct" || s.Kind == "interface" || s.Kind == "type" || s.Kind == "function" {
						key := sibRel + ":" + s.NamePath
						if !seen[key] {
							seen[key] = true
							out = append(out, formatTypeEntry(sibRel, s, sibSrc))
						}
					}
				}
			}
		}
		// Same-package files
		scanGoDir(filepath.Dir(absFile))
		// Intra-repo Go module imports (only packages whose base alias appears in bodyIdents)
		if modDir, modPath := readGoModulePath(repoRoot, absFile); modPath != "" {
			for _, m := range reGoImport.FindAllStringSubmatch(string(src), 40) {
				imp := m[1]
				if !strings.HasPrefix(imp, modPath+"/") {
					continue
				}
				pkgName := filepath.Base(imp)
				if !bodyIdents[pkgName] {
					continue
				}
				subDir := filepath.Join(modDir, filepath.FromSlash(strings.TrimPrefix(imp, modPath+"/")))
				if fi, err := os.Stat(subDir); err == nil && fi.IsDir() {
					scanGoDir(subDir)
				}
			}
		}
	}

	sort.Strings(out)
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// extractOutgoingCallees finds top local/package functions called inside bodyText.
func extractOutgoingCallees(repoRoot, absFile string, selfSym astSymbol, selfFileSyms []astSymbol, bodyText string) []string {
	called := map[string]bool{}
	for _, m := range reCallExpr.FindAllStringSubmatch(bodyText, 80) {
		nm := m[1]
		if nm != selfSym.Name && len(nm) >= 2 && !codeStopWords[strings.ToLower(nm)] {
			called[nm] = true
		}
	}
	if len(called) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	selfRel, _ := filepath.Rel(repoRoot, absFile)
	selfRel = filepath.ToSlash(selfRel)
	for _, s := range flattenSymbols(selfFileSyms) {
		if (s.Kind == "function" || s.Kind == "method") && called[s.Name] && s.NamePath != selfSym.NamePath {
			if !seen[s.NamePath] {
				seen[s.NamePath] = true
				out = append(out, fmt.Sprintf("- %s:L%d %s — %s", selfRel, s.StartLine, s.NamePath, firstLineTrimmed(s.Signature)))
			}
		}
	}
	sort.Strings(out)
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

func fastCallersViaRG(repoRoot string, sym astSymbol, maxHits int) []string {
	if maxHits <= 0 {
		maxHits = 10
	}
	rgBin := findRipgrep()
	if rgBin == "" || sym.Name == "" {
		return nil
	}
	args := []string{"-n", "-w", "--no-heading", "--color", "never", "--max-count", "4", "--max-filesize", "1M",
		"-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*", "-g", "!dist/*", "-g", "!.next/*",
		"-g", "!target/*", "-g", "!Pods/*", "-g", "!.superpowers/*", "-g", "!.worktrees/*",
		"-g", "!*.md", "-g", "!*.txt", "-g", "!*.json", "-g", "!*.lock",
		"-e", sym.Name, repoRoot}
	out, err := exec.Command(rgBin, args...).Output()
	if err != nil {
		return nil
	}
	var prodHits, testHits []string
	perFile := map[string]int{}
	prefix := repoRoot + string(filepath.Separator)
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln == "" {
			continue
		}
		clean := strings.TrimPrefix(ln, prefix)
		parts := strings.SplitN(clean, ":", 3)
		if len(parts) < 3 {
			continue
		}
		fRel := filepath.ToSlash(parts[0])
		if isNonCodePath(fRel) {
			continue
		}
		lineNo := 0
		_, _ = fmt.Sscanf(parts[1], "%d", &lineNo)
		if fRel == sym.RelPath && lineNo >= sym.StartLine && lineNo <= sym.EndLine {
			continue
		}
		snippet := strings.TrimSpace(parts[2])
		// Skip pure import/comment lines to maximize signal
		if strings.HasPrefix(snippet, "import ") || strings.HasPrefix(snippet, "//") || strings.HasPrefix(snippet, "/*") || strings.HasPrefix(snippet, "*") {
			continue
		}
		if perFile[fRel] >= 2 {
			continue
		}
		perFile[fRel]++
		if len(snippet) > 100 {
			snippet = truncateUTF8(snippet, 97) + "..."
		}
		entry := fmt.Sprintf("- %s:L%d  %s", fRel, lineNo, snippet)
		if isTestCodePath(fRel) {
			testHits = append(testHits, entry)
		} else {
			prodHits = append(prodHits, entry)
		}
	}
	// Production callers first, test callers only if space remains
	combined := append(prodHits, testHits...)
	if len(combined) > maxHits {
		combined = combined[:maxHits]
	}
	return combined
}

func buildSingleSymbolPack(repoRoot, relScope, symbolQuery string, incCallers, incTypes, incDiags bool, bodyCap int) (string, []string, []string, error) {
	matches, sources, err := findSymbolsAcrossRepo(repoRoot, relScope, symbolQuery, false, nil, nil, 5)
	if err != nil {
		return "", nil, nil, err
	}
	if len(matches) == 0 {
		matches, sources, err = findSymbolsAcrossRepo(repoRoot, relScope, symbolQuery, true, nil, nil, 5)
		if err != nil {
			return "", nil, nil, err
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol matching %q found.", symbolQuery), nil, nil, nil
	}

	sym := matches[0]
	src := sources[sym.RelPath]
	absFile := filepath.Join(repoRoot, filepath.FromSlash(sym.RelPath))
	bodyText := ""
	if int(sym.EndByte) <= len(src) {
		bodyText = string(src[sym.StartByte:sym.EndByte])
	}
	selfFileSyms, _, _ := extractFileSymbolsCached(absFile, sym.RelPath, src)

	// Run independent stages in parallel!
	var (
		sigs      []string
		callees   []string
		callers   []string
		callerTag string
		hoverTxt  string
		diagTxt   string
		wg        sync.WaitGroup
	)
	if incTypes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sigs = extractImportedTypeSignatures(repoRoot, absFile, src, sym, selfFileSyms, bodyText)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		callees = extractOutgoingCallees(repoRoot, absFile, sym, selfFileSyms, bodyText)
	}()
	if incCallers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if lang := detectLSPLang(absFile); lang != "" {
				if warmRefs, ok := queryWarmLSPReferences(repoRoot, lang, absFile, sym.NameLine-1, sym.NameCol); ok && len(warmRefs) > 0 {
					var prod, tests []string
					for _, r := range warmRefs {
						if r.RelPath == sym.RelPath && r.Line >= sym.StartLine && r.Line <= sym.EndLine {
							continue
						}
						entry := fmt.Sprintf("- %s:L%d  %s", r.RelPath, r.Line, r.Preview)
						if isTestCodePath(r.RelPath) {
							tests = append(tests, entry)
						} else {
							prod = append(prod, entry)
						}
					}
					callers = append(prod, tests...)
					if len(callers) > 10 {
						callers = callers[:10]
					}
					callerTag = "LSP"
				}
			}
			if len(callers) == 0 {
				callers = fastCallersViaRG(repoRoot, sym, 10)
				callerTag = "fast-rg"
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if lang := detectLSPLang(absFile); lang != "" {
			hoverTxt = queryWarmLSPHover(repoRoot, lang, absFile, sym.NameLine-1, sym.NameCol)
		}
	}()
	if incDiags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			diagTxt = checkFileQuick(absFile)
		}()
	}
	wg.Wait()

	var b strings.Builder
	fmt.Fprintf(&b, "## Symbol: [%s] %s (%s:L%d-L%d)\n", sym.Kind, sym.NamePath, sym.RelPath, sym.StartLine, sym.EndLine)
	if hoverTxt != "" {
		fmt.Fprintf(&b, "Type/Hover: %s\n", firstLineTrimmed(hoverTxt))
	}
	if len(matches) > 1 {
		var others []string
		for _, m := range matches[1:] {
			others = append(others, fmt.Sprintf("%s (%s:L%d)", m.NamePath, m.RelPath, m.StartLine))
		}
		fmt.Fprintf(&b, "Other matches: %s\n", strings.Join(others, ", "))
	}

	trimmedBody := bodyText
	if len(trimmedBody) > bodyCap {
		trimmedBody = truncateUTF8(trimmedBody, bodyCap) + "\n... [body truncated]"
	}
	b.WriteString("```\n")
	b.WriteString(trimmedBody)
	if !strings.HasSuffix(trimmedBody, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("```\n")

	if len(callees) > 0 {
		b.WriteString("\n### Local Callees\n")
		for _, cl := range callees {
			b.WriteString(cl + "\n")
		}
	}
	if len(callers) > 0 {
		fmt.Fprintf(&b, "\n### Callers / References (%s, top %d)\n", callerTag, len(callers))
		for _, c := range callers {
			b.WriteString(c + "\n")
		}
	}

	var diagSlice []string
	if diagTxt != "" {
		diagSlice = strings.Split(diagTxt, "\n")
	}
	return strings.TrimSpace(b.String()), sigs, diagSlice, nil
}

func toolCodeContextPack(repoRoot string, args map[string]any) (string, error) {
	symbols := argStringSlice(args, "symbols")
	if single := strings.TrimSpace(argString(args, "symbol")); single != "" {
		symbols = append([]string{single}, symbols...)
	} else if np := strings.TrimSpace(argString(args, "name_path")); np != "" {
		symbols = append([]string{np}, symbols...)
	}
	if len(symbols) == 0 {
		return "", errors.New("symbol or symbols is required")
	}
	if len(symbols) > 6 {
		symbols = symbols[:6]
	}

	relScope := argString(args, "relative_path")
	incCallers := argBool(args, "include_callers", true)
	incTypes := argBool(args, "include_types", true)
	incDiags := argBool(args, "include_diagnostics", true)
	maxChars := argInt(args, "max_chars", 6000)
	if maxChars <= 0 {
		maxChars = 6000
	}
	bodyCap := (maxChars * 55 / 100) / len(symbols)
	if bodyCap < 900 {
		bodyCap = 900
	}

	type packPart struct {
		section string
		sigs    []string
		diags   []string
		err     error
	}
	parts := make([]packPart, len(symbols))
	var wg sync.WaitGroup
	for i, symQ := range symbols {
		wg.Add(1)
		go func(idx int, q string) {
			defer wg.Done()
			sec, sigs, diags, err := buildSingleSymbolPack(repoRoot, relScope, q, incCallers, incTypes, incDiags, bodyCap)
			parts[idx] = packPart{section: sec, sigs: sigs, diags: diags, err: err}
		}(i, symQ)
	}
	wg.Wait()

	var b strings.Builder
	seenSigs := map[string]bool{}
	var allSigs []string
	seenDiags := map[string]bool{}
	var allDiags []string

	for i, p := range parts {
		if p.err != nil {
			if len(parts) == 1 {
				return "", p.err
			}
			fmt.Fprintf(&b, "## Symbol %q: error: %v\n\n", symbols[i], p.err)
			continue
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(p.section)
		for _, s := range p.sigs {
			if !seenSigs[s] {
				seenSigs[s] = true
				allSigs = append(allSigs, s)
			}
		}
		for _, d := range p.diags {
			if d != "" && !seenDiags[d] {
				seenDiags[d] = true
				allDiags = append(allDiags, d)
			}
		}
	}

	if len(allSigs) > 0 {
		b.WriteString("\n\n### Referenced Local & Imported Types/Signatures\n")
		for i, s := range allSigs {
			if i >= 12 {
				break
			}
			b.WriteString(s + "\n")
		}
	}
	if len(allDiags) > 0 {
		b.WriteString("\n\n### Diagnostics\n")
		for i, d := range allDiags {
			if i >= 8 {
				break
			}
			b.WriteString(d + "\n")
		}
	}
	return truncateAnswer(strings.TrimSpace(b.String()), maxChars), nil
}
