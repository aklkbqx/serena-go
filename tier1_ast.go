package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// LSP SymbolKind constants for include_kinds / exclude_kinds filtering compatibility.
const (
	symKindModule    = 2
	symKindClass     = 5
	symKindMethod    = 6
	symKindProperty  = 7
	symKindField     = 8
	symKindCtor      = 9
	symKindEnum      = 10
	symKindInterface = 11
	symKindFunction  = 12
	symKindVariable  = 13
	symKindConstant  = 14
	symKindStruct    = 23
)

type astSymbol struct {
	Name      string
	NamePath  string
	Kind      string
	KindNum   int
	RelPath   string
	StartLine int // 1-based
	EndLine   int // 1-based
	StartByte uint32
	EndByte   uint32
	NameLine  int // 1-based line of identifier token (for LSP)
	NameCol   int // 0-based col of identifier token (for LSP)
	Signature string
	Children  []astSymbol
}

type syntaxDiag struct {
	Line    int
	Col     int
	Message string
}

func firstLineTrimmed(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	if len(s) > 140 {
		s = s[:137] + "..."
	}
	return s
}

func findIdentifierNode(bt *gts.BoundTree, n *gts.Node) *gts.Node {
	if n == nil {
		return nil
	}
	for _, field := range []string{"name", "declarator", "type"} {
		if c := bt.ChildByField(n, field); c != nil {
			ct := bt.NodeType(c)
			if isIdentType(ct) {
				return c
			}
			// Handle C/C++ function_declarator -> identifier, or Go type_spec -> name
			if inner := findIdentifierNode(bt, c); inner != nil {
				return inner
			}
		}
	}
	for i := 0; i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if isIdentType(bt.NodeType(c)) {
			return c
		}
	}
	return nil
}

func isIdentType(t string) bool {
	switch t {
	case "identifier", "type_identifier", "field_identifier", "property_identifier",
		"simple_identifier", "constant", "word", "scoped_identifier":
		return true
	default:
		return false
	}
}

func classifyNode(bt *gts.BoundTree, n *gts.Node, src []byte) (kind string, kindNum int, nameNode *gts.Node, container bool) {
	t := bt.NodeType(n)
	switch t {
	case "function_declaration", "function_definition", "function_item", "generator_function_declaration":
		return "function", symKindFunction, findIdentifierNode(bt, n), false
	case "method_declaration", "method_definition", "method_elem":
		return "method", symKindMethod, findIdentifierNode(bt, n), false
	case "class_declaration", "class_definition", "class_specifier":
		return "class", symKindClass, findIdentifierNode(bt, n), true
	case "interface_declaration", "protocol_declaration", "trait_item":
		return "interface", symKindInterface, findIdentifierNode(bt, n), true
	case "struct_item", "struct_specifier":
		return "struct", symKindStruct, findIdentifierNode(bt, n), true
	case "enum_declaration", "enum_item", "enum_specifier":
		return "enum", symKindEnum, findIdentifierNode(bt, n), true
	case "type_alias_declaration", "type_item", "type_spec":
		// Go struct/interface inside type_spec
		if typ := bt.ChildByField(n, "type"); typ != nil {
			tt := bt.NodeType(typ)
			if tt == "struct_type" {
				return "struct", symKindStruct, findIdentifierNode(bt, n), true
			}
			if tt == "interface_type" {
				return "interface", symKindInterface, findIdentifierNode(bt, n), true
			}
		}
		return "type", symKindInterface, findIdentifierNode(bt, n), false
	case "impl_item":
		// Rust impl Foo { fn bar() }
		if typ := bt.ChildByField(n, "type"); typ != nil {
			return "impl", symKindClass, typ, true
		}
		return "impl", symKindClass, findIdentifierNode(bt, n), true
	case "mod_item", "module", "namespace_definition":
		return "module", symKindModule, findIdentifierNode(bt, n), true
	case "public_field_definition", "field_definition", "property_signature":
		return "field", symKindField, findIdentifierNode(bt, n), false
	case "variable_declarator", "const_spec", "var_spec", "const_item", "static_item":
		// Capture top-level constants and arrow/function variables (e.g. export const Foo = () => ...)
		nm := findIdentifierNode(bt, n)
		if nm == nil {
			return "", 0, nil, false
		}
		if val := bt.ChildByField(n, "value"); val != nil {
			vt := bt.NodeType(val)
			if vt == "arrow_function" || vt == "function_expression" || vt == "function" {
				return "function", symKindFunction, nm, false
			}
		}
		if t == "const_spec" || t == "const_item" || t == "static_item" {
			return "const", symKindConstant, nm, false
		}
		// Only record top-level variable_declarators (const/let/var at file scope)
		if p := n.Parent(); p != nil {
			pt := bt.NodeType(p)
			if pt == "lexical_declaration" || pt == "variable_declaration" {
				if gp := p.Parent(); gp != nil {
					gpt := bt.NodeType(gp)
					if gpt == "program" || gpt == "export_statement" {
						return "const", symKindConstant, nm, false
					}
				}
			}
		}
	}
	_ = src
	return "", 0, nil, false
}

