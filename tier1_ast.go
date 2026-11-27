package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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
	NameCol   int // 0-based UTF-8 byte col of identifier token (for LSP)
	Signature string
	Children  []astSymbol
}

type syntaxDiag struct {
	Line    int
	Col     int
	Message string
}

// Tiny mtime-keyed AST cache (capped at 32 files, 60s TTL) to avoid double parsing
// within a turn while keeping multi-session Go heap footprint negligible.
type cachedFileAST struct {
	mtimeNS  int64
	size     int64
	cachedAt time.Time
	syms     []astSymbol
	diags    []syntaxDiag
}

var (
	fileASTMu    sync.Mutex
	fileASTCache = map[string]cachedFileAST{}
)

func purgeFileASTCache() {
	fileASTMu.Lock()
	fileASTCache = map[string]cachedFileAST{}
	fileASTMu.Unlock()
}

func extractFileSymbolsCached(absPath, relPath string, src []byte) ([]astSymbol, []syntaxDiag, error) {
	info, err := os.Stat(absPath)
	if err == nil {
		key := canonicalPath(absPath)
		mtime := info.ModTime().UnixNano()
		sz := info.Size()
		fileASTMu.Lock()
		if ent, ok := fileASTCache[key]; ok && ent.mtimeNS == mtime && ent.size == sz && time.Since(ent.cachedAt) < 60*time.Second {
			fileASTMu.Unlock()
			return ent.syms, ent.diags, nil
		}
		fileASTMu.Unlock()

		syms, diags, perr := extractFileSymbols(absPath, relPath, src)
		if perr != nil {
			return nil, nil, perr
		}
		fileASTMu.Lock()
		if len(fileASTCache) >= 32 {
			// Evict oldest entry
			oldestK := ""
			var oldestT time.Time
			for k, v := range fileASTCache {
				if oldestK == "" || v.cachedAt.Before(oldestT) {
					oldestK = k
					oldestT = v.cachedAt
				}
			}
			if oldestK != "" {
				delete(fileASTCache, oldestK)
			}
		}
		fileASTCache[key] = cachedFileAST{
			mtimeNS:  mtime,
			size:     sz,
			cachedAt: time.Now(),
			syms:     syms,
			diags:    diags,
		}
		fileASTMu.Unlock()
		return syms, diags, nil
	}
	return extractFileSymbols(absPath, relPath, src)
}

func firstLineTrimmed(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	if len(s) > 140 {
		s = truncateUTF8(s, 137) + "..."
	}
	return s
}

// extractSignature collapses multiline declaration headers (up to opening body delimiter)
// and skips leading Python @decorator lines so the AI sees the actual signature.
func extractSignature(s string) string {
	lines := strings.Split(s, "\n")
	var parts []string
	started := false
	for _, raw := range lines {
		ln := strings.TrimSpace(raw)
		if ln == "" {
			if started {
				break
			}
			continue
		}
		// Skip leading Python/TS decorator lines if declaration follows
		if !started && strings.HasPrefix(ln, "@") {
			continue
		}
		started = true
		parts = append(parts, ln)
		if strings.HasSuffix(ln, "{") || strings.HasSuffix(ln, ":") || strings.Contains(ln, "=>") || strings.HasSuffix(ln, ";") || len(parts) >= 4 {
			break
		}
	}
	if len(parts) == 0 {
		return firstLineTrimmed(s)
	}
	joined := strings.Join(parts, " ")
	joined = strings.Join(strings.Fields(joined), " ")
	if len(joined) > 160 {
		joined = truncateUTF8(joined, 157) + "..."
	}
	return joined
}

