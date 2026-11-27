package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRemoteLSPPathAndURITranslationAndFailover(t *testing.T) {
	t.Setenv("SERENA_CONFIG", "none")

	localRoot := "/workspace/sample-repo"
	remoteRoot := "/srv/work/sample-repo"

	// Local -> Remote
	gotRemote := mapLocalPathToRemote(localRoot+"/pkg/mcp/server.go", localRoot, remoteRoot)
	if gotRemote != "/srv/work/sample-repo/pkg/mcp/server.go" {
		t.Fatalf("mapLocalPathToRemote = %q", gotRemote)
	}

	// Remote URI -> Local URI
	gotLocalURI := mapRemoteURIToLocalURI("file:///srv/work/sample-repo/pkg/mcp/server.go", localRoot, remoteRoot)
	wantLocalURI := fileToURI(filepath.Join(canonicalPath(localRoot), "pkg/mcp/server.go"))
	if gotLocalURI != wantLocalURI {
		t.Fatalf("mapRemoteURIToLocalURI = %q, want %q", gotLocalURI, wantLocalURI)
	}

	// WorkspaceEdit URI translation (both changes and documentChanges)
	rawEdit := json.RawMessage(`{
		"changes": {"file:///srv/work/sample-repo/a.go": [{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"X"}]},
		"documentChanges": [{"textDocument":{"uri":"file:///srv/work/sample-repo/b.go"},"edits":[]}]
	}`)
	translated := string(translateWorkspaceEditToLocal(rawEdit, localRoot, remoteRoot))
	if strings.Contains(translated, "/srv/work/") {
		t.Fatalf("expected all remote URIs translated to local, got: %s", translated)
	}
	if !strings.Contains(translated, "a.go") || !strings.Contains(translated, "b.go") {
		t.Fatalf("missing translated file paths in %s", translated)
	}

	// Opt-in safety: unconfigured remote LSP returns nil immediately without network probes.
	t.Setenv("SERENA_REMOTE_LSP", "auto")
	t.Setenv("SERENA_REMOTE_LSP_HOST", "")
	t.Setenv("SERENA_REMOTE_LSP_PROBE_ADDR", "")
	t.Setenv("SERENA_REMOTE_LSP_ROOT_PREFIX", "")
	if tgt := resolveRemoteLSPTarget(localRoot, "go"); tgt != nil {
		t.Fatalf("expected nil target when remote host unconfigured, got %+v", tgt)
	}

	// Fast offline failover (<350ms when configured remote probe port refuses/unreachable)
	t.Setenv("SERENA_REMOTE_LSP_HOST", "build-box")
	t.Setenv("SERENA_REMOTE_LSP_PROBE_ADDR", "127.0.0.1:1")
	t.Setenv("SERENA_REMOTE_LSP_ROOT_PREFIX", "/srv/work")
	t0 := time.Now()
	tgt := resolveRemoteLSPTarget(localRoot, "go")
	dt := time.Since(t0)
	if tgt != nil {
		t.Fatalf("expected nil target when probe fails, got %+v", tgt)
	}
	if dt > 400*time.Millisecond {
		t.Fatalf("offline failover too slow: %s", dt)
	}
}

func TestShellSingleQuoteAndCommandSafety(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/srv/work/repo", "'/srv/work/repo'"},
		{"/srv/work/my repo", "'/srv/work/my repo'"},
		{"/srv/work/$(touch /tmp/pwn)", "'/srv/work/$(touch /tmp/pwn)'"},
		{"/srv/work/`id`", "'/srv/work/`id`'"},
		{"/srv/work/o'neil", `'/srv/work/o'\''neil'`},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Fatalf("shellSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	spec := buildRemoteLSPSpec("go", &remoteLSPTarget{
		HostAlias:  "build-box",
		RemoteRoot: "/srv/work/$(id)'evil",
		RemoteCmd:  "exec gopls serve",
	})
	shellCmd := spec.Cmd[len(spec.Cmd)-1]
	if !strings.Contains(shellCmd, `cd '/srv/work/$(id)'\''evil'`) {
		t.Fatalf("remote shell command did not single-quote remoteRoot safely: %s", shellCmd)
	}
}

