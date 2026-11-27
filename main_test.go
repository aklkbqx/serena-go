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

func TestParseStartArgsNormalizationAndKeyDeterminism(t *testing.T) {
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(tmp, "repo-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}

	c1 := parseStartArgs([]string{"start-mcp-server", "--context=antigravity", "--project-from-cwd"}[1:], realDir)
	c2 := parseStartArgs([]string{"--context", "antigravity", "--project", linkDir, "--enable-web-dashboard=true"}, "/elsewhere")
	c3 := parseStartArgs([]string{"--context=claude-code", "--project-from-cwd"}, realDir)

	if c1.passthrough || c2.passthrough || c3.passthrough {
		t.Fatalf("expected non-passthrough configs")
	}
	if k1, k2 := daemonKey(c1), daemonKey(c2); k1 != k2 {
		t.Fatalf("expected identical key across cwd/symlink project forms, got %s vs %s", k1, k2)
	}
	if k1, k3 := daemonKey(c1), daemonKey(c3); k1 != k3 {
		t.Fatalf("expected unified key across contexts (agy/claude share 1 daemon per repo), got %s vs %s", k1, k3)
	}
}

func TestParseStartArgsPassthroughFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--transport", "streamable-http", "--port", "8000"},
		{"--transport=sse"},
		{"--help"},
		{"--port=9999"},
	} {
		cfg := parseStartArgs(args, "/tmp")
		if !cfg.passthrough {
			t.Fatalf("expected passthrough for args=%v", args)
		}
	}
}

func TestExtractSSEPayloads(t *testing.T) {
	sse := "event: message\r\nid: 1\r\ndata: {\"jsonrpc\": \"2.0\", \"id\": 1, \"result\": {\"ok\": true}}\r\n\r\n: keepalive\r\n\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":2}\n"
	lines, err := extractSSEPayloads([]byte(sse))
	if err != nil {
		t.Fatalf("extractSSEPayloads error: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 payloads, got %d (%q)", len(lines), lines)
	}
	if string(lines[0]) != `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` {
		t.Fatalf("unexpected payload[0]: %s", lines[0])
	}
	if string(lines[1]) != `{"jsonrpc":"2.0","id":2}` {
		t.Fatalf("unexpected payload[1]: %s", lines[1])
	}
}

const mockSerenaHTTPScript = `#!/usr/bin/env python3
import http.server, json, os, sys, uuid

spawn_log = os.environ["MOCK_SPAWN_LOG"]
with open(spawn_log, "a") as f:
    f.write(f"spawn:{os.getpid()}\n")

args = sys.argv[1:]
port = int(args[args.index("--port") + 1])
sessions = set()

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass
    def do_GET(self):
        self.send_response(406)
        self.end_headers()
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(n))
        method = body.get("method")
        mid = body.get("id")
        if method == "initialize":
            sid = uuid.uuid4().hex
            sessions.add(sid)
            proto = (body.get("params") or {}).get("protocolVersion", "2024-11-05")
            resp = {"jsonrpc": "2.0", "id": mid, "result": {"protocolVersion": proto, "capabilities": {"tools": {}}, "serverInfo": {"name": "mock-serena", "version": "1.0"}}}
            out = json.dumps(resp).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Mcp-Session-Id", sid)
            self.end_headers()
            self.wfile.write(out)
            return
        sid = self.headers.get("Mcp-Session-Id", "")
        if sid not in sessions:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"missing session")
            return
        if method == "notifications/initialized":
            self.send_response(202)
            self.end_headers()
            return
        if method == "tools/list":
            resp = {"jsonrpc": "2.0", "id": mid, "result": {"tools": [{"name": "find_symbol", "inputSchema": {"type": "object"}}]}}
        elif method == "tools/call":
            resp = {"jsonrpc": "2.0", "id": mid, "result": {"content": [{"type": "text", "text": f"ok:{os.getpid()}"}]}}
        else:
            resp = {"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": "not found"}}
        out = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(out)

http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
`