func findIdentifierNode(bt *gts.BoundTree, n *gts.Node) *gts.Node {
	if n == nil {
		return nil
	}
	for _, field := range []string{"name", "declarator", "type", "left"} {
		if c := bt.ChildByField(n, field); c != nil {
			ct := bt.NodeType(c)
			if isIdentType(ct) {
				return c
			}
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
		"simple_identifier", "constant", "word", "scoped_identifier", "type_name":
		return true
	default:
		return false
	}
}

func isUpperConstantName(name string) bool {
	if len(name) < 2 {
		return false
	}
	hasAlpha := false
	for _, r := range name {
		if r >= 'A' && r <= 'Z' {
			hasAlpha = true
		} else if (r >= '0' && r <= '9') || r == '_' {
			continue
		} else {
			return false
		}
	}
	return hasAlpha
}

func classifySwiftDecl(bt *gts.BoundTree, n *gts.Node) (string, int) {
	head := firstLineTrimmed(bt.NodeText(n))
	for _, kw := range strings.Fields(head) {
		switch kw {
		case "struct":
			return "struct", symKindStruct
		case "enum":
			return "enum", symKindEnum
		case "protocol":
			return "interface", symKindInterface
		case "actor", "class", "extension":
			return "class", symKindClass
		}
	}
	return "class", symKindClass
}

func cleanRustImplType(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.IndexByte(raw, '<'); idx >= 0 {
		raw = raw[:idx]
	}
	if idx := strings.LastIndex(raw, "::"); idx >= 0 {
		raw = raw[idx+2:]
	}
	return strings.TrimSpace(raw)
}

func classifyNode(bt *gts.BoundTree, n *gts.Node, src []byte, inContainer bool) (kind string, kindNum int, nameNode *gts.Node, container bool) {
	t := bt.NodeType(n)
	switch t {
	case "function_declaration", "function_definition", "function_item", "generator_function_declaration":
		if inContainer && (t == "function_definition" || t == "function_item" || t == "function_declaration") {
			return "method", symKindMethod, findIdentifierNode(bt, n), false
		}
		return "function", symKindFunction, findIdentifierNode(bt, n), false
	case "method_declaration", "method_definition", "method_elem", "method_signature",
		"abstract_method_signature", "function_signature_item", "method_spec":
		return "method", symKindMethod, findIdentifierNode(bt, n), false
	case "class_declaration":
		// tree-sitter-swift reuses class_declaration for struct/enum/protocol/extension
		if bt.ChildByField(n, "declaration_kind") != nil || strings.Contains(firstLineTrimmed(bt.NodeText(n)), "struct ") || strings.Contains(firstLineTrimmed(bt.NodeText(n)), "enum ") {
			k, kn := classifySwiftDecl(bt, n)
			return k, kn, findIdentifierNode(bt, n), true
		}
		return "class", symKindClass, findIdentifierNode(bt, n), true
	case "abstract_class_declaration", "class_definition", "class_specifier":
		return "class", symKindClass, findIdentifierNode(bt, n), true
	case "interface_declaration", "protocol_declaration", "trait_item":
		return "interface", symKindInterface, findIdentifierNode(bt, n), true
	case "struct_item", "struct_specifier":
		return "struct", symKindStruct, findIdentifierNode(bt, n), true
	case "enum_declaration", "enum_item", "enum_specifier":
		return "enum", symKindEnum, findIdentifierNode(bt, n), true
	case "type_alias_declaration", "type_item", "type_spec":
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
		if typ := bt.ChildByField(n, "type"); typ != nil {
			return "impl", symKindClass, typ, true
		}
		return "impl", symKindClass, findIdentifierNode(bt, n), true
	case "mod_item", "module", "namespace_definition", "internal_module":
		return "module", symKindModule, findIdentifierNode(bt, n), true
	case "public_field_definition", "field_definition", "property_signature", "field_declaration":
		if inContainer {
			return "field", symKindField, findIdentifierNode(bt, n), false
		}
	case "enum_assignment":
		if inContainer {
			return "const", symKindConstant, findIdentifierNode(bt, n), false
		}
	case "assignment":
		// Python top-level UPPER_CASE constants (e.g. DEFAULT_TTL = 60)
		if !inContainer {
			if left := bt.ChildByField(n, "left"); left != nil && bt.NodeType(left) == "identifier" {
				if isUpperConstantName(strings.TrimSpace(bt.NodeText(left))) {
					return "const", symKindConstant, left, false
				}
			}
		}
	case "variable_declarator", "const_spec", "var_spec", "const_item", "static_item":
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
		if t == "var_spec" {
			if p := n.Parent(); p != nil && bt.NodeType(p) == "var_declaration" {
				if gp := p.Parent(); gp != nil && bt.NodeType(gp) == "source_file" {
					return "var", symKindVariable, nm, false
				}
			}
		}
		// Top-level JS/TS variable_declarators
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

// goMethodReceiver extracts Go method receiver type name cleanly, including generic receivers
// such as `func (m *Map[K, V]) Lookup(...)` -> "Map".
func goMethodReceiver(bt *gts.BoundTree, n *gts.Node) string {
	if bt.NodeType(n) != "method_declaration" {
		return ""
	}
	recv := bt.ChildByField(n, "receiver")
	if recv == nil {
		return ""
	}
	txt := bt.NodeText(recv)
	txt = strings.Trim(txt, "() \t\n")
	// Strip generic parameter list `[K, V]` BEFORE splitting on whitespace!
	if idx := strings.IndexByte(txt, '['); idx >= 0 {
		txt = txt[:idx]
	}
	parts := strings.Fields(txt)
	if len(parts) == 0 {
		return ""
	}
	typ := parts[len(parts)-1]
	typ = strings.TrimLeft(typ, "*")
	return strings.TrimSpace(typ)
}

// expandToOuterDeclaration widens start/end byte range to cover export_statement,
// decorated_definition, and single-item type/const/var/lexical declarations.
func expandToOuterDeclaration(bt *gts.BoundTree, n *gts.Node) *gts.Node {
	cur := n
	for p := cur.Parent(); p != nil; p = cur.Parent() {
		pt := bt.NodeType(p)
		if pt == "export_statement" || pt == "decorated_definition" || pt == "ambient_declaration" ||
			((pt == "type_declaration" || pt == "const_declaration" || pt == "var_declaration") && p.NamedChildCount() == 1) ||
			((pt == "lexical_declaration" || pt == "variable_declaration") && p.NamedChildCount() == 1) {
			cur = p
			continue
		}
		break
	}
	return cur
}

func extractFileSymbols(absPath, relPath string, src []byte) ([]astSymbol, []syntaxDiag, error) {
	bt, err := grammars.ParseFilePooled(absPath, src)
	if err != nil {
		return nil, nil, err
	}
	defer bt.Release()

	root := bt.RootNode()
	if root == nil {
		return nil, nil, nil
	}

	var walk func(n *gts.Node, prefix string, depth int, inContainer bool) []astSymbol
	walk = func(n *gts.Node, prefix string, depth int, inContainer bool) []astSymbol {
		if n == nil || depth > 8 {
			return nil
		}
		var out []astSymbol
		for i := 0; i < n.NamedChildCount(); i++ {
			c := n.NamedChild(i)
			kind, kindNum, nameNode, isContainer := classifyNode(bt, c, src, inContainer)
			if kind != "" && nameNode != nil {
				name := strings.TrimSpace(bt.NodeText(nameNode))
				if kind == "impl" {
					name = cleanRustImplType(name)
				}
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
					Signature: extractSignature(string(src[sb:eb])),
				}
				if isContainer {
					sym.Children = walk(c, namePath, depth+1, true)
				}
				out = append(out, sym)
				continue
			}
			// Transparent wrappers (export_statement, type_declaration, struct_type, class_body, etc.)
			ct := bt.NodeType(c)
			switch ct {
			case "export_statement", "decorated_definition", "ambient_declaration",
				"lexical_declaration", "variable_declaration",
				"type_declaration", "const_declaration", "var_declaration",
				"struct_type", "interface_type", "field_declaration_list",
				"class_body", "declaration_list", "interface_body", "object_type",
				"enum_body", "expression_statement", "statement_block",
				"block", "suite", "protocol_body":
				out = append(out, walk(c, prefix, depth+1, inContainer)...)
			}
		}
		return out
	}
	syms := walk(root, "", 0, false)

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
						snippet = truncateUTF8(snippet, 47) + "..."
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
		sig := s.Signature
		if len(sig) > 100 {
			sig = truncateUTF8(sig, 97) + "..."
		}
		fmt.Fprintf(b, "%s[%s] %s L%d-%d | %s\n", indent, s.Kind, s.NamePath, s.StartLine, s.EndLine, sig)
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
	syms, diags, err := extractFileSymbolsCached(target, rel, src)
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

func candidateFilesForSymbol(repoRoot, relScope, pattern string, caseInsensitive bool) ([]string, error) {
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
	leaf := strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if idx := strings.LastIndexByte(leaf, '/'); idx >= 0 {
		leaf = leaf[idx+1:]
	}
	rgBin := findRipgrep()
	if rgBin != "" && leaf != "" {
		args := []string{"-l", "-F", "--max-filesize", "1M",
			"-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*", "-g", "!dist/*", "-g", "!.next/*",
			"-g", "!target/*", "-g", "!Pods/*", "-g", "!.superpowers/*", "-g", "!.worktrees/*"}
		if caseInsensitive {
			args = append(args, "-i")
		}
		args = append(args, "-e", leaf, target)
		out, err := exec.Command(rgBin, args...).Output()
		if err == nil {
			var files []string
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if ln != "" && grammars.DetectLanguage(ln) != nil {
					files = append(files, ln)
				}
			}
			sort.Strings(files)
			if len(files) > 160 {
				files = files[:160]
			}
			return files, nil
		}
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
	}
	// Fallback when rg is unavailable or pattern is empty: walk up to 160 code files.
	var files []string
	_ = filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipIndexDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= 160 {
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
	// Fast-path: use pre-built V2 mmap symbol index when querying directory scope with warm cache.
	if relScope == "" || relScope == "." {
		if idxHits, ok := lookupSymbolsInMmapIndex(repoRoot, pattern, substr, incKinds, excKinds, maxMatches); ok {
			sources := make(map[string][]byte, len(idxHits))
			for _, s := range idxHits {
				if _, loaded := sources[s.RelPath]; !loaded {
					if raw, rerr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(s.RelPath))); rerr == nil {
						sources[s.RelPath] = raw
					}
				}
			}
			return idxHits, sources, nil
		}
	}

	files, err := candidateFilesForSymbol(repoRoot, relScope, pattern, substr)
	if err != nil {
		return nil, nil, err
	}
	if len(files) == 0 {
		return nil, map[string][]byte{}, nil
	}

	workers := runtime.NumCPU()
	if workers > 6 {
		workers = 6
	}
	if workers < 1 {
		workers = 1
	}
	if len(files) < workers {
		workers = len(files)
	}

	type fileResult struct {
		idx     int
		rel     string
		src     []byte
		matches []astSymbol
	}
	jobs := make(chan int, len(files))
	for i := range files {
		jobs <- i
	}
	close(jobs)

	results := make([]fileResult, len(files))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				f := files[idx]
				src, rerr := os.ReadFile(f)
				if rerr != nil || len(src) > 1024*1024 {
					continue
				}
				rel, _ := filepath.Rel(repoRoot, f)
				rel = filepath.ToSlash(rel)
				syms, _, perr := extractFileSymbolsCached(f, rel, src)
				if perr != nil {
					continue
				}
				var local []astSymbol
				for _, s := range flattenSymbols(syms) {
					if len(incKinds) > 0 && !incKinds[s.KindNum] {
						continue
					}
					if len(excKinds) > 0 && excKinds[s.KindNum] {
						continue
					}
					if matchSymbolPattern(s, pattern, substr) {
						local = append(local, s)
					}
				}
				if len(local) > 0 {
					results[idx] = fileResult{idx: idx, rel: rel, src: src, matches: local}
				}
			}
		}()
	}
	wg.Wait()

	var matches []astSymbol
	sources := make(map[string][]byte)
	for _, r := range results {
		for _, m := range r.matches {
			matches = append(matches, m)
			sources[r.rel] = r.src
			if len(matches) >= maxMatches {
				return matches, sources, nil
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
	incInfo := argBool(args, "include_info", false)
	depth := argInt(args, "depth", 0)
	maxMatches := argInt(args, "max_matches", 20)
	incKinds := argIntSet(args, "include_kinds")
	excKinds := argIntSet(args, "exclude_kinds")

	matches, sources, err := findSymbolsAcrossRepo(repoRoot, relScope, pat, substr, incKinds, excKinds, maxMatches)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		if !substr {
			hintMatches, _, _ := findSymbolsAcrossRepo(repoRoot, relScope, pat, true, incKinds, excKinds, 5)
			if len(hintMatches) > 0 {
				var b strings.Builder
				fmt.Fprintf(&b, "No exact match for %q. Closest symbols:\n", pat)
				for _, m := range hintMatches {
					fmt.Fprintf(&b, "- [%s] %s in %s (L%d-L%d)\n", m.Kind, m.NamePath, m.RelPath, m.StartLine, m.EndLine)
				}
				return b.String(), nil
			}
		}
		return fmt.Sprintf("No symbol matching %q found.", pat), nil
	}

	var b strings.Builder
	for i, m := range matches {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "[%s] %s — %s:L%d-L%d\n", m.Kind, m.NamePath, m.RelPath, m.StartLine, m.EndLine)
		if incInfo {
			absFile := filepath.Join(repoRoot, filepath.FromSlash(m.RelPath))
			if lang := detectLSPLang(absFile); lang != "" {
				if hover := queryWarmLSPHover(repoRoot, lang, absFile, m.NameLine-1, m.NameCol); hover != "" {
					fmt.Fprintf(&b, "  hover: %s\n", firstLineTrimmed(hover))
				}
			}
		}
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

// resolveSingleSymbolInFile requires an exact symbol match for safety on mutating operations.
// When exact matching fails, it reports closest substring candidates in the error message.
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
	syms, _, err := extractFileSymbolsCached(target, rel, src)
	if err != nil {
		return astSymbol{}, "", nil, 0, err
	}
	flat := flattenSymbols(syms)
	var hits []astSymbol
	for _, s := range flat {
		if matchSymbolPattern(s, namePath, false) {
			hits = append(hits, s)
		}
	}
	if len(hits) == 0 {
		var hints []string
		for _, s := range flat {
			if matchSymbolPattern(s, namePath, true) {
				hints = append(hints, fmt.Sprintf("%s (L%d)", s.NamePath, s.StartLine))
				if len(hints) >= 5 {
					break
				}
			}
		}
		if len(hints) > 0 {
			return astSymbol{}, "", nil, 0, fmt.Errorf("exact symbol %q not found in %s (closest: %s)", namePath, rel, strings.Join(hints, ", "))
		}
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
	if rel == "" || np == "" {
		return "", errors.New("relative_path and name_path are required")
	}
	targetPath, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return "", err
	}
	unlock := lockFileWrite(targetPath)
	defer unlock()

	sym, target, src, perm, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	out.Write(src[:sym.StartByte])
	out.WriteString(body)
	if len(body) > 0 && body[len(body)-1] != '\n' && int(sym.EndByte) < len(src) && src[sym.EndByte] != '\n' {
		out.WriteByte('\n')
	}
	out.Write(src[sym.EndByte:])
	newBytes := out.Bytes()
	if err := atomicWriteFile(target, newBytes, perm); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Replaced %s in %s (was L%d-L%d).", sym.NamePath, rel, sym.StartLine, sym.EndLine)
	if diag := checkFileQuick(target); diag != "" {
		msg += "\n[Diagnostics]\n" + diag
	}
	return msg, nil
}

func toolInsertRelativeToSymbol(repoRoot string, args map[string]any, after bool) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path")
	body := argString(args, "body")
	if rel == "" || np == "" {
		return "", errors.New("relative_path and name_path are required")
	}
	targetPath, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return "", err
	}
	unlock := lockFileWrite(targetPath)
	defer unlock()

	sym, target, src, perm, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if !after {
		out.Write(src[:sym.StartByte])
		out.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			out.WriteByte('\n')
		}
		out.Write(src[sym.StartByte:])
	} else {
		out.Write(src[:sym.EndByte])
		if sym.EndByte > 0 && src[sym.EndByte-1] != '\n' {
			out.WriteByte('\n')
		}
		if !strings.HasPrefix(body, "\n") {
			out.WriteByte('\n')
		}
		out.WriteString(body)
		if !strings.HasSuffix(body, "\n") {
			out.WriteByte('\n')
		}
		out.Write(src[sym.EndByte:])
	}
	newBytes := out.Bytes()
	if err := atomicWriteFile(target, newBytes, perm); err != nil {
		return "", err
	}
	dirWord := "before"
	if after {
		dirWord = "after"
	}
	msg := fmt.Sprintf("Inserted code %s %s in %s.", dirWord, sym.NamePath, rel)
	if diag := checkFileQuick(target); diag != "" {
		msg += "\n[Diagnostics]\n" + diag
	}
	return msg, nil
}