func TestConfigFileAndEnvPrecedence(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.json")
	raw := `{
		"remote_lsp": {
			"mode": "auto",
			"host": "cfg-host",
			"probe_addr": "192.0.2.10:22",
			"root_prefix": "/srv/cfg"
		},
		"embeddings": {
			"url": "http://127.0.0.1:19999/v1/embeddings",
			"model": "cfg-model"
		}
	}`
	if err := os.WriteFile(cfgPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERENA_CONFIG", cfgPath)
	t.Setenv("SERENA_REMOTE_LSP", "")
	t.Setenv("SERENA_REMOTE_LSP_HOST", "")
	t.Setenv("SERENA_REMOTE_LSP_PROBE_ADDR", "")
	t.Setenv("SERENA_REMOTE_LSP_ROOT_PREFIX", "")

	if got := remoteSSHHost(); got != "cfg-host" {
		t.Fatalf("remoteSSHHost from config = %q, want cfg-host", got)
	}
	if got := remoteProbeAddr(); got != "192.0.2.10:22" {
		t.Fatalf("remoteProbeAddr from config = %q, want 192.0.2.10:22", got)
	}
	if got := remoteRootPrefix(); got != "/srv/cfg" {
		t.Fatalf("remoteRootPrefix from config = %q, want /srv/cfg", got)
	}

	// Env var overrides config file.
	t.Setenv("SERENA_REMOTE_LSP_HOST", "env-host")
	if got := remoteSSHHost(); got != "env-host" {
		t.Fatalf("remoteSSHHost env override = %q, want env-host", got)
	}
}

func TestPathTraversalAndSymlinkContainment(t *testing.T) {
	outer := t.TempDir()
	repo := filepath.Join(outer, "repo")
	outside := filepath.Join(outer, "outside")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top-secret"), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "ok.txt"), []byte("ok"), 0o644)
	if err := os.Symlink(outside, filepath.Join(repo, "escape-link")); err != nil {
		t.Fatal(err)
	}

	// Valid path succeeds
	if _, err := resolveRepoPath(repo, "ok.txt"); err != nil {
		t.Fatalf("expected valid path inside repo to succeed: %v", err)
	}
	// Classic ../ traversal blocked
	if _, err := resolveRepoPath(repo, "../outside/secret.txt"); err == nil {
		t.Fatalf("expected ../ traversal to be rejected")
	}
	// Direct symlink pointing outside repo blocked
	if _, err := resolveRepoPath(repo, "escape-link/secret.txt"); err == nil {
		t.Fatalf("expected symlink escape to be rejected")
	}
	// Deep non-existent nested path under symlink pointing outside repo blocked
	if _, err := resolveRepoPath(repo, "escape-link/nonexistent/deep/new.txt"); err == nil {
		t.Fatalf("expected nested non-existent symlink escape to be rejected")
	}
	// Memory file traversal blocked
	if _, err := memoryFilePath(repo, "../../escape"); err == nil {
		t.Fatalf("expected memoryFilePath traversal to be rejected")
	}
}

func TestParseLinuxMeminfoPressure(t *testing.T) {
	normal := "MemTotal:       16384000 kB\nMemAvailable:    8192000 kB\n"
	warn := "MemTotal:       16384000 kB\nMemAvailable:    2000000 kB\n"
	critical := "MemTotal:       16384000 kB\nMemAvailable:     400000 kB\n"

	if got := parseLinuxMeminfoPressure(normal); got != 1 {
		t.Fatalf("normal pressure = %d, want 1", got)
	}
	if got := parseLinuxMeminfoPressure(warn); got != 2 {
		t.Fatalf("warn pressure = %d, want 2", got)
	}
	if got := parseLinuxMeminfoPressure(critical); got != 4 {
		t.Fatalf("critical pressure = %d, want 4", got)
	}
}

func TestSemanticCodeSearchASTBM25AndMockGPURRF(t *testing.T) {
	repo := t.TempDir()
	idxDir := t.TempDir()
	t.Setenv("SERENA_INDEX_DIR", idxDir)
	t.Setenv("SERENA_CONFIG", "none")

	_ = os.WriteFile(filepath.Join(repo, "webhook.ts"), []byte(`
export async function handleCallbackDedup(eventId: string, attempt: number) {
  if (attempt > 3) throw new Error("backoff exhausted");
  return { deduped: true, eventId };
}
`), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "math.ts"), []byte(`
export function multiplyMatrix(a: number, b: number): number {
  return a * b;
}
`), 0o644)
	_ = os.WriteFile(filepath.Join(repo, "session.ts"), []byte(`
export function rotateJwtGrant(userId: string): string {
  return "jwt_" + userId;
}
`), 0o644)

	// 1. Offline mode (AST-BM25 + domain synonyms: "webhook retry" -> matches handleCallbackDedup!)
	t.Setenv("SERENA_EMBED_URL", "off")
	res, err := dispatchNativeTool(repo, "semantic_code_search", map[string]any{
		"query": "where is webhook retry backoff handled",
		"top_k": 3,
	})
	if err != nil {
		t.Fatalf("semantic_code_search offline: %v", err)
	}
	if !strings.Contains(res, "AST-BM25") || !strings.Contains(res, "handleCallbackDedup") {
		t.Fatalf("expected handleCallbackDedup in offline AST-BM25 results:\n%s", res)
	}

	// 2. Hybrid BM25 + GPU Vector RRF via mock OpenAI /v1/embeddings server
	embedCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalls++
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i, txt := range req.Input {
			low := strings.ToLower(txt)
			vec := []float32{0.1, 0.1, 0.1}
			if strings.Contains(low, "jwt") || strings.Contains(low, "login") || strings.Contains(low, "auth") {
				vec = []float32{0.99, 0.05, 0.01}
			} else if strings.Contains(low, "callback") || strings.Contains(low, "webhook") {
				vec = []float32{0.05, 0.99, 0.01}
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "mock-nomic-gpu",
			"data":  data,
		})
	}))
	defer srv.Close()

	t.Setenv("SERENA_EMBED_URL", srv.URL)
	res2, err := dispatchNativeTool(repo, "semantic_code_search", map[string]any{
		"query": "user login authentication session refresh",
		"top_k": 3,
	})
	if err != nil {
		t.Fatalf("semantic_code_search hybrid GPU: %v", err)
	}
	if !strings.Contains(res2, "Hybrid BM25 + GPU Vector") || !strings.Contains(res2, "rotateJwtGrant") {
		t.Fatalf("expected Hybrid GPU Vector result with rotateJwtGrant:\n%s", res2)
	}
	if embedCalls != 1 {
		t.Fatalf("expected 1 batched embedding call, got %d", embedCalls)
	}

	// 3. Verify fetchEmbeddings splits large input lists into safe GPU sub-batches (<= maxEmbedBatch)
	var batchCalls atomic.Int32
	srvBatch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		batchCalls.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Input) > maxEmbedBatch {
			http.Error(w, "batch too large", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{float32(i + 1), 0.5}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "nomic-embed-text", "data": data})
	}))
	defer srvBatch.Close()

	inputs := make([]string, 70)
	for i := range inputs {
		inputs[i] = "search_document: chunk"
	}
	embs, model, err := fetchEmbeddings(srvBatch.URL, inputs)
	if err != nil {
		t.Fatalf("fetchEmbeddings sub-batch failed: %v", err)
	}
	if len(embs) != 70 || model != "nomic-embed-text" {
		t.Fatalf("unexpected sub-batch result: len=%d model=%q", len(embs), model)
	}
	if got := batchCalls.Load(); got != 3 {
		t.Fatalf("expected 3 sub-batches for 70 inputs (32+32+6), got %d", got)
	}
}