// goMethodReceiver extracts Go method receiver type name (e.g. "*Server" -> "Server").
func goMethodReceiver(bt *gts.BoundTree, n *gts.Node) string {
	if bt.NodeType(n) != "method_declaration" {
		return ""
	}
	recv := bt.ChildByField(n, "receiver")
	if recv == nil {
		return ""
	}
	txt := bt.NodeText(recv)
	// Strip parens, pointer, receiver var name, generic args
	txt = strings.Trim(txt, "() \t\n")
	parts := strings.Fields(txt)
	if len(parts) == 0 {
		return ""
	}
	typ := parts[len(parts)-1]
	typ = strings.TrimLeft(typ, "*")
	if idx := strings.IndexByte(typ, '['); idx >= 0 {
		typ = typ[:idx]
	}
	return strings.TrimSpace(typ)
}

// expandToOuterDeclaration widens start/end byte range to cover export_statement / lexical_declaration wrappers.
func expandToOuterDeclaration(bt *gts.BoundTree, n *gts.Node) *gts.Node {
	cur := n
	for p := cur.Parent(); p != nil; p = cur.Parent() {
		pt := bt.NodeType(p)
		if pt == "export_statement" || pt == "decorated_definition" ||
			(pt == "type_declaration" && p.NamedChildCount() == 1) ||
			((pt == "lexical_declaration" || pt == "variable_declaration") && p.NamedChildCount() == 1) {
			cur = p
			continue
		}
		break
	}
	return cur
}

