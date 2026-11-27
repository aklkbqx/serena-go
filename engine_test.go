package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTier0MemoriesAndFS(t *testing.T) {
	repo := t.TempDir()

	// initial_instructions & onboarding
	if got, err := dispatchNativeTool(repo, "initial_instructions", nil); err != nil || !strings.Contains(got, "code_context_pack") {
		t.Fatalf("initial_instructions unexpected: %q err=%v", got, err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := dispatchNativeTool(repo, "onboarding", nil); err != nil || !strings.Contains(got, "go") {
		t.Fatalf("onboarding unexpected: %q err=%v", got, err)
	}

	// Memory CRUD
	if _, err := dispatchNativeTool(repo, "write_memory", map[string]any{
		"memory_name": "arch",
		"content":     "# Arch\nUse Go + AST.\n",
	}); err != nil {
		t.Fatalf("write_memory: %v", err)
	}
	if got, err := dispatchNativeTool(repo, "list_memories", nil); err != nil || !strings.Contains(got, `"arch"`) {
		t.Fatalf("list_memories: %q err=%v", got, err)
	}
	if got, err := dispatchNativeTool(repo, "read_memory", map[string]any{"memory_name": "arch"}); err != nil || !strings.Contains(got, "Use Go + AST.") {
		t.Fatalf("read_memory: %q err=%v", got, err)
	}
	if _, err := dispatchNativeTool(repo, "edit_memory", map[string]any{
		"memory_name": "arch",
		"needle":      "Go + AST",
		"repl":        "Pure Go + gotreesitter",
		"mode":        "literal",
	}); err != nil {
		t.Fatalf("edit_memory: %v", err)
	}
	if got, err := dispatchNativeTool(repo, "read_memory", map[string]any{"memory_name": "arch.md"}); err != nil || !strings.Contains(got, "Pure Go + gotreesitter") {
		t.Fatalf("read_memory after edit: %q err=%v", got, err)
	}
	if _, err := dispatchNativeTool(repo, "rename_memory", map[string]any{"old_name": "arch", "new_name": "architecture"}); err != nil {
		t.Fatalf("rename_memory: %v", err)
	}
	if _, err := dispatchNativeTool(repo, "delete_memory", map[string]any{"memory_name": "architecture"}); err != nil {
		t.Fatalf("delete_memory: %v", err)
	}
	// Path traversal must fail
	if _, err := dispatchNativeTool(repo, "read_memory", map[string]any{"memory_name": "../../etc/passwd"}); err == nil {
		t.Fatalf("expected path traversal rejection")
	}
}

func TestTier1ASTOutlineFindSurgeryAndContextPack(t *testing.T) {
	repo := t.TempDir()
	typesTS := `export interface UserProfile {
  id: string;
  email: string;
}

export function formatEmail(email: string): string {
  return email.trim().toLowerCase();
}
`
	serviceTS := `import { UserProfile, formatEmail } from "./types";

export class AuthService {
  normalizeUser(user: UserProfile): string {
    return formatEmail(user.email);
  }
}

export const helperArrow = (x: number): number => {
  return x + 1;
};
`
	callerTS := `import { AuthService } from "./service";

export function runLogin() {
  const svc = new AuthService();
  return svc.normalizeUser({ id: "1", email: "A@B.COM" });
}
`
	if err := os.WriteFile(filepath.Join(repo, "types.ts"), []byte(typesTS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "service.ts"), []byte(serviceTS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "caller.ts"), []byte(callerTS), 0o644); err != nil {
		t.Fatal(err)
	}

	// Overview
	ov, err := dispatchNativeTool(repo, "get_symbols_overview", map[string]any{"relative_path": "service.ts", "depth": 2})
	if err != nil {
		t.Fatalf("get_symbols_overview: %v", err)
	}
	for _, want := range []string{"AuthService", "AuthService/normalizeUser", "helperArrow"} {
		if !strings.Contains(ov, want) {
			t.Fatalf("overview missing %q:\n%s", want, ov)
		}
	}

	// Find symbol across repo with body
	found, err := dispatchNativeTool(repo, "find_symbol", map[string]any{
		"name_path_pattern": "AuthService/normalizeUser",
		"include_body":      true,
	})
	if err != nil || !strings.Contains(found, "formatEmail(user.email)") {
		t.Fatalf("find_symbol failed: %q err=%v", found, err)
	}

	// Compound 1-shot code_context_pack
	pack, err := dispatchNativeTool(repo, "code_context_pack", map[string]any{
		"symbol":        "AuthService/normalizeUser",
		"relative_path": "service.ts",
	})
	if err != nil {
		t.Fatalf("code_context_pack: %v", err)
	}
	for _, want := range []string{
		"AuthService/normalizeUser",
		"formatEmail(user.email)",
		"types.ts",
		"UserProfile",
		"formatEmail",
		"caller.ts",
	} {
		if !strings.Contains(pack, want) {
			t.Fatalf("code_context_pack missing %q:\n%s", want, pack)
		}
	}

	// Surgical replace_symbol_body + syntax verification
	if _, err := dispatchNativeTool(repo, "replace_symbol_body", map[string]any{
		"relative_path": "service.ts",
		"name_path":     "helperArrow",
		"body":          "export const helperArrow = (x: number): number => x * 2;",
	}); err != nil {
		t.Fatalf("replace_symbol_body: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(repo, "service.ts"))
	if !strings.Contains(string(after), "x * 2;") {
		t.Fatalf("replace_symbol_body did not persist:\n%s", after)
	}

	// Broken syntax triggers immediate syntax warning in checkFileQuick
	badFile := filepath.Join(repo, "broken.go")
	_ = os.WriteFile(badFile, []byte("package main\nfunc broken( {\n"), 0o644)
	if diag := checkFileQuick(badFile); diag == "" || !strings.Contains(diag, "[syntax]") {
		t.Fatalf("expected syntax diagnostic for broken.go, got %q", diag)
	}
}

const mockLSPServerPy = `#!/usr/bin/env python3
import json, os, sys

spawn_log = os.environ.get("MOCK_LSP_SPAWNS", "")
if spawn_log:
    with open(spawn_log, "a") as f:
        f.write(f"spawn:{os.getpid()}\n")

def read_msg():
    clen = None
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            return None
        line = line.decode("utf-8").strip()
        if not line:
            break
        if line.lower().startswith("content-length:"):
            clen = int(line.split(":", 1)[1].strip())
    if not clen:
        return None
    body = sys.stdin.buffer.read(clen)
    return json.loads(body.decode("utf-8"))

def send_msg(obj):
    raw = json.dumps(obj).encode("utf-8")
    sys.stdout.buffer.write(f"Content-Length: {len(raw)}\r\n\r\n".encode("utf-8") + raw)
    sys.stdout.buffer.flush()

docs = {}

while True:
    msg = read_msg()
    if msg is None:
        break
    method = msg.get("method", "")
    mid = msg.get("id")
    params = msg.get("params") or {}
    if method == "initialize":
        send_msg({"jsonrpc": "2.0", "id": mid, "result": {"capabilities": {"referencesProvider": True, "definitionProvider": True, "implementationProvider": True, "renameProvider": True}}})
    elif method == "textDocument/didOpen":
        td = params["textDocument"]
        docs[td["uri"]] = td["text"]
    elif method == "textDocument/didChange":
        td = params["textDocument"]
        docs[td["uri"]] = params["contentChanges"][0]["text"]
    elif method == "textDocument/references":
        uri = params["textDocument"]["uri"]
        line = params["position"]["line"]
        # Return a reference on line 3 if symbol is on line 0 (func Target), else empty
        if line == 1:
            send_msg({"jsonrpc": "2.0", "id": mid, "result": [
                {"uri": uri, "range": {"start": {"line": 4, "character": 10}, "end": {"line": 4, "character": 16}}}
            ]})
        else:
            send_msg({"jsonrpc": "2.0", "id": mid, "result": []})
    elif method == "textDocument/definition":
        uri = params["textDocument"]["uri"]
        send_msg({"jsonrpc": "2.0", "id": mid, "result": {"uri": uri, "range": {"start": {"line": 1, "character": 5}, "end": {"line": 1, "character": 11}}}})
    elif method == "textDocument/implementation":
        uri = params["textDocument"]["uri"]
        send_msg({"jsonrpc": "2.0", "id": mid, "result": [{"uri": uri, "range": {"start": {"line": 1, "character": 5}, "end": {"line": 1, "character": 11}}}]})
    elif method == "textDocument/diagnostic":
        send_msg({"jsonrpc": "2.0", "id": mid, "result": {"items": [
            {"range": {"start": {"line": 1, "character": 0}, "end": {"line": 1, "character": 5}}, "severity": 2, "source": "mock-lsp", "message": "unused helper"}
        ]}})
    elif method == "textDocument/rename":
        uri = params["textDocument"]["uri"]
        new_name = params["newName"]
        send_msg({"jsonrpc": "2.0", "id": mid, "result": {
            "changes": {
                uri: [
                    {"range": {"start": {"line": 1, "character": 5}, "end": {"line": 1, "character": 11}}, "newText": new_name},
                    {"range": {"start": {"line": 4, "character": 8}, "end": {"line": 4, "character": 14}}, "newText": new_name},
                ]
            }
        }})
    elif method == "shutdown":
        send_msg({"jsonrpc": "2.0", "id": mid, "result": None})
    elif method == "exit":
        break
`

func TestTier2LSPSharingLRUAndTools(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "serena")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	mockLSP := filepath.Join(tmp, "mock_lsp.py")
	if err := os.WriteFile(mockLSP, []byte(mockLSPServerPy), 0o755); err != nil {
		t.Fatal(err)
	}
	lspState := filepath.Join(tmp, "lsp-state")
	spawnLog := filepath.Join(tmp, "lsp-spawns.log")

	repoA := filepath.Join(tmp, "repoA")
	repoB := filepath.Join(tmp, "repoB")
	_ = os.MkdirAll(repoA, 0o755)
	_ = os.MkdirAll(repoB, 0o755)

	goSrc := "package main\nfunc Target() int { return 1 }\nfunc Unused() int { return 2 }\nfunc Main() int {\n\treturn Target()\n}\n"
	_ = os.WriteFile(filepath.Join(repoA, "app.go"), []byte(goSrc), 0o644)
	_ = os.WriteFile(filepath.Join(repoB, "app.go"), []byte(goSrc), 0o644)

	t.Setenv("SERENA_LSP_STATE_DIR", lspState)
	t.Setenv("SERENA_LSP_CMD_GO", "python3 "+mockLSP)
	t.Setenv("SERENA_LSP_IDLE_SECS", "30")
	t.Setenv("SERENA_MAX_LSPS", "1")
	t.Setenv("MOCK_LSP_SPAWNS", spawnLog)

	defer func() {
		entries, _ := os.ReadDir(lspState)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				if m, err := readLSPMeta(filepath.Join(lspState, e.Name())); err == nil {
					killProcessGroup(m.PGID)
				}
			}
		}
	}()

	// Run stdio native MCP against repoA
	callMCP := func(repo string, calls []string) []map[string]any {
		cmd := exec.Command(bin, "start-mcp-server", "--project", repo)
		cmd.Env = os.Environ()
		in, _ := cmd.StdinPipe()
		out, _ := cmd.StdoutPipe()
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		r := bufio.NewReaderSize(out, 64*1024)
		var res []map[string]any
		for _, c := range calls {
			_, _ = io.WriteString(in, c+"\n")
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("read response for %s: %v", c, err)
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("unmarshal %q: %v", line, err)
			}
			res = append(res, m)
		}
		_ = in.Close()
		_ = cmd.Wait()
		return res
	}

	res := callMCP(repoA, []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"find_referencing_symbols","arguments":{"name_path":"Target","relative_path":"app.go"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_diagnostics_for_file","arguments":{"relative_path":"app.go"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"safe_delete_symbol","arguments":{"name_path_pattern":"Target","relative_path":"app.go"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"safe_delete_symbol","arguments":{"name_path_pattern":"Unused","relative_path":"app.go"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"rename_symbol","arguments":{"name_path":"Target","relative_path":"app.go","new_name":"Renamed"}}}`,
	})
	raw3, _ := json.Marshal(res[2])
	if !strings.Contains(string(raw3), "via go LSP") || !strings.Contains(string(raw3), "app.go:L5") {
		t.Fatalf("expected LSP reference at L5 via go LSP, got %s", raw3)
	}
	raw4, _ := json.Marshal(res[3])
	if !strings.Contains(string(raw4), "unused helper") {
		t.Fatalf("expected mock-lsp diagnostic, got %s", raw4)
	}
	raw5, _ := json.Marshal(res[4])
	if !strings.Contains(string(raw5), "refusing safe_delete_symbol") {
		t.Fatalf("expected safe_delete_symbol refusal for Target, got %s", raw5)
	}
	raw6, _ := json.Marshal(res[5])
	if !strings.Contains(string(raw6), "Safely deleted unreferenced symbol Unused") {
		t.Fatalf("expected Unused deletion, got %s", raw6)
	}
	raw7, _ := json.Marshal(res[6])
	if !strings.Contains(string(raw7), "Renamed cleanly") {
		t.Fatalf("expected rename_symbol success, got %s", raw7)
	}
	renamedContent, _ := os.ReadFile(filepath.Join(repoA, "app.go"))
	if !strings.Contains(string(renamedContent), "func Renamed()") {
		t.Fatalf("expected func Renamed() in app.go, got:\n%s", renamedContent)
	}

	// Verify only 1 LSP spawn happened across all 5 LSP tool calls on repoA!
	spawns1, _ := os.ReadFile(spawnLog)
	if n := len(strings.Fields(string(spawns1))); n != 1 {
		t.Fatalf("expected 1 shared LSP spawn for repoA, got %d", n)
	}

	// Waking repoB with SERENA_MAX_LSPS=1 must evict repoA cleanly!
	_ = callMCP(repoB, []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_diagnostics_for_file","arguments":{"relative_path":"app.go"}}}`,
	})
	time.Sleep(200 * time.Millisecond)
	metaFiles := 0
	entries, _ := os.ReadDir(lspState)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			metaFiles++
		}
	}
	if metaFiles != 1 {
		t.Fatalf("expected Global LRU Cap (max 1) to leave 1 active meta, got %d", metaFiles)
	}
}
