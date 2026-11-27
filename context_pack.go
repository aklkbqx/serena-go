package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	reTSImport = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?(?:\{([^}]+)\}|([A-Za-z0-9_$]+))\s+from\s+['"]([^'"]+)['"]`)
	reGoImport = regexp.MustCompile(`(?m)^\s*(?:[A-Za-z0-9_]+\s+)?"([^"]+)"`)
	rePyImport = regexp.MustCompile(`(?m)^\s*from\s+(\.+[A-Za-z0-9_.]*)\s+import\s+([^\n]+)`)
)

func resolveRelativeImportFile(fromAbsFile, spec string) string {
	dir := filepath.Dir(fromAbsFile)
	base := filepath.Join(dir, spec)
	candidates := []string{
		base,
		base + ".ts", base + ".tsx", base + ".js", base + ".jsx",
		filepath.Join(base, "index.ts"), filepath.Join(base, "index.tsx"),
		filepath.Join(base, "index.js"),
		base + ".py", filepath.Join(base, "__init__.py"),
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

func extractImportedTypeSignatures(repoRoot, absFile string, src []byte, bodyText string) []string {
	var out []string
	seen := map[string]bool{}
	ext := strings.ToLower(filepath.Ext(absFile))

	if ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" || ext == ".mts" || ext == ".cts" {
		matches := reTSImport.FindAllStringSubmatch(string(src), 40)
		for _, m := range matches {
			spec := m[3]
			if !strings.HasPrefix(spec, ".") {
				continue
			}
			impFile := resolveRelativeImportFile(absFile, spec)
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
					if idx := strings.Index(part, " as "); idx >= 0 {
						part = strings.TrimSpace(part[:idx])
					}
					if part != "" && strings.Contains(bodyText, part) {
						idents = append(idents, part)
					}
				}
			} else if m[2] != "" && strings.Contains(bodyText, m[2]) {
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
			syms, _, err := extractFileSymbols(impFile, impRel, impSrc)
			if err != nil {
				continue
			}
			flat := flattenSymbols(syms)
			for _, id := range idents {
				for _, s := range flat {
					if s.Name == id && !seen[impRel+":"+s.Name] {
						seen[impRel+":"+s.Name] = true
						out = append(out, fmt.Sprintf("- %s:L%d [%s] %s", impRel, s.StartLine, s.Kind, s.Signature))
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
			syms, _, err := extractFileSymbols(impFile, filepath.ToSlash(impRel), impSrc)
			if err != nil {
				continue
			}
			flat := flattenSymbols(syms)
			for _, part := range strings.Split(m[2], ",") {
				id := strings.TrimSpace(strings.Split(strings.TrimSpace(part), " as ")[0])
				if id == "" || !strings.Contains(bodyText, id) {
					continue
				}
				for _, s := range flat {
					if s.Name == id && !seen[impRel+":"+s.Name] {
						seen[impRel+":"+s.Name] = true
						out = append(out, fmt.Sprintf("- %s:L%d [%s] %s", filepath.ToSlash(impRel), s.StartLine, s.Kind, s.Signature))
					}
				}
			}
		}
	} else if ext == ".go" {
		// Same-package symbols referenced in body (very common in Go without explicit imports)
		dir := filepath.Dir(absFile)
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
			syms, _, err := extractFileSymbols(sib, sibRel, sibSrc)
			if err != nil {
				continue
			}
			for _, s := range flattenSymbols(syms) {
				if len(s.Name) < 3 || !strings.Contains(bodyText, s.Name) {
					continue
				}
				if s.Kind == "struct" || s.Kind == "interface" || s.Kind == "type" || s.Kind == "function" {
					key := sibRel + ":" + s.NamePath
					if !seen[key] {
						seen[key] = true
						out = append(out, fmt.Sprintf("- %s:L%d [%s] %s", sibRel, s.StartLine, s.Kind, s.Signature))
					}
				}
			}
		}
		_ = reGoImport
	}

	sort.Strings(out)
	if len(out) > 12 {
		out = out[:12]
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
	args := []string{"-n", "-w", "--no-heading", "--color", "never", "--max-count", "8", "--max-filesize", "1M",
		"-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*", "-g", "!dist/*", "-g", "!.next/*", "-g", "!target/*", "-g", "!Pods/*",
		"-e", sym.Name, repoRoot}
	out, err := exec.Command(rgBin, args...).Output()
	if err != nil {
		return nil
	}
	var hits []string
	prefix := repoRoot + string(filepath.Separator)
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln == "" {
			continue
		}
		clean := strings.TrimPrefix(ln, prefix)
		// Parse file:line:text
		parts := strings.SplitN(clean, ":", 3)
		if len(parts) < 3 {
			continue
		}
		fRel := filepath.ToSlash(parts[0])
		lineNo := 0
		_, _ = fmt.Sscanf(parts[1], "%d", &lineNo)
		// Skip the definition line itself
		if fRel == sym.RelPath && lineNo >= sym.StartLine && lineNo <= sym.EndLine {
			continue
		}
		snippet := strings.TrimSpace(parts[2])
		if len(snippet) > 100 {
			snippet = snippet[:97] + "..."
		}
		hits = append(hits, fmt.Sprintf("- %s:L%d  %s", fRel, lineNo, snippet))
		if len(hits) >= maxHits {
			break
		}
	}
	return hits
}

func toolCodeContextPack(repoRoot string, args map[string]any) (string, error) {
	symbolQuery := argString(args, "symbol")
	if symbolQuery == "" {
		symbolQuery = argString(args, "name_path")
	}
	if symbolQuery == "" {
		return "", errors.New("symbol is required")
	}
	relScope := argString(args, "relative_path")
	incCallers := argBool(args, "include_callers", true)
	incTypes := argBool(args, "include_types", true)
	incDiags := argBool(args, "include_diagnostics", true)
	maxChars := argInt(args, "max_chars", 6000)
	if maxChars <= 0 {
		maxChars = 6000
	}

	matches, sources, err := findSymbolsAcrossRepo(repoRoot, relScope, symbolQuery, false, nil, nil, 5)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		matches, sources, err = findSymbolsAcrossRepo(repoRoot, relScope, symbolQuery, true, nil, nil, 5)
		if err != nil {
			return "", err
		}
	}
	if len(matches) == 0 {
		return fmt.Sprintf("No symbol matching %q found.", symbolQuery), nil
	}

	sym := matches[0]
	src := sources[sym.RelPath]
	absFile := filepath.Join(repoRoot, filepath.FromSlash(sym.RelPath))
	bodyText := ""
	if int(sym.EndByte) <= len(src) {
		bodyText = string(src[sym.StartByte:sym.EndByte])
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Symbol: [%s] %s (%s:L%d-L%d)\n", sym.Kind, sym.NamePath, sym.RelPath, sym.StartLine, sym.EndLine)
	if len(matches) > 1 {
		var others []string
		for _, m := range matches[1:] {
			others = append(others, fmt.Sprintf("%s (%s:L%d)", m.NamePath, m.RelPath, m.StartLine))
		}
		fmt.Fprintf(&b, "Other matches: %s\n", strings.Join(others, ", "))
	}

	// Definition body (reserve ~55% of budget for body)
	bodyCap := maxChars * 55 / 100
	if bodyCap < 1200 {
		bodyCap = 1200
	}
	trimmedBody := bodyText
	if len(trimmedBody) > bodyCap {
		trimmedBody = trimmedBody[:bodyCap] + "\n... [body truncated]"
	}
	b.WriteString("```\n")
	b.WriteString(trimmedBody)
	if !strings.HasSuffix(trimmedBody, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString("```\n")

	// 1-hop imported type / signature context
	if incTypes {
		if sigs := extractImportedTypeSignatures(repoRoot, absFile, src, bodyText); len(sigs) > 0 {
			b.WriteString("\n### Referenced Local Types/Signatures\n")
			for _, s := range sigs {
				b.WriteString(s + "\n")
			}
		}
	}

	// Callers / references (prefer warm LSP if already running, else fast rg with 0 cold start)
	if incCallers {
		var callers []string
		if lang := detectLSPLang(absFile); lang != "" {
			if warmRefs, ok := queryWarmLSPReferences(repoRoot, lang, absFile, sym.NameLine-1, sym.NameCol); ok && len(warmRefs) > 0 {
				for _, r := range warmRefs {
					if r.RelPath == sym.RelPath && r.Line >= sym.StartLine && r.Line <= sym.EndLine {
						continue
					}
					callers = append(callers, fmt.Sprintf("- %s:L%d  %s", r.RelPath, r.Line, r.Preview))
					if len(callers) >= 10 {
						break
					}
				}
			}
		}
		sourceTag := "LSP"
		if len(callers) == 0 {
			callers = fastCallersViaRG(repoRoot, sym, 10)
			sourceTag = "fast-rg"
		}
		if len(callers) > 0 {
			fmt.Fprintf(&b, "\n### Callers / References (%s, top %d)\n", sourceTag, len(callers))
			for _, c := range callers {
				b.WriteString(c + "\n")
			}
		}
	}

	// Live single-file diagnostics (syntax + oxlint/ty + warm LSP)
	if incDiags {
		if diags := checkFileQuick(absFile); diags != "" {
			b.WriteString("\n### Diagnostics\n")
			b.WriteString(diags + "\n")
		}
	}

	return truncateAnswer(strings.TrimSpace(b.String()), maxChars), nil
}