func extractFileSymbols(absPath, relPath string, src []byte) ([]astSymbol, []syntaxDiag, error) {
	bt, err := grammars.ParseFile(absPath, src)
	if err != nil {
		return nil, nil, err
	}
	defer bt.Release()

	root := bt.RootNode()
	if root == nil {
		return nil, nil, nil
	}

	var syms []astSymbol
	var walk func(n *gts.Node, prefix string, depth int) []astSymbol
	walk = func(n *gts.Node, prefix string, depth int) []astSymbol {
		if n == nil || depth > 8 {
			return nil
		}
		var out []astSymbol
		for i := 0; i < n.NamedChildCount(); i++ {
			c := n.NamedChild(i)
			kind, kindNum, nameNode, isContainer := classifyNode(bt, c, src)
			if kind != "" && nameNode != nil {
				name := strings.TrimSpace(bt.NodeText(nameNode))
				if name == "" {
					continue
				}
				fullPrefix := prefix
				if fullPrefix == "" {
					if recv := goMethodReceiver(bt, c); recv != "" {
						fullPrefix = recv
					}
				}
				namePath := name
				if fullPrefix != "" {
					namePath = fullPrefix + "/" + name
				}
				spanNode := expandToOuterDeclaration(bt, c)
				sb := spanNode.StartByte()
				eb := spanNode.EndByte()
				if int(eb) > len(src) {
					eb = uint32(len(src))
				}
				if sb > eb {
					sb = eb
				}
				sym := astSymbol{
					Name:      name,
					NamePath:  namePath,
					Kind:      kind,
					KindNum:   kindNum,
					RelPath:   relPath,
					StartLine: int(spanNode.StartPoint().Row) + 1,
					EndLine:   int(spanNode.EndPoint().Row) + 1,
					StartByte: sb,
					EndByte:   eb,
					NameLine:  int(nameNode.StartPoint().Row) + 1,
					NameCol:   int(nameNode.StartPoint().Column),
					Signature: firstLineTrimmed(string(src[sb:eb])),
				}
				if isContainer {
					sym.Children = walk(c, namePath, depth+1)
				}
				out = append(out, sym)
				continue
			}
			// Transparent wrappers (export_statement, type_declaration, class_body, block, etc.)
			ct := bt.NodeType(c)
			switch ct {
			case "export_statement", "decorated_definition", "lexical_declaration", "variable_declaration",
				"type_declaration", "const_declaration", "var_declaration", "class_body", "declaration_list",
				"interface_body", "object_type", "block", "suite", "statement_block":
				out = append(out, walk(c, prefix, depth+1)...)
			}
		}
		return out
	}
	syms = walk(root, "", 0)

	// Collect syntax errors (capped at 10)
	var diags []syntaxDiag
	if root.HasError() {
		var collectErrs func(n *gts.Node)
		collectErrs = func(n *gts.Node) {
			if n == nil || len(diags) >= 10 {
				return
			}
			if n.IsError() || n.IsMissing() {
				pt := n.StartPoint()
				msg := "syntax error"
				if n.IsMissing() {
					msg = fmt.Sprintf("missing %s", bt.NodeType(n))
				} else {
					snippet := firstLineTrimmed(bt.NodeText(n))
					if len(snippet) > 50 {
						snippet = snippet[:47] + "..."
					}
					if snippet != "" {
						msg = fmt.Sprintf("syntax error near %q", snippet)
					}
				}
				diags = append(diags, syntaxDiag{
					Line:    int(pt.Row) + 1,
					Col:     int(pt.Column) + 1,
					Message: msg,
				})
				return
			}
			if !n.HasError() {
				return
			}
			for i := 0; i < n.ChildCount(); i++ {
				collectErrs(n.Child(i))
			}
		}
		collectErrs(root)
	}
	return syms, diags, nil
}

func flattenSymbols(syms []astSymbol) []astSymbol {
	var out []astSymbol
	for _, s := range syms {
		out = append(out, s)
		if len(s.Children) > 0 {
			out = append(out, flattenSymbols(s.Children)...)
		}
	}
	return out
}

func matchSymbolPattern(sym astSymbol, pattern string, substr bool) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if pattern == "" {
		return true
	}
	if substr {
		lp := strings.ToLower(pattern)
		return strings.Contains(strings.ToLower(sym.NamePath), lp) || strings.Contains(strings.ToLower(sym.Name), lp)
	}
	if strings.Contains(pattern, "/") {
		return sym.NamePath == pattern || strings.HasSuffix(sym.NamePath, "/"+pattern)
	}
	return sym.Name == pattern || sym.NamePath == pattern
}

func formatSymbolTree(syms []astSymbol, maxDepth, curDepth int, b *strings.Builder) {
	indent := strings.Repeat("  ", curDepth)
	for _, s := range syms {
		fmt.Fprintf(b, "%s[%s] %s (L%d-L%d) — %s\n", indent, s.Kind, s.NamePath, s.StartLine, s.EndLine, s.Signature)
		if curDepth < maxDepth && len(s.Children) > 0 {
			formatSymbolTree(s.Children, maxDepth, curDepth+1, b)
		}
	}
}