func TestHomeIndexPrivacyGuard(t *testing.T) {
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		if !isForbiddenIndexRoot(home) {
			t.Fatalf("expected $HOME %q to be blocked from indexing", home)
		}
	}
	if !isForbiddenIndexRoot("/") {
		t.Fatalf("expected / to be blocked from indexing")
	}
	repo := t.TempDir()
	if isForbiddenIndexRoot(repo) {
		t.Fatalf("expected temp repo dir %q to be allowed", repo)
	}
}

func TestThaiTokenizerUTF8AndSQ8Mmap(t *testing.T) {
	// 1. UTF-8 safe truncation never creates replacement runes (\ufffd)
	thai := "ระบบยืนยันตัวตนและโทเค็นสำหรับผู้ใช้งานทั้งหมดในโปรเจกต์"
	cut := truncateUTF8(thai, 17)
	if strings.ContainsRune(cut, '\uFFFD') {
		t.Fatalf("truncateUTF8 produced invalid UTF-8: %q", cut)
	}

	// 2. Thai tokenizer preserves unicode.Mn combining marks and produces 3-4 grapheme n-grams
	toks := tokenizeCode("ระบบยืนยันตัวตน authToken")
	joined := strings.Join(toks, " ")
	if !strings.Contains(joined, "ยืน") || !strings.Contains(joined, "ตัวตน") || !strings.Contains(joined, "auth") || !strings.Contains(joined, "token") {
		t.Fatalf("unexpected Thai+Code tokens: %v", toks)
	}

	// 3. SQ8 quantization preserves >0.995 cosine similarity with float32
	v1 := []float32{0.82, -0.31, 0.44, 0.19, -0.77, 0.55, 0.12, -0.64}
	v2 := []float32{0.79, -0.28, 0.49, 0.21, -0.73, 0.50, 0.10, -0.60}
	wantCos := cosineSim(v1, v2)
	q1, s1 := quantizeUnitSQ8(v1)
	deq1 := make([]float32, len(q1))
	for i, b := range q1 {
		deq1[i] = float32(b) * s1
	}
	gotCos := cosineSim(deq1, v2)
	diff := wantCos - gotCos
	if diff < 0 {
		diff = -diff
	}
	if diff > 0.01 {
		t.Fatalf("SQ8 cosine drift too large: want %.4f got %.4f", wantCos, gotCos)
	}
}