func TestSchemaCacheAvoidsColdDaemonUntilToolCall(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "serena")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build serena-shim: %v\n%s", err, out)
	}
	mockReal := filepath.Join(tmp, "real_serena.py")
	if err := os.WriteFile(mockReal, []byte(mockSerenaHTTPScript), 0o755); err != nil {
		t.Fatal(err)
	}
	spawnLog := filepath.Join(tmp, "spawns.log")
	stateDir := filepath.Join(tmp, "state")
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"SERENA_SHIM_REAL_BIN="+mockReal,
		"SERENA_SHIM_STATE_DIR="+stateDir,
		"MOCK_SPAWN_LOG="+spawnLog,
	)

	readLine := func(r *bufio.Reader) map[string]any {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		return m
	}
	countSpawns := func() int {
		b, err := os.ReadFile(spawnLog)
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(b)))
	}
	killDaemons := func() {
		entries, _ := os.ReadDir(stateDir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".schema.json") {
				if m, err := readMeta(filepath.Join(stateDir, e.Name())); err == nil {
					killProcessGroup(m.PGID)
					_ = os.Remove(filepath.Join(stateDir, e.Name()))
				}
			}
		}
	}
	defer killDaemons()

	// Session 1 (cold): spawns daemon to populate .schema.json
	cmd1 := exec.Command(bin, "start-mcp-server", "--context=antigravity", "--project", repoDir)
	cmd1.Env = env
	in1, _ := cmd1.StdinPipe()
	out1, _ := cmd1.StdoutPipe()
	cmd1.Stderr = os.Stderr
	if err := cmd1.Start(); err != nil {
		t.Fatal(err)
	}
	r1 := bufio.NewReaderSize(out1, 64*1024)
	_, _ = io.WriteString(in1, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`+"\n")
	if res := readLine(r1); res["result"] == nil {
		t.Fatalf("cold init failed: %v", res)
	}
	_, _ = io.WriteString(in1, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	_, _ = io.WriteString(in1, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	if res := readLine(r1); res["result"] == nil {
		t.Fatalf("cold tools/list failed: %v", res)
	}
	_ = in1.Close()
	_ = cmd1.Wait()
	if got := countSpawns(); got != 1 {
		t.Fatalf("expected 1 daemon spawn on cold session, got %d", got)
	}

	// Simulate idle reap before Session 2 starts
	killDaemons()
	time.Sleep(150 * time.Millisecond)

	// Session 2 (warm schema cache): initialize + tools/list + ping must NOT spawn daemon!
	cmd2 := exec.Command(bin, "start-mcp-server", "--context=antigravity", "--project", repoDir)
	cmd2.Env = env
	in2, _ := cmd2.StdinPipe()
	out2, _ := cmd2.StdoutPipe()
	cmd2.Stderr = os.Stderr
	if err := cmd2.Start(); err != nil {
		t.Fatal(err)
	}
	r2 := bufio.NewReaderSize(out2, 64*1024)
	_, _ = io.WriteString(in2, `{"jsonrpc":"2.0","id":"w1","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`+"\n")
	if res := readLine(r2); res["id"] != "w1" || res["result"] == nil {
		t.Fatalf("warm init failed: %v", res)
	}
	_, _ = io.WriteString(in2, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	_, _ = io.WriteString(in2, `{"jsonrpc":"2.0","id":20,"method":"tools/list"}`+"\n")
	if res := readLine(r2); res["result"] == nil {
		t.Fatalf("warm tools/list failed: %v", res)
	}
	_, _ = io.WriteString(in2, `{"jsonrpc":"2.0","id":21,"method":"ping"}`+"\n")
	if res := readLine(r2); res["result"] == nil {
		t.Fatalf("warm ping failed: %v", res)
	}
	if got := countSpawns(); got != 1 {
		t.Fatalf("expected 0 daemon spawns during warm init/tools/list/ping, total=%d", got)
	}

	// First tools/call lazily wakes daemon #2, replays handshake, and returns tool result
	_, _ = io.WriteString(in2, `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"find_symbol","arguments":{}}}`+"\n")
	if res := readLine(r2); res["result"] == nil {
		t.Fatalf("lazy tools/call failed: %v", res)
	}
	if got := countSpawns(); got != 2 {
		t.Fatalf("expected 2 total spawns after tools/call wake, got %d", got)
	}
	_ = in2.Close()
	_ = cmd2.Wait()
}

func TestDiscoverNotificationsPingNoRespawnAndZeroZombies(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "serena")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build serena-shim: %v\n%s", err, out)
	}
	mockReal := filepath.Join(tmp, "real_serena.py")
	if err := os.WriteFile(mockReal, []byte(mockSerenaHTTPScript), 0o755); err != nil {
		t.Fatal(err)
	}
	spawnLog := filepath.Join(tmp, "spawns.log")
	stateDir := filepath.Join(tmp, "state")
	schemaDir := filepath.Join(tmp, "schema-cache")
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"SERENA_SHIM_REAL_BIN="+mockReal,
		"SERENA_SHIM_STATE_DIR="+stateDir,
		"SERENA_SHIM_SCHEMA_CACHE_DIR="+schemaDir,
		"SERENA_SHIM_INIT_IDLE_SECS=2",
		"SERENA_SHIM_IDLE_SECS=2",
		"MOCK_SPAWN_LOG="+spawnLog,
	)

	readLine := func(r *bufio.Reader) map[string]any {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		return m
	}
	countSpawns := func() int {
		b, err := os.ReadFile(spawnLog)
		if err != nil {
			return 0
		}
		return len(strings.Fields(string(b)))
	}
	killDaemons := func() {
		entries, _ := os.ReadDir(stateDir)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".schema.json") {
				if m, err := readMeta(filepath.Join(stateDir, e.Name())); err == nil {
					killProcessGroup(m.PGID)
					_ = os.Remove(filepath.Join(stateDir, e.Name()))
				}
			}
		}
	}
	defer killDaemons()

	cmd := exec.Command(bin, "start-mcp-server", "--context=antigravity", "--project", repoDir)
	cmd.Env = env
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReaderSize(out, 64*1024)

	// 1. Pre-init server/discover + subscriptions/listen + notifications must NOT spawn daemon!
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"disc1","method":"server/discover","params":{}}`+"\n")
	if res := readLine(r); res["error"] == nil {
		t.Fatalf("expected -32601 error for server/discover, got %v", res)
	}
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"sub1","method":"subscriptions/listen","params":{}}`+"\n")
	if res := readLine(r); res["error"] == nil {
		t.Fatalf("expected -32601 error for subscriptions/listen, got %v", res)
	}
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`+"\n")
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"p0","method":"ping"}`+"\n")
	if res := readLine(r); res["result"] == nil {
		t.Fatalf("expected ping result, got %v", res)
	}
	if got := countSpawns(); got != 0 {
		t.Fatalf("expected 0 spawns for discover/subscriptions/notification/ping, got %d", got)
	}

	// 2. Cold initialize + tools/list -> spawns daemon #1 and populates schemaDir (separate from stateDir!).
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`+"\n")
	if res := readLine(r); res["result"] == nil {
		t.Fatalf("cold init failed: %v", res)
	}
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n")
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`+"\n")
	if res := readLine(r); res["result"] == nil {
		t.Fatalf("cold tools/list failed: %v", res)
	}
	if got := countSpawns(); got != 1 {
		t.Fatalf("expected 1 spawn after cold init, got %d", got)
	}
	schemaEntries, _ := os.ReadDir(schemaDir)
	if len(schemaEntries) == 0 {
		t.Fatalf("expected schema cache file in %s", schemaDir)
	}

	// 3. Keep sending pings every 500ms for 3.5s (past 2s idle timeout); ping MUST NOT reset .activity,
	// so daemon #1 MUST be reaped cleanly and NOT respawned by ping or notifications!
	for i := 0; i < 7; i++ {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"keepalive","method":"ping"}`+"\n")
		if res := readLine(r); res["result"] == nil {
			t.Fatalf("ping %d failed: %v", i, res)
		}
	}
	// Send stray notifications and tools/list after daemon #1 has been reaped (even with stale sessionID!).
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":99}}`+"\n")
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"post-reap-list","method":"tools/list"}`+"\n")
	if res := readLine(r); res["result"] == nil {
		t.Fatalf("post-reap tools/list failed: %v", res)
	}
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"post-reap-ping","method":"ping"}`+"\n")
	if res := readLine(r); res["result"] == nil {
		t.Fatalf("post-reap ping failed: %v", res)
	}
	if got := countSpawns(); got != 1 {
		t.Fatalf("ping/notification/cached-list after idle reap must NOT respawn daemon; spawns=%d", got)
	}

	// Verify daemon #1 meta file was removed by supervisor reap AND zero defunct zombie children remain!
	metaLeft := 0
	if entries, err := os.ReadDir(stateDir); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") && !strings.HasSuffix(e.Name(), ".schema.json") {
				metaLeft++
			}
		}
	}
	if metaLeft != 0 {
		t.Fatalf("expected daemon #1 to be reaped despite pings (meta files left=%d)", metaLeft)
	}

	psOut, err := exec.Command("/bin/ps", "-Ao", "ppid=,stat=,command=").Output()
	if err == nil {
		shimPPID := strings.TrimSpace(filepath.Base(string([]byte{})))
		_ = shimPPID
		for _, ln := range strings.Split(string(psOut), "\n") {
			fields := strings.Fields(ln)
			if len(fields) >= 2 && fields[0] == strings.TrimSpace(func() string {
				b, _ := json.Marshal(cmd.Process.Pid)
				return string(b)
			}()) && strings.Contains(fields[1], "Z") {
				t.Fatalf("found zombie child under serena-shim pid %d: %s", cmd.Process.Pid, ln)
			}
		}
	}

	// 4. Subsequent tools/call cleanly wakes daemon #2!
	_, _ = io.WriteString(in, `{"jsonrpc":"2.0","id":"wake2","method":"tools/call","params":{"name":"find_symbol","arguments":{}}}`+"\n")
	if res := readLine(r); res["result"] == nil {
		t.Fatalf("tools/call wake #2 failed: %v", res)
	}
	if got := countSpawns(); got != 2 {
		t.Fatalf("expected 2 spawns after real tools/call, got %d", got)
	}

	_ = in.Close()
	_ = cmd.Wait()
}

func TestSchemaCacheBaseDirDefaultsToUserCacheDir(t *testing.T) {
	t.Setenv("SERENA_SHIM_STATE_DIR", "")
	t.Setenv("SERENA_SHIM_SCHEMA_CACHE_DIR", "")
	ucd, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no UserCacheDir")
	}
	want := filepath.Join(ucd, "serena-shim")
	if got := schemaCacheBaseDir(); got != want {
		t.Fatalf("schemaCacheBaseDir() = %q, want %q", got, want)
	}
}