func toolGetSymbolsOverview(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	target, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	depth := argInt(args, "depth", 1)
	if depth < 0 {
		depth = 0
	}
	syms, diags, err := extractFileSymbols(target, rel, src)
	if err != nil {
		return "", fmt.Errorf("AST parse %s: %w", rel, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d top-level symbols):\n", rel, len(syms))
	formatSymbolTree(syms, depth, 0, &b)
	if len(diags) > 0 {
		fmt.Fprintf(&b, "\nSyntax warnings (%d):\n", len(diags))
		for _, d := range diags {
			fmt.Fprintf(&b, "  L%d:%d %s\n", d.Line, d.Col, d.Message)
		}
	}
	return truncateAnswer(strings.TrimSpace(b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

func candidateFilesForSymbol(repoRoot, relScope, pattern string) ([]string, error) {
	target, err := resolveRepoPath(repoRoot, relScope)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{target}, nil
	}
	// Extract leaf name for ripgrep prefilter
	leaf := strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if idx := strings.LastIndexByte(leaf, '/'); idx >= 0 {
		leaf = leaf[idx+1:]
	}
	rgBin := findRipgrep()
	if rgBin != "" && leaf != "" {
		args := []string{"-l", "-F", "--max-filesize", "1M",
			"-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*", "-g", "!dist/*", "-g", "!.next/*", "-g", "!target/*", "-g", "!Pods/*",
			"-e", leaf, target}
		out, err := exec.Command(rgBin, args...).Output()
		if err == nil {
			var files []string
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if ln != "" && grammars.DetectLanguage(ln) != nil {
					files = append(files, ln)
				}
			}
			sort.Strings(files)
			if len(files) > 80 {
				files = files[:80]
			}
			return files, nil
		}
	}
	// Fallback: walk up to 80 code files
	var files []string
	_ = filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "graft", "dist", ".next", "target", "Pods", "DerivedData", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= 80 {
			return filepath.SkipAll
		}
		if grammars.DetectLanguage(p) != nil {
			files = append(files, p)
		}
		return nil
	})
	return files, nil
}

func findSymbolsAcrossRepo(repoRoot, relScope, pattern string, substr bool, incKinds, excKinds map[int]bool, maxMatches int) ([]astSymbol, map[string][]byte, error) {
	if maxMatches <= 0 {
		maxMatches = 20
	}
	files, err := candidateFilesForSymbol(repoRoot, relScope, pattern)
	if err != nil {
		return nil, nil, err
	}
	var matches []astSymbol
	sources := make(map[string][]byte)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil || len(src) > 1024*1024 {
			continue
		}
		rel, _ := filepath.Rel(repoRoot, f)
		rel = filepath.ToSlash(rel)
		syms, _, err := extractFileSymbols(f, rel, src)
		if err != nil {
			continue
		}
		for _, s := range flattenSymbols(syms) {
			if len(incKinds) > 0 && !incKinds[s.KindNum] {
				continue
			}
			if len(excKinds) > 0 && excKinds[s.KindNum] {
				continue
			}
			if matchSymbolPattern(s, pattern, substr) {
				matches = append(matches, s)
				sources[rel] = src
				if len(matches) >= maxMatches {
					return matches, sources, nil
				}
			}
		}
	}
	return matches, sources, nil
}