func TestASTFixesExactMutationSafetyAndUTF16(t *testing.T) {
	repo := t.TempDir()
	goSrc := `package demo

const Version = "1.0.0"

var GlobalFlag = true

type Map[K comparable, V any] struct {
	Items map[K]V
}

func (m *Map[K, V]) Lookup(k K) (V, bool) {
	v, ok := m.Items[k]
	return v, ok
}
`
	goPath := filepath.Join(repo, "map.go")
	_ = os.WriteFile(goPath, []byte(goSrc), 0o644)
	syms, _, err := extractFileSymbols(goPath, "map.go", []byte(goSrc))
	if err != nil {
		t.Fatalf("extractFileSymbols go: %v", err)
	}
	flat := flattenSymbols(syms)
	foundLookup, foundVar, foundField := false, false, false
	for _, s := range flat {
		if s.NamePath == "Map/Lookup" {
			foundLookup = true
		}
		if s.Name == "GlobalFlag" && s.Kind == "var" {
			foundVar = true
		}
		if s.Name == "Items" && s.Kind == "field" {
			foundField = true
		}
		if s.Name == "Version" && !strings.HasPrefix(string([]byte(goSrc)[s.StartByte:s.EndByte]), "const ") {
			t.Fatalf("const Version span missing leading 'const ': %q", string([]byte(goSrc)[s.StartByte:s.EndByte]))
		}
	}
	if !foundLookup || !foundVar || !foundField {
		t.Fatalf("missing Go AST symbols: lookup=%v var=%v field=%v; flat=%+v", foundLookup, foundVar, foundField, flat)
	}

	// Strict mutation safety: fuzzy substring "Look" must NOT silently match "Map/Lookup"!
	if _, _, _, _, err := resolveSingleSymbolInFile(repo, "map.go", "Look"); err == nil || !strings.Contains(err.Error(), "Map/Lookup") {
		t.Fatalf("expected exact-match failure with closest hint, got err=%v", err)
	}

	// UTF-16 <-> UTF-8 offset accuracy on Thai + emoji line
	line := []byte(`const msg = "สวัสดี🚀"; const target = 1;`)
	idxTarget := bytes.Index(line, []byte("target"))
	u16 := byteColToLSPCol(line, idxTarget, "utf-16")
	back := lspColToByteCol(line, u16, "utf-16")
	if back != idxTarget {
		t.Fatalf("UTF-16 round-trip mismatch: byte=%d -> utf16=%d -> byte=%d", idxTarget, u16, back)
	}

	// Slim profile reduces tool count significantly vs full profile
	fullTools := nativeToolCatalog("full")
	slimTools := nativeToolCatalog("slim")
	if len(slimTools) >= len(fullTools) || len(slimTools) < 10 {
		t.Fatalf("unexpected tool profile counts: full=%d slim=%d", len(fullTools), len(slimTools))
	}

	// Hook apply_patch path extraction
	patchPayload := []byte(`{"cwd":"/repo","tool_input":{"patch":"*** Begin Patch\n*** Update File: src/auth.ts\n@@\n*** Add File: src/new.go\n*** End Patch"}}`)
	gotPaths := extractPathsFromHookPayload(patchPayload, "/repo")
	if len(gotPaths) != 2 || gotPaths[0] != "/repo/src/auth.ts" || gotPaths[1] != "/repo/src/new.go" {
		t.Fatalf("unexpected apply_patch paths: %v", gotPaths)
	}
}
