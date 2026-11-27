package main

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func nativeToolCatalog() []mcpToolDef {
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

	return []mcpToolDef{
		{
			Name:        "semantic_code_search",
			Description: "Hybrid AST-chunked BM25 + optional Windows RTX 3070 Ti dense vector code search. Use for intent/concept queries (e.g. 'webhook retry logic' or 'token refresh') instead of blind grep loops.",
			InputSchema: obj(map[string]any{
				"query":         str("Natural language intent or code concept query"),
				"relative_path": str("Optional file or subdirectory scope"),
				"top_k":         num("Max ranked AST chunks to return (default 8)"),
				"include_body":  boo("Include chunk source code snippet (default true)"),
				"max_chars":     num("Max output chars (default 6000)"),
			}, "query"),
		},
		{
			Name:        "code_context_pack",
			Description: "1-shot surgical context pack for a symbol: definition body + 1-hop imported type signatures + top callers + live file diagnostics. Prefer this over multiple read/grep turns.",
			InputSchema: obj(map[string]any{
				"symbol":              str("Symbol name or name_path (e.g. AuthService/login or handleRequest)"),
				"relative_path":       str("Optional file or directory path relative to repo root"),
				"include_callers":     boo("Include top callers/references (default true)"),
				"include_types":       boo("Include 1-hop imported type/function signatures (default true)"),
				"include_diagnostics": boo("Include instant syntax/lint/LSP diagnostics (default true)"),
				"max_chars":           num("Max output chars (default 6000)"),
			}, "symbol"),
		},
		{
			Name:        "get_symbols_overview",
			Description: "Instant AST symbol outline of a file (0 MB idle). Returns symbol kinds, name paths, line spans, and signatures.",
			InputSchema: obj(map[string]any{
				"relative_path":    str("File path relative to repo root"),
				"depth":            num("Max nesting depth (default 1)"),
				"max_answer_chars": num("Max output chars"),
			}, "relative_path"),
		},
		{
			Name:        "find_symbol",
			Description: "Find symbols by name or name_path via fast AST + ripgrep prefilter (no LSP needed). Optionally returns exact body.",
			InputSchema: obj(map[string]any{
				"name_path_pattern":  str("Symbol name or path pattern (e.g. Foo or Class/method)"),
				"relative_path":      str("Optional file or directory to scope search"),
				"depth":              num("Child symbol depth (default 0)"),
				"include_body":       boo("Include symbol source code body"),
				"include_info":       boo("Include signature/hover info"),
				"include_kinds":      arr("integer", "Optional LSP SymbolKind ints to include"),
				"exclude_kinds":      arr("integer", "Optional LSP SymbolKind ints to exclude"),
				"substring_matching": boo("Match symbol name by substring"),
				"max_matches":        num("Max matches to return (default 20)"),
				"max_answer_chars":   num("Max output chars"),
			}, "name_path_pattern"),
		},
		{
			Name:        "find_referencing_symbols",
			Description: "Find semantic references to a symbol across the repo using lazy per-language LSP (with AST/rg fallback).",
			InputSchema: obj(map[string]any{
				"name_path":        str("Symbol name or name_path in relative_path"),
				"relative_path":    str("File containing the symbol"),
				"include_kinds":    arr("integer", "Optional kinds filter"),
				"exclude_kinds":    arr("integer", "Optional kinds exclusion"),
				"max_answer_chars": num("Max output chars"),
			}, "name_path", "relative_path"),
		},
		{
			Name:        "find_declaration",
			Description: "Resolve declaration/definition of an identifier matched by regex inside a file via lazy LSP.",
			InputSchema: obj(map[string]any{
				"relative_path":               str("File containing the reference"),
				"regex":                       str("Regex matching the identifier occurrence"),
				"containing_symbol_name_path": str("Optional enclosing symbol to narrow match"),
				"include_body":                boo("Include declaration body"),
				"include_info":                boo("Include hover/type info"),
			}, "relative_path", "regex"),
		},
		{
			Name:        "find_implementations",
			Description: "Find implementations of an interface/trait/abstract symbol via lazy per-language LSP.",
			InputSchema: obj(map[string]any{
				"name_path":        str("Interface/trait/method name_path"),
				"relative_path":    str("File containing the symbol"),
				"include_info":     boo("Include signature info"),
				"include_kinds":    arr("integer", "Optional kinds filter"),
				"exclude_kinds":    arr("integer", "Optional kinds exclusion"),
				"max_answer_chars": num("Max output chars"),
			}, "name_path", "relative_path"),
		},
		{
			Name:        "get_diagnostics_for_file",
			Description: "Get compiler/LSP + AST syntax + fast linter (oxlint/ty) diagnostics for a file.",
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
			Description: "Replace the exact AST span of a symbol in a file and immediately verify syntax.",
			InputSchema: obj(map[string]any{
				"name_path":     str("Symbol name or name_path"),
				"relative_path": str("File path relative to repo root"),
				"body":          str("New complete symbol definition text"),
			}, "name_path", "relative_path", "body"),
		},
		{
			Name:        "insert_before_symbol",
			Description: "Insert code immediately before a symbol's AST definition.",
			InputSchema: obj(map[string]any{
				"name_path":     str("Target symbol name_path"),
				"relative_path": str("File path relative to repo root"),
				"body":          str("Code to insert before symbol"),
			}, "name_path", "relative_path", "body"),
		},
		{
			Name:        "insert_after_symbol",
			Description: "Insert code immediately after a symbol's AST definition.",
			InputSchema: obj(map[string]any{
				"name_path":     str("Target symbol name_path"),
				"relative_path": str("File path relative to repo root"),
				"body":          str("Code to insert after symbol"),
			}, "name_path", "relative_path", "body"),
		},
		{
			Name:        "rename_symbol",
			Description: "Semantically rename a symbol across the workspace via lazy LSP.",
			InputSchema: obj(map[string]any{
				"name_path":     str("Symbol name or name_path"),
				"relative_path": str("File containing the symbol"),
				"new_name":      str("New identifier name"),
			}, "name_path", "relative_path", "new_name"),
		},
		{
			Name:        "safe_delete_symbol",
			Description: "Verify zero external references via LSP/rg, then safely delete the symbol's AST span.",
			InputSchema: obj(map[string]any{
				"name_path_pattern": str("Symbol name or name_path to delete"),
				"relative_path":     str("File containing the symbol"),
			}, "name_path_pattern", "relative_path"),
		},
		{
			Name:        "search_for_pattern",
			Description: "Fast ripgrep pattern search over repo files (0 MB daemon).",
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
			InputSchema: obj(map[string]any{
				"topic": str("Optional topic/prefix filter"),
			}),
		},
		{
			Name:        "read_memory",
			Description: "Read a project memory file from .serena/memories (0 MB, instant).",
			InputSchema: obj(map[string]any{
				"memory_name": str("Memory name (with or without .md)"),
			}, "memory_name"),
		},
		{
			Name:        "write_memory",
			Description: "Create or overwrite a project memory file in .serena/memories (0 MB, instant).",
			InputSchema: obj(map[string]any{
				"memory_name": str("Memory name"),
				"content":     str("Markdown content"),
				"max_chars":   num("Optional max chars"),
			}, "memory_name", "content"),
		},
		{
			Name:        "edit_memory",
			Description: "Edit a project memory file in .serena/memories using literal or regex replacement.",
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
			InputSchema: obj(map[string]any{
				"memory_name": str("Memory name"),
			}, "memory_name"),
		},
		{
			Name:        "rename_memory",
			Description: "Rename/move a project memory file inside .serena/memories.",
			InputSchema: obj(map[string]any{
				"old_name": str("Existing memory name"),
				"new_name": str("New memory name"),
			}, "old_name", "new_name"),
		},
		{
			Name:        "onboarding",
			Description: "Return a concise repo overview (stack, graft status, memories) in <5ms (0 MB).",
			InputSchema: obj(map[string]any{}),
		},
		{
			Name:        "initial_instructions",
			Description: "Return concise usage instructions for Serena Pure-Go 3-Tier Engine (0 MB).",
			InputSchema: obj(map[string]any{}),
		},
	}
}