func toolFindSymbol(repoRoot string, args map[string]any) (string, error) {
	pat := argString(args, "name_path_pattern")
	if pat == "" {
		return "", errors.New("name_path_pattern is required")
	}
	relScope := argString(args, "relative_path")
	substr := argBool(args, "substring_matching", false)
	incBody := argBool(args, "include_body", false)
	depth := argInt(args, "depth", 0)
	maxMatches := argInt(args, "max_matches", 20)
	incKinds := argIntSet(args, "include_kinds")
	excKinds := argIntSet(args, "exclude_kinds")

	matches, sources, err := findSymbolsAcrossRepo(repoRoot, relScope, pat, substr, incKinds, excKinds, maxMatches)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 && !substr {
		// Helpful automatic fallback hint: try substring match if exact failed
		matches, sources, _ = findSymbolsAcrossRepo(repoRoot, relScope, pat, true, incKinds, excKinds, 5)
		if len(matches) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "No exact match for %q. Closest symbols:\n", pat)
			for _, m := range matches {
				fmt.Fprintf(&b, "- [%s] %s in %s (L%d-L%d)\n", m.Kind, m.NamePath, m.RelPath, m.StartLine, m.EndLine)
			}
			return b.String(), nil
		}
		return fmt.Sprintf("No symbol matching %q found.", pat), nil
	}

	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%s] %s — %s:L%d-L%d\n", m.Kind, m.NamePath, m.RelPath, m.StartLine, m.EndLine)
		if depth > 0 && len(m.Children) > 0 {
			formatSymbolTree(m.Children, depth, 1, &b)
		}
		if incBody {
			if src, ok := sources[m.RelPath]; ok && int(m.EndByte) <= len(src) {
				b.WriteString("```\n")
				b.Write(src[m.StartByte:m.EndByte])
				if m.EndByte > m.StartByte && src[m.EndByte-1] != '\n' {
					b.WriteByte('\n')
				}
				b.WriteString("```\n")
			}
		} else {
			fmt.Fprintf(&b, "  %s\n", m.Signature)
		}
	}
	return truncateAnswer(strings.TrimSpace(b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

func resolveSingleSymbolInFile(repoRoot, rel, namePath string) (astSymbol, string, []byte, os.FileMode, error) {
	target, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return astSymbol{}, "", nil, 0, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return astSymbol{}, "", nil, 0, err
	}
	src, err := os.ReadFile(target)
	if err != nil {
		return astSymbol{}, "", nil, 0, err
	}
	syms, _, err := extractFileSymbols(target, rel, src)
	if err != nil {
		return astSymbol{}, "", nil, 0, err
	}
	var hits []astSymbol
	for _, s := range flattenSymbols(syms) {
		if matchSymbolPattern(s, namePath, false) {
			hits = append(hits, s)
		}
	}
	if len(hits) == 0 {
		// Try fuzzy substring match if unambiguous
		for _, s := range flattenSymbols(syms) {
			if matchSymbolPattern(s, namePath, true) {
				hits = append(hits, s)
			}
		}
	}
	if len(hits) == 0 {
		return astSymbol{}, "", nil, 0, fmt.Errorf("symbol %q not found in %s", namePath, rel)
	}
	if len(hits) > 1 {
		var names []string
		for _, h := range hits {
			names = append(names, fmt.Sprintf("%s (L%d)", h.NamePath, h.StartLine))
		}
		return astSymbol{}, "", nil, 0, fmt.Errorf("ambiguous symbol %q in %s; matches: %s", namePath, rel, strings.Join(names, ", "))
	}
	return hits[0], target, src, info.Mode().Perm(), nil
}

func toolReplaceSymbolBody(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path")
	body := argString(args, "body")
	sym, target, src, perm, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	out.Write(src[:sym.StartByte])
	out.WriteString(body)
	out.Write(src[sym.EndByte:])
	if err := atomicWriteFile(target, out.Bytes(), perm); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Replaced symbol %s in %s (was L%d-L%d).", sym.NamePath, rel, sym.StartLine, sym.EndLine)
	if diag := checkSyntaxQuick(target, out.Bytes()); diag != "" {
		msg += "\n[Syntax Warning]\n" + diag
	}
	return msg, nil
}

func toolInsertRelativeToSymbol(repoRoot string, args map[string]any, after bool) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path")
	body := argString(args, "body")
	sym, target, src, perm, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if after {
		out.Write(src[:sym.EndByte])
		if !strings.HasPrefix(body, "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(body)
		out.Write(src[sym.EndByte:])
	} else {
		out.Write(src[:sym.StartByte])
		out.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			out.WriteByte('\n')
		}
		out.Write(src[sym.StartByte:])
	}
	if err := atomicWriteFile(target, out.Bytes(), perm); err != nil {
		return "", err
	}
	pos := "before"
	if after {
		pos = "after"
	}
	msg := fmt.Sprintf("Inserted code %s symbol %s in %s.", pos, sym.NamePath, rel)
	if diag := checkSyntaxQuick(target, out.Bytes()); diag != "" {
		msg += "\n[Syntax Warning]\n" + diag
	}
	return msg, nil
}

