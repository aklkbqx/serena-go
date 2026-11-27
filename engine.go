package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

func dispatchNativeTool(repoRoot, name string, args map[string]any) (string, error) {
	switch name {
	// Tier 0: Instant Native Go FS / ripgrep (0 MB)
	case "initial_instructions":
		return toolInitialInstructions(), nil
	case "onboarding":
		return toolOnboarding(repoRoot), nil
	case "list_memories":
		return toolListMemories(repoRoot, args)
	case "read_memory":
		return toolReadMemory(repoRoot, args)
	case "write_memory":
		return toolWriteMemory(repoRoot, args)
	case "edit_memory":
		return toolEditMemory(repoRoot, args)
	case "delete_memory":
		return toolDeleteMemory(repoRoot, args)
	case "rename_memory":
		return toolRenameMemory(repoRoot, args)
	case "search_for_pattern":
		return toolSearchForPattern(repoRoot, args)
	case "replace_content":
		return toolReplaceContent(repoRoot, args)
	case "replace_in_files":
		return toolReplaceInFiles(repoRoot, args)

	// Tier 1: Stateless Pure-Go gotreesitter AST + Hybrid Semantic Search (0 MB idle, <25ms)
	case "semantic_code_search":
		return toolSemanticCodeSearch(repoRoot, args)
	case "code_context_pack":
		return toolCodeContextPack(repoRoot, args)
	case "get_symbols_overview":
		return toolGetSymbolsOverview(repoRoot, args)
	case "find_symbol":
		return toolFindSymbol(repoRoot, args)
	case "replace_symbol_body":
		return toolReplaceSymbolBody(repoRoot, args)
	case "insert_before_symbol":
		return toolInsertRelativeToSymbol(repoRoot, args, false)
	case "insert_after_symbol":
		return toolInsertRelativeToSymbol(repoRoot, args, true)

	// Tier 2: On-Demand Per-Language Lazy LSP (shared across agents, LRU + pressure governed)
	case "find_referencing_symbols":
		return toolFindReferencingSymbols(repoRoot, args)
	case "find_declaration":
		return toolFindDeclaration(repoRoot, args)
	case "find_implementations":
		return toolFindImplementations(repoRoot, args)
	case "rename_symbol":
		return toolRenameSymbol(repoRoot, args)
	case "get_diagnostics_for_file":
		return toolGetDiagnosticsForFile(repoRoot, args)
	case "safe_delete_symbol":
		return toolSafeDeleteSymbol(repoRoot, args)

	default:
		return "", fmt.Errorf("unknown tool %q", name)
	}
}

func runNativeMCP(cfg parsedConfig) int {
	repoRoot := cfg.project
	if repoRoot == "" {
		cwd, _ := os.Getwd()
		repoRoot = canonicalPath(cwd)
	}
	tools := nativeToolCatalog()

	reader := bufio.NewReaderSize(os.Stdin, 256*1024)
	var stdoutMu sync.Mutex
	var wg sync.WaitGroup

	writeJSON := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		stdoutMu.Lock()
		_, _ = os.Stdout.Write(append(b, '\n'))
		stdoutMu.Unlock()
	}

	handleReq := func(raw []byte) {
		var env rpcEnvelope
		if json.Unmarshal(raw, &env) != nil {
			return
		}
		if !hasRPCID(env.ID) || env.Method == "" {
			return
		}
		idRaw := json.RawMessage(bytes.TrimSpace(env.ID))

		switch env.Method {
		case "initialize":
			proto := extractProtoVersion(env.Params)
			if proto == "" {
				proto = "2024-11-05"
			}
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      idRaw,
				"result": map[string]any{
					"protocolVersion": proto,
					"capabilities": map[string]any{
						"tools": map[string]any{"listChanged": false},
					},
					"serverInfo": map[string]any{
						"name":    "serena-go",
						"version": buildVersion,
					},
					"instructions": "Serena Pure-Go 3-Tier Engine. Prefer code_context_pack and find_symbol/get_symbols_overview (0-RAM AST) before reading whole files.",
				},
			})
		case "ping":
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{}})
		case "tools/list":
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      idRaw,
				"result":  map[string]any{"tools": tools},
			})
		case "resources/list":
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{"resources": []any{}}})
		case "resources/templates/list":
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{"resourceTemplates": []any{}}})
		case "prompts/list":
			writeJSON(map[string]any{"jsonrpc": "2.0", "id": idRaw, "result": map[string]any{"prompts": []any{}}})
		case "server/discover", "subscriptions/listen":
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      idRaw,
				"error":   map[string]any{"code": -32601, "message": "Method not found"},
			})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(env.Params, &p); err != nil {
				writeJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      idRaw,
					"error":   map[string]any{"code": -32602, "message": "invalid params: " + err.Error()},
				})
				return
			}
			text, terr := dispatchNativeTool(repoRoot, p.Name, p.Arguments)
			if terr != nil {
				writeJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      idRaw,
					"result": map[string]any{
						"isError": true,
						"content": []map[string]any{{"type": "text", "text": terr.Error()}},
					},
				})
				return
			}
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      idRaw,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				},
			})
		default:
			writeJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      idRaw,
				"error":   map[string]any{"code": -32601, "message": "Method not found: " + env.Method},
			})
		}
	}

	for {
		lineBytes, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(lineBytes)
		if len(trimmed) > 0 {
			msgCopy := append([]byte(nil), trimmed...)
			method := parseMethod(msgCopy)
			if method == "initialize" || method == "notifications/initialized" || method == "server/discover" {
				handleReq(msgCopy)
			} else {
				wg.Add(1)
				go func(req []byte) {
					defer wg.Done()
					handleReq(req)
				}(msgCopy)
			}
		}
		if err != nil {
			break
		}
	}
	wg.Wait()
	return 0
}
