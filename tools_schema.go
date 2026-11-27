package main

import "strings"

type mcpToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

type mcpToolDef struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	InputSchema map[string]any      `json:"inputSchema"`
	Annotations *mcpToolAnnotations `json:"annotations,omitempty"`
}

func nativeToolCatalog(profile string) []mcpToolDef {
	obj := func(props map[string]any, req ...string) map[string]any {
		m := map[string]any{
			"type":       "object",
			"properties": props,
		}
		if len(req) > 0 {
			m["required"] = req
		}
		return m
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	num := func(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }
	boo := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	arr := func(itemType, desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": itemType}, "description": desc}
	}

	ro := &mcpToolAnnotations{ReadOnlyHint: true, DestructiveHint: false, IdempotentHint: true, OpenWorldHint: false}
	edit := &mcpToolAnnotations{ReadOnlyHint: false, DestructiveHint: false, IdempotentHint: false, OpenWorldHint: false}
	destr := &mcpToolAnnotations{ReadOnlyHint: false, DestructiveHint: true, IdempotentHint: false, OpenWorldHint: false}

	all := []mcpToolDef{
		{
			Name:        "semantic_code_search",
			Description: "Hybrid AST-chunked BM25 + optional OpenAI-compatible GPU dense vector code search. Use for intent/concept queries (English or multilingual) instead of blind grep loops.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"query":         str("Natural language intent or code concept query"),
				"relative_path": str("Optional file or subdirectory scope"),
				"top_k":         num("Max ranked AST chunks to return (default 8)"),
				"include_body":  boo("Include source snippet (default true; top-3 full, rest signatures)"),
				"max_chars":     num("Max output chars (default 6000)"),
			}, "query"),
		},
		{
			Name:        "code_context_pack",
			Description: "1-shot surgical context pack for one or more symbols: definition body + imported/local type bodies + callers + callees + warm LSP hover + diagnostics. Prefer over multiple read/grep turns.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"symbol":              str("Single symbol name or name_path (e.g. AuthService/login)"),
				"symbols":             arr("string", "Optional batch of symbol names/name_paths to inspect together with deduplicated types"),
				"relative_path":       str("Optional file or directory path relative to repo root"),
				"include_callers":     boo("Include top callers/references (default true)"),
				"include_types":       boo("Include referenced local/imported type bodies & signatures (default true)"),
				"include_diagnostics": boo("Include instant syntax/lint/LSP diagnostics (default true)"),
				"max_chars":           num("Max output chars (default 6000)"),
			}),
		},
		{
			Name:        "changed_symbols",
			Description: "Map current git diff/staged hunks to affected AST symbols, top callers, and live diagnostics in 1 call. Ideal for code review and verifying edits.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"base":                str("Optional git ref to diff against (default: working tree + staged)"),
				"staged":              boo("Inspect only staged changes (default false)"),
				"include_callers":     boo("Include callers of modified symbols (default true)"),
				"include_diagnostics": boo("Include quick diagnostics on modified files (default true)"),
				"max_chars":           num("Max output chars (default 6000)"),
			}),
		},
		{
			Name:        "get_symbols_overview",
			Description: "Instant AST symbol outline of a file (0 MB idle). Returns symbol kinds, name paths, line spans, and compact signatures.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"relative_path":    str("File path relative to repo root"),
				"depth":            num("Max nesting depth (default 1)"),
				"max_answer_chars": num("Max output chars"),
			}, "relative_path"),
		},
		{
			Name:        "find_symbol",
			Description: "Find symbols by name or name_path via indexed/AST lookup (no LSP needed). Optionally returns exact body and warm hover info.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"name_path_pattern":  str("Symbol name or path pattern (e.g. Foo or Class/method)"),
				"relative_path":      str("Optional file or directory to scope search"),
				"depth":              num("Child symbol depth (default 0)"),
				"include_body":       boo("Include symbol source code body"),
				"include_info":       boo("Include warm LSP hover/type info when available"),
				"include_kinds":      arr("integer", "Optional LSP SymbolKind ints to include"),
				"exclude_kinds":      arr("integer", "Optional LSP SymbolKind ints to exclude"),
				"substring_matching": boo("Match symbol name by case-insensitive substring"),
				"max_matches":        num("Max matches to return (default 20)"),
				"max_answer_chars":   num("Max output chars"),
			}, "name_path_pattern"),
		},
		{
			Name:        "find_referencing_symbols",
			Description: "Find semantic references to a symbol across the repo using lazy per-language LSP (with AST/rg fallback).",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"name_path":        str("Symbol name or name_path in relative_path"),
				"relative_path":    str("File containing the symbol"),
				"max_answer_chars": num("Max output chars"),
			}, "name_path", "relative_path"),
		},
		{
			Name:        "find_declaration",
			Description: "Resolve declaration/definition of an identifier matched by regex inside a file via lazy LSP.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"relative_path":               str("File containing the reference"),
				"regex":                       str("Regex matching the identifier occurrence"),
				"containing_symbol_name_path": str("Optional enclosing symbol to narrow match"),
				"include_body":                boo("Include declaration body"),
				"include_info":                boo("Include warm LSP hover/type info"),
			}, "relative_path", "regex"),
		},
		{
			Name:        "find_implementations",
			Description: "Find implementations of an interface/trait/abstract symbol via lazy per-language LSP.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"name_path":        str("Interface/trait/method name_path"),
				"relative_path":    str("File containing the symbol"),
				"max_answer_chars": num("Max output chars"),
			}, "name_path", "relative_path"),
		},
		{
			Name:        "get_diagnostics_for_file",
			Description: "Get compiler/LSP + AST syntax + fast linter (oxlint/ty) diagnostics for a file.",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"relative_path":    str("File path relative to repo root"),
				"start_line":       num("Optional 1-based start line filter"),
				"end_line":         num("Optional 1-based end line filter"),
				"min_severity":     str("Optional min severity: error|warning|info|hint"),
				"max_answer_chars": num("Max output chars"),
			}, "relative_path"),
		},
		{
			Name:        "replace_symbol_body",
			Description: "Replace the exact AST span of a symbol in a file and immediately verify syntax/lint.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"name_path":     str("Exact symbol name or name_path"),
				"relative_path": str("File path relative to repo root"),
				"body":          str("New complete symbol definition text"),
			}, "name_path", "relative_path", "body"),
		},
		{
			Name:        "insert_before_symbol",
			Description: "Insert code immediately before a symbol's exact AST definition.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"name_path":     str("Exact target symbol name_path"),
				"relative_path": str("File path relative to repo root"),
				"body":          str("Code to insert before symbol"),
			}, "name_path", "relative_path", "body"),
		},
		{
			Name:        "insert_after_symbol",
			Description: "Insert code immediately after a symbol's exact AST definition.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"name_path":     str("Exact target symbol name_path"),
				"relative_path": str("File path relative to repo root"),
				"body":          str("Code to insert after symbol"),
			}, "name_path", "relative_path", "body"),
		},
		{
			Name:        "rename_symbol",
			Description: "Semantically rename a symbol across the workspace via lazy LSP.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"name_path":     str("Exact symbol name or name_path"),
				"relative_path": str("File containing the symbol"),
				"new_name":      str("New identifier name"),
			}, "name_path", "relative_path", "new_name"),
		},
		{
			Name:        "safe_delete_symbol",
			Description: "Verify zero external references via LSP/rg, then safely delete the symbol's exact AST span.",
			Annotations: destr,
			InputSchema: obj(map[string]any{
				"name_path_pattern": str("Exact symbol name or name_path to delete"),
				"relative_path":     str("File containing the symbol"),
			}, "name_path_pattern", "relative_path"),
		},
		{
			Name:        "search_for_pattern",
			Description: "Fast ripgrep pattern search over repo files (0 MB daemon).",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"substring_pattern":             str("Regex or substring pattern"),
				"relative_path":                 str("Optional subdir or file"),
				"paths_include_glob":            str("Optional include glob"),
				"paths_exclude_glob":            str("Optional exclude glob"),
				"context_lines_before":          num("Context lines before match"),
				"context_lines_after":           num("Context lines after match"),
				"multiline":                     boo("Allow multiline regex matching"),
				"restrict_search_to_code_files": boo("Limit search to code files"),
				"skip_ignored_files":            boo("Respect .gitignore (default true)"),
				"max_answer_chars":              num("Max output chars"),
			}, "substring_pattern"),
		},
		{
			Name:        "replace_content",
			Description: "Replace literal or regex text inside a single file atomically.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"relative_path":              str("File path relative to repo root"),
				"needle":                     str("Target text or regex"),
				"repl":                       str("Replacement text"),
				"mode":                       str("literal or regex"),
				"allow_multiple_occurrences": boo("Allow replacing multiple matches"),
			}, "relative_path", "needle", "repl", "mode"),
		},
		{
			Name:        "replace_in_files",
			Description: "Replace literal or regex text across matching repo files atomically.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"needle":             str("Target text or regex"),
				"repl":               str("Replacement text"),
				"mode":               str("literal or regex"),
				"relative_path":      str("Optional subdir/file scope"),
				"paths_include_glob": str("Optional include glob"),
				"paths_exclude_glob": str("Optional exclude glob"),
				"dry_run":            boo("Preview matches without modifying files"),
				"expected_count":     num("Optional expected replacement count"),
				"max_answer_chars":   num("Max output chars"),
			}, "needle", "repl", "mode"),
		},
		{
			Name:        "list_memories",
			Description: "List project memory files in .serena/memories (0 MB, instant).",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"topic": str("Optional topic/prefix filter"),
			}),
		},
		{
			Name:        "read_memory",
			Description: "Read a project memory file from .serena/memories (0 MB, instant).",
			Annotations: ro,
			InputSchema: obj(map[string]any{
				"memory_name": str("Memory name (with or without .md)"),
			}, "memory_name"),
		},
		{
			Name:        "write_memory",
			Description: "Create or overwrite a project memory file in .serena/memories (0 MB, instant).",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"memory_name": str("Memory name"),
				"content":     str("Markdown content"),
				"max_chars":   num("Optional max chars"),
			}, "memory_name", "content"),
		},
		{
			Name:        "edit_memory",
			Description: "Edit a project memory file in .serena/memories using literal or regex replacement.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"memory_name":                str("Memory name"),
				"needle":                     str("Target text or regex"),
				"repl":                       str("Replacement text"),
				"mode":                       str("literal or regex"),
				"allow_multiple_occurrences": boo("Allow multiple replacements"),
			}, "memory_name", "needle", "repl", "mode"),
		},
		{
			Name:        "delete_memory",
			Description: "Delete a project memory file from .serena/memories.",
			Annotations: destr,
			InputSchema: obj(map[string]any{
				"memory_name": str("Memory name"),
			}, "memory_name"),
		},
		{
			Name:        "rename_memory",
			Description: "Rename/move a project memory file inside .serena/memories.",
			Annotations: edit,
			InputSchema: obj(map[string]any{
				"old_name": str("Existing memory name"),
				"new_name": str("New memory name"),
			}, "old_name", "new_name"),
		},
		{
			Name:        "onboarding",
			Description: "Return a concise repo overview (stack, graft status, memories) in <5ms (0 MB).",
			Annotations: ro,
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "initial_instructions",
			Description: "Return concise usage instructions for serena-go 3-Tier Engine (0 MB).",
			Annotations: ro,
			InputSchema: obj(map[string]any{}),
		},
	}

	if strings.EqualFold(strings.TrimSpace(profile), "slim") {
		keep := map[string]bool{
			"semantic_code_search":     true,
			"code_context_pack":        true,
			"changed_symbols":          true,
			"get_symbols_overview":     true,
			"find_symbol":              true,
			"find_referencing_symbols": true,
			"find_declaration":         true,
			"find_implementations":     true,
			"get_diagnostics_for_file": true,
			"replace_symbol_body":      true,
			"rename_symbol":            true,
			"safe_delete_symbol":       true,
			"list_memories":            true,
			"read_memory":              true,
			"write_memory":             true,
		}
		slim := make([]mcpToolDef, 0, len(keep))
		for _, t := range all {
			if keep[t.Name] {
				slim = append(slim, t)
			}
		}
		return slim
	}
	return all
}