func checkSyntaxQuick(absPath string, src []byte) string {
	if grammars.DetectLanguage(absPath) == nil {
		return ""
	}
	_, diags, err := extractFileSymbols(absPath, filepath.Base(absPath), src)
	if err != nil || len(diags) == 0 {
		return ""
	}
	var b strings.Builder
	for i, d := range diags {
		if i >= 5 {
			break
		}
		fmt.Fprintf(&b, "%s:%d:%d: [syntax] %s\n", filepath.Base(absPath), d.Line, d.Col, d.Message)
	}
	return strings.TrimSpace(b.String())
}

// checkFileQuick powers `serena check-file <path>` (PostToolUse hook) and code_context_pack diagnostics.
// Runs <10ms AST syntax + <50ms oxlint/ty + optional warm-only LSP diagnostics (NEVER cold-boots LSP).
func checkFileQuick(absPath string) string {
	src, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	var lines []string
	if syn := checkSyntaxQuick(absPath, src); syn != "" {
		lines = append(lines, strings.Split(syn, "\n")...)
	}

	ext := strings.ToLower(filepath.Ext(absPath))
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	home, _ := os.UserHomeDir()
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		if ox := findToolBin("oxlint",
			filepath.Join(home, ".bun/bin/oxlint"),
			filepath.Join(home, ".local/bin/oxlint"),
		); ox != "" {
			cmd := exec.CommandContext(ctx, ox, "--format", "unix", "-D", "correctness", "--quiet", absPath)
			out, _ := cmd.Output()
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				ln = strings.TrimSpace(ln)
				if ln != "" && strings.Contains(ln, ":") && !strings.HasPrefix(ln, "Found ") && !strings.HasPrefix(ln, "Finished ") {
					lines = append(lines, "[oxlint] "+strings.ReplaceAll(ln, filepath.Dir(absPath)+string(filepath.Separator), ""))
				}
			}
		}
	case ".py":
		if tyBin := findToolBin("ty", filepath.Join(home, ".local/bin/ty")); tyBin != "" {
			cmd := exec.CommandContext(ctx, tyBin, "check", "--output-format", "concise", absPath)
			out, _ := cmd.Output()
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				ln = strings.TrimSpace(ln)
				if ln != "" && strings.Contains(ln, "error[") {
					lines = append(lines, "[ty] "+ln)
				}
			}
		}
	}

	// Query warm LSP diagnostics if an LSP daemon is ALREADY running (0 cold-start cost!).
	if repoRoot := findGitRoot(filepath.Dir(absPath)); repoRoot != "" {
		if lang := detectLSPLang(absPath); lang != "" {
			if warmDiags := queryWarmLSPDiagnostics(repoRoot, lang, absPath); len(warmDiags) > 0 {
				lines = append(lines, warmDiags...)
			}
		}
	}

	// Deduplicate and cap at 5 diagnostics.
	seen := map[string]bool{}
	var uniq []string
	for _, l := range lines {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		uniq = append(uniq, l)
		if len(uniq) >= 5 {
			break
		}
	}
	return strings.Join(uniq, "\n")
}

func findToolBin(name string, fallbacks ...string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, f := range fallbacks {
		if f == "" {
			continue
		}
		if fi, err := os.Stat(f); err == nil && !fi.IsDir() {
			return f
		}
	}
	return ""
}

func findGitRoot(dir string) string {
	cur := canonicalPath(dir)
	for cur != "" && cur != "/" {
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}