func checkGoSyntaxStdlib(absPath string, src []byte) string {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, absPath, src, parser.SkipObjectResolution)
	if err == nil {
		return ""
	}
	base := filepath.Base(absPath)
	rawLines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	var out []string
	for _, ln := range rawLines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		ln = strings.ReplaceAll(ln, absPath, base)
		out = append(out, "[syntax] "+ln)
		if len(out) >= 5 {
			break
		}
	}
	return strings.Join(out, "\n")
}

func checkSyntaxQuick(absPath string, src []byte) string {
	if strings.EqualFold(filepath.Ext(absPath), ".go") {
		return checkGoSyntaxStdlib(absPath, src)
	}
	_, diags, err := extractFileSymbolsCached(absPath, filepath.Base(absPath), src)
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

// runFastLinters invokes external fast correctness linters (oxlint for TS/JS, ty for Python).
func runFastLinters(absPath string) (lines []string, ran bool) {
	ext := strings.ToLower(filepath.Ext(absPath))
	home, _ := os.UserHomeDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		if ox := findToolBin("oxlint",
			filepath.Join(home, ".bun/bin/oxlint"),
			filepath.Join(home, ".local/bin/oxlint"),
		); ox != "" {
			ran = true
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
			ran = true
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
	return lines, ran
}

// checkFileQuick powers `serena check-file <path>`, Serena edit tools, and code_context_pack.
// Runs checks in parallel: Go stdlib go/parser (<1ms), TS oxlint without redundant tree-sitter
// decompression (~50ms), Python ty, plus warm-only LSP diagnostics (NEVER cold-boots LSP).
func checkFileQuick(absPath string) string {
	src, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(absPath))
	isTS := ext == ".ts" || ext == ".tsx" || ext == ".js" || ext == ".jsx" || ext == ".mjs" || ext == ".cjs" || ext == ".mts" || ext == ".cts"

	var (
		synLines  []string
		lintLines []string
		lintRan   bool
		lspLines  []string
		wg        sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		lintLines, lintRan = runFastLinters(absPath)
	}()
	go func() {
		defer wg.Done()
		if repoRoot := findGitRoot(filepath.Dir(absPath)); repoRoot != "" {
			if lang := detectLSPLang(absPath); lang != "" {
				lspLines = queryWarmLSPDiagnostics(repoRoot, lang, absPath)
			}
		}
	}()

	// Run syntax check in parallel for non-TS files (Go uses <1ms stdlib go/parser).
	if !isTS {
		if syn := checkSyntaxQuick(absPath, src); syn != "" {
			synLines = strings.Split(syn, "\n")
		}
	}
	wg.Wait()

	// For TS/JS, only fall back to tree-sitter syntax parse if oxlint is not installed.
	if isTS && !lintRan {
		if syn := checkSyntaxQuick(absPath, src); syn != "" {
			synLines = strings.Split(syn, "\n")
		}
	}

	var all []string
	all = append(all, synLines...)
	all = append(all, lintLines...)
	all = append(all, lspLines...)

	seen := map[string]bool{}
	var uniq []string
	for _, l := range all {
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
