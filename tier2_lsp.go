package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type lspLangSpec struct {
	Lang        string
	Cmd         []string
	Env         []string
	InitOptions map[string]any
}

func lspStateDir() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_LSP_STATE_DIR")); v != "" {
		return v
	}
	return fmt.Sprintf("/tmp/serena-lsp-%d", os.Getuid())
}

func detectLSPLang(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		return "ts"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".swift":
		return "swift"
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".m", ".mm":
		return "cpp"
	default:
		return ""
	}
}

func lspLanguageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".mts", ".cts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".go":
		return "go"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".swift":
		return "swift"
	case ".c":
		return "c"
	case ".cc", ".cpp", ".cxx", ".hpp":
		return "cpp"
	case ".m":
		return "objective-c"
	case ".mm":
		return "objective-cpp"
	default:
		return "plaintext"
	}
}

func resolveLSPSpec(lang string) (*lspLangSpec, error) {
	home, _ := os.UserHomeDir()
	// Allow test override for any language: SERENA_LSP_CMD_<LANG>="cmd arg1 ..."
	if envCmd := strings.TrimSpace(os.Getenv("SERENA_LSP_CMD_" + strings.ToUpper(lang))); envCmd != "" {
		parts := strings.Fields(envCmd)
		if len(parts) > 0 {
			return &lspLangSpec{Lang: lang, Cmd: parts}, nil
		}
	}

	switch lang {
	case "ts":
		if p := findToolBin("vtsls",
			filepath.Join(home, ".bun/bin/vtsls"),
			filepath.Join(home, ".local/bin/vtsls"),
		); p != "" {
			return &lspLangSpec{
				Lang: "ts",
				Cmd:  []string{p, "--stdio"},
				Env:  []string{"NODE_OPTIONS=--max-old-space-size=768"},
				InitOptions: map[string]any{
					"hostInfo": "serena-go",
					"typescript": map[string]any{
						"tsserver": map[string]any{
							"maxTsServerMemory": 768,
						},
					},
					"vtsls": map[string]any{
						"autoUseWorkspaceTsdk": true,
					},
				},
			}, nil
		}
		if p := findToolBin("typescript-language-server",
			filepath.Join(home, ".serena/language_servers/static/TypeScriptLanguageServer/ts-lsp/node_modules/.bin/typescript-language-server"),
		); p != "" {
			return &lspLangSpec{
				Lang: "ts",
				Cmd:  []string{p, "--stdio"},
				Env:  []string{"NODE_OPTIONS=--max-old-space-size=768"},
				InitOptions: map[string]any{
					"maxTsServerMemory": 768,
					"preferences": map[string]any{
						"disableSuggestions": false,
					},
				},
			}, nil
		}
		return nil, errors.New("no TypeScript LSP found (install @vtsls/language-server)")

	case "go":
		p := findToolBin("gopls",
			filepath.Join(home, ".local/bin/gopls"),
			filepath.Join(home, "go/bin/gopls"),
		)
		if p == "" {
			return nil, errors.New("gopls not found")
		}
		return &lspLangSpec{
			Lang: "go",
			Cmd:  []string{p, "serve"},
			InitOptions: map[string]any{
				"memoryMode":       "DegradeClosed",
				"staticcheck":      false,
				"directoryFilters": []string{"-node_modules", "-.git", "-dist", "-build", "-.next", "-graft", "-Pods"},
			},
		}, nil

	case "rust":
		p := findToolBin("rust-analyzer",
			filepath.Join(home, ".cargo/bin/rust-analyzer"),
		)
		if p == "" {
			return nil, errors.New("rust-analyzer not found")
		}
		return &lspLangSpec{
			Lang: "rust",
			Cmd:  []string{p},
			InitOptions: map[string]any{
				"lru": map[string]any{"capacity": 64},
				"checkOnSave": map[string]any{
					"enable": false,
				},
				"cargo": map[string]any{
					"buildScripts": map[string]any{"enable": true},
				},
			},
		}, nil

	case "python":
		if p := findToolBin("ty", filepath.Join(home, ".local/bin/ty")); p != "" {
			return &lspLangSpec{
				Lang: "python",
				Cmd:  []string{p, "server"},
			}, nil
		}
		if p := findToolBin("basedpyright-langserver", "pyright-langserver"); p != "" {
			return &lspLangSpec{
				Lang: "python",
				Cmd:  []string{p, "--stdio"},
				Env:  []string{"NODE_OPTIONS=--max-old-space-size=768"},
			}, nil
		}
		return nil, errors.New("no Python LSP found (install ty or basedpyright)")

	case "swift":
		p := findToolBin("sourcekit-lsp", "/usr/bin/sourcekit-lsp")
		if p == "" {
			return nil, errors.New("sourcekit-lsp not found")
		}
		return &lspLangSpec{Lang: "swift", Cmd: []string{p}}, nil

	case "cpp":
		p := findToolBin("clangd", "/usr/bin/clangd")
		if p == "" {
			return nil, errors.New("clangd not found")
		}
		return &lspLangSpec{
			Lang: "cpp",
			Cmd:  []string{p, "--malloc-trim", "--pch-storage=disk", "--limit-results=100", "-j=2"},
		}, nil
	}
	return nil, fmt.Errorf("unsupported LSP language %q", lang)
}

func lspDaemonKey(repoRoot, lang string) string {
	sum := sha256.Sum256([]byte(canonicalPath(repoRoot) + "\n" + lang))
	return hex.EncodeToString(sum[:])[:16]
}

type lspDaemonMeta struct {
	Key          string `json:"key"`
	PID          int    `json:"pid"`
	ChildPID     int    `json:"child_pid"`
	PGID         int    `json:"pgid"`
	SockPath     string `json:"sock_path"`
	RepoRoot     string `json:"repo_root"`
	Lang         string `json:"lang"`
	Remote       bool   `json:"remote,omitempty"`
	RemoteHost   string `json:"remote_host,omitempty"`
	RemoteRoot   string `json:"remote_root,omitempty"`
	LastActivity int64  `json:"last_activity"`
}

func readLSPMeta(path string) (*lspDaemonMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m lspDaemonMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func fileToURI(absPath string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	return u.String()
}

func uriToFile(rawURI string) string {
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme != "file" {
		return rawURI
	}
	return filepath.FromSlash(u.Path)
}

// --- LSP Framing Client (inside __lsp_supervise daemon) ---

type lspProcessClient struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	pgid       int
	done       chan struct{}
	root       string
	remoteRoot string
	remoteHost string
	lang       string

	writeMu   sync.Mutex
	nextID    int64
	pendingMu sync.Mutex
	pending   map[int64]chan json.RawMessage

	openMu   sync.Mutex
	openVers map[string]int

	diagMu   sync.Mutex
	diags    map[string][]lspDiagnostic
	diagCond map[string][]chan struct{}
}

type lspDiagnostic struct {
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
		End struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"end"`
	} `json:"range"`
	Severity int    `json:"severity"` // 1=Error, 2=Warning, 3=Info, 4=Hint
	Source   string `json:"source"`
	Message  string `json:"message"`
}

type lspLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
		End struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"end"`
	} `json:"range"`
}

func startLSPProcess(repoRoot, remoteRoot, remoteHost string, spec *lspLangSpec, logFile io.Writer) (*lspProcessClient, error) {
	cmd := exec.Command(spec.Cmd[0], spec.Cmd[1:]...)
	if remoteRoot == "" {
		cmd.Dir = repoRoot
	}
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdinPipe.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdinPipe.Close()
		_ = stdoutPipe.Close()
		return nil, err
	}
	pgid := cmd.Process.Pid
	if p, err := syscall.Getpgid(cmd.Process.Pid); err == nil && p > 0 {
		pgid = p
	}
	c := &lspProcessClient{
		cmd:        cmd,
		stdin:      stdinPipe,
		pgid:       pgid,
		done:       make(chan struct{}),
		root:       repoRoot,
		remoteRoot: remoteRoot,
		remoteHost: remoteHost,
		lang:       spec.Lang,
		pending:    map[int64]chan json.RawMessage{},
		openVers:   map[string]int{},
		diags:      map[string][]lspDiagnostic{},
		diagCond:   map[string][]chan struct{}{},
	}
	go c.readLoop(stdoutPipe)
	if err := c.initialize(spec); err != nil {
		c.shutdown()
		return nil, err
	}
	return c, nil
}

func (c *lspProcessClient) readLoop(r io.ReadCloser) {
	defer close(c.done)
	defer r.Close()
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		contentLen := -1
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				c.failAllPending()
				_ = c.cmd.Wait()
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(line[len("content-length:"):])
				if n, err := strconv.Atoi(v); err == nil {
					contentLen = n
				}
			}
		}
		if contentLen <= 0 || contentLen > 64*1024*1024 {
			continue
		}
		body := make([]byte, contentLen)
		if _, err := io.ReadFull(br, body); err != nil {
			c.failAllPending()
			_ = c.cmd.Wait()
			return
		}
		c.handleMessage(body)
	}
}

func (c *lspProcessClient) failAllPending() {
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()
}

func (c *lspProcessClient) handleMessage(body []byte) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return
	}
	// Response to our request
	if msg.Method == "" && len(bytes.TrimSpace(msg.ID)) > 0 {
		var id int64
		if json.Unmarshal(msg.ID, &id) == nil {
			c.pendingMu.Lock()
			ch, ok := c.pending[id]
			if ok {
				delete(c.pending, id)
			}
			c.pendingMu.Unlock()
			if ok {
				ch <- body
			}
		}
		return
	}
	// Server-initiated notification: publishDiagnostics
	if msg.Method == "textDocument/publishDiagnostics" {
		var p struct {
			URI         string          `json:"uri"`
			Diagnostics []lspDiagnostic `json:"diagnostics"`
		}
		if json.Unmarshal(msg.Params, &p) == nil && p.URI != "" {
			c.diagMu.Lock()
			c.diags[p.URI] = p.Diagnostics
			waiters := c.diagCond[p.URI]
			delete(c.diagCond, p.URI)
			c.diagMu.Unlock()
			for _, w := range waiters {
				close(w)
			}
		}
		return
	}
	// Server-initiated request (e.g. workspace/configuration, client/registerCapability): reply cleanly so server doesn't block!
	if msg.Method != "" && len(bytes.TrimSpace(msg.ID)) > 0 {
		var resp any = nil
		if msg.Method == "workspace/configuration" {
			var p struct {
				Items []json.RawMessage `json:"items"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			arr := make([]any, len(p.Items))
			for i := range arr {
				arr[i] = map[string]any{}
			}
			resp = arr
		}
		payload, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      json.RawMessage(bytes.TrimSpace(msg.ID)),
			"result":  resp,
		})
		_ = c.writeFrame(payload)
	}
}

func (c *lspProcessClient) writeFrame(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errors.New("lsp stdin closed")
	}
	hdr := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := io.WriteString(c.stdin, hdr); err != nil {
		return err
	}
	_, err := c.stdin.Write(payload)
	return err
}

func (c *lspProcessClient) notify(method string, params any) error {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return err
	}
	return c.writeFrame(b)
}

func (c *lspProcessClient) call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	c.pendingMu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.pendingMu.Unlock()

	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(b); err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case raw, ok := <-ch:
		if !ok {
			return nil, errors.New("lsp exited before responding")
		}
		var env struct {
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		if env.Error != nil {
			return nil, fmt.Errorf("lsp error %d: %s", env.Error.Code, env.Error.Message)
		}
		return env.Result, nil
	case <-c.done:
		return nil, errors.New("lsp exited")
	case <-timer.C:
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("lsp %s timed out after %s", method, timeout)
	}
}

func (c *lspProcessClient) initialize(spec *lspLangSpec) error {
	lspRoot := c.root
	if c.remoteRoot != "" {
		lspRoot = c.remoteRoot
	}
	rootURI := fileToURI(lspRoot)
	params := map[string]any{
		"processId": nil,
		"rootUri":   rootURI,
		"rootPath":  lspRoot,
		"workspaceFolders": []map[string]any{
			{"uri": rootURI, "name": filepath.Base(lspRoot)},
		},
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": true},
				"references":      map[string]any{"dynamicRegistration": false},
				"definition":      map[string]any{"dynamicRegistration": false},
				"implementation":  map[string]any{"dynamicRegistration": false},
				"rename":          map[string]any{"prepareSupport": false},
				"hover":           map[string]any{"contentFormat": []string{"plaintext", "markdown"}},
				"diagnostic":      map[string]any{"dynamicRegistration": false},
				"publishDiagnostics": map[string]any{
					"relatedInformation": false,
				},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"applyEdit":        false,
			},
		},
	}
	if len(spec.InitOptions) > 0 {
		params["initializationOptions"] = spec.InitOptions
	}
	if _, err := c.call("initialize", params, 25*time.Second); err != nil {
		return err
	}
	return c.notify("initialized", map[string]any{})
}

func (c *lspProcessClient) syncFile(absPath string) (string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	targetPath := mapLocalPathToRemote(absPath, c.root, c.remoteRoot)
	uri := fileToURI(targetPath)
	c.openMu.Lock()
	ver, opened := c.openVers[uri]
	ver++
	c.openVers[uri] = ver
	c.openMu.Unlock()

	if !opened {
		err = c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": lspLanguageID(absPath),
				"version":    ver,
				"text":       string(data),
			},
		})
	} else {
		err = c.notify("textDocument/didChange", map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": ver},
			"contentChanges": []map[string]any{
				{"text": string(data)},
			},
		})
	}
	return uri, err
}

func (c *lspProcessClient) shutdown() {
	_, _ = c.call("shutdown", nil, 1500*time.Millisecond)
	_ = c.notify("exit", nil)
	c.writeMu.Lock()
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	c.writeMu.Unlock()
	select {
	case <-c.done:
		return
	case <-time.After(1 * time.Second):
	}
	killProcessGroup(c.pgid)
}

// --- Unix Socket IPC Protocol for Shared Per-(Repo, Lang) LSP Daemon ---

type lspIPCReq struct {
	Action      string `json:"action"` // references|definition|implementation|rename|diagnostics|hover|ping|shutdown
	File        string `json:"file,omitempty"`
	Line        int    `json:"line,omitempty"` // 0-based
	Col         int    `json:"col,omitempty"`  // 0-based
	NewName     string `json:"new_name,omitempty"`
	IncludeDecl bool   `json:"include_decl,omitempty"`
	Fast        bool   `json:"fast,omitempty"`
}

type lspIPCResp struct {
	OK          bool            `json:"ok"`
	Error       string          `json:"error,omitempty"`
	Locations   []lspLocation   `json:"locations,omitempty"`
	Diagnostics []lspDiagnostic `json:"diagnostics,omitempty"`
	Edit        json.RawMessage `json:"edit,omitempty"`
	Hover       string          `json:"hover,omitempty"`
	Host        string          `json:"host,omitempty"`
}

func parseLocations(raw json.RawMessage) []lspLocation {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var many []lspLocation
	if json.Unmarshal(trimmed, &many) == nil {
		return many
	}
	// Handle LocationLink[] (targetUri + targetSelectionRange)
	var links []struct {
		TargetURI string `json:"targetUri"`
		TargetSel struct {
			Start struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"start"`
			End struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"end"`
		} `json:"targetSelectionRange"`
	}
	if json.Unmarshal(trimmed, &links) == nil && len(links) > 0 && links[0].TargetURI != "" {
		out := make([]lspLocation, 0, len(links))
		for _, lk := range links {
			var loc lspLocation
			loc.URI = lk.TargetURI
			loc.Range = lk.TargetSel
			out = append(out, loc)
		}
		return out
	}
	var single lspLocation
	if json.Unmarshal(trimmed, &single) == nil && single.URI != "" {
		return []lspLocation{single}
	}
	return nil
}

func runLSPSupervisor(args []string) int {
	// Format: __lsp_supervise <key> <sockPath> <metaPath> <repoRoot> <lang> [remoteHost remoteRoot]
	if len(args) < 5 {
		fmt.Fprintln(os.Stderr, "serena __lsp_supervise: insufficient args")
		return 2
	}
	key, sockPath, metaPath, repoRoot, lang := args[0], args[1], args[2], args[3], args[4]
	remoteHost, remoteRoot := "", ""
	if len(args) >= 7 {
		remoteHost, remoteRoot = args[5], args[6]
	}
	stateDir := filepath.Dir(metaPath)
	_ = os.MkdirAll(stateDir, 0o700)
	logPath := filepath.Join(stateDir, key+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 1
	}
	defer logFile.Close()

	var spec *lspLangSpec
	if remoteHost != "" && remoteRoot != "" {
		_, rcmd, ok := remoteLSPCommandForLang(lang)
		if !ok {
			fmt.Fprintf(logFile, "remote LSP unsupported for %s\n", lang)
			return 1
		}
		spec = buildRemoteLSPSpec(lang, &remoteLSPTarget{HostAlias: remoteHost, RemoteRoot: remoteRoot, RemoteCmd: rcmd})
	} else {
		spec, err = resolveLSPSpec(lang)
		if err != nil {
			fmt.Fprintf(logFile, "resolveLSPSpec(%s): %v\n", lang, err)
			return 1
		}
	}
	client, err := startLSPProcess(repoRoot, remoteRoot, remoteHost, spec, logFile)
	if err != nil {
		fmt.Fprintf(logFile, "startLSPProcess(%s remote=%s): %v\n", lang, remoteHost, err)
		return 1
	}

	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		client.shutdown()
		return 1
	}
	_ = os.Chmod(sockPath, 0o600)

	hostLabel := "local"
	if remoteHost != "" {
		hostLabel = remoteHost
	}

	var actMu sync.Mutex
	lastAct := time.Now()
	touchAct := func() {
		actMu.Lock()
		lastAct = time.Now()
		actMu.Unlock()
		meta := lspDaemonMeta{
			Key:          key,
			PID:          os.Getpid(),
			ChildPID:     client.cmd.Process.Pid,
			PGID:         client.pgid,
			SockPath:     sockPath,
			RepoRoot:     repoRoot,
			Lang:         lang,
			Remote:       remoteHost != "",
			RemoteHost:   remoteHost,
			RemoteRoot:   remoteRoot,
			LastActivity: time.Now().Unix(),
		}
		if b, err := json.Marshal(meta); err == nil {
			_ = os.WriteFile(metaPath, b, 0o600)
		}
	}
	touchAct()

	stopCh := make(chan struct{})
	var stopOnce sync.Once
	triggerStop := func() {
		stopOnce.Do(func() {
			close(stopCh)
			_ = ln.Close()
		})
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-client.done:
				triggerStop()
				return
			case <-sigCh:
				triggerStop()
				return
			case <-ticker.C:
				maxD, idleLim := adaptiveLSPPolicy()
				// Remote LSPs use 0 MB LSP RAM on the local host, so don't evict them on local OS memory pressure.
				if remoteHost == "" && maxD == 0 {
					fmt.Fprintf(logFile, "[lsp-supervisor] critical memory pressure; evicting %s/%s\n", repoRoot, lang)
					triggerStop()
					return
				}
				if remoteHost != "" && idleLim < 300*time.Second {
					idleLim = 300 * time.Second
				}
				actMu.Lock()
				idleFor := time.Since(lastAct)
				actMu.Unlock()
				if idleFor >= idleLim {
					fmt.Fprintf(logFile, "[lsp-supervisor] idle %s >= %s; reaping %s/%s (%s)\n", idleFor.Round(time.Second), idleLim, repoRoot, lang, hostLabel)
					triggerStop()
					return
				}
			}
		}
	}()

	handleConn := func(conn net.Conn) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(35 * time.Second))
		var req lspIPCReq
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		touchAct()
		resp := lspIPCResp{OK: true, Host: hostLabel}
		switch req.Action {
		case "ping":
		case "shutdown":
			_ = json.NewEncoder(conn).Encode(resp)
			triggerStop()
			return
		case "references", "definition", "implementation":
			uri, err := client.syncFile(req.File)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
				break
			}
			pos := map[string]any{"line": req.Line, "character": req.Col}
			params := map[string]any{
				"textDocument": map[string]any{"uri": uri},
				"position":     pos,
			}
			method := "textDocument/" + req.Action
			if req.Action == "references" {
				params["context"] = map[string]any{"includeDeclaration": req.IncludeDecl}
			}
			raw, err := client.call(method, params, 20*time.Second)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
			} else {
				resp.Locations = translateLocationsToLocal(parseLocations(raw), repoRoot, remoteRoot)
			}
		case "rename":
			uri, err := client.syncFile(req.File)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
				break
			}
			params := map[string]any{
				"textDocument": map[string]any{"uri": uri},
				"position":     map[string]any{"line": req.Line, "character": req.Col},
				"newName":      req.NewName,
			}
			raw, err := client.call("textDocument/rename", params, 20*time.Second)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
			} else {
				resp.Edit = translateWorkspaceEditToLocal(raw, repoRoot, remoteRoot)
			}
		case "hover":
			uri, err := client.syncFile(req.File)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
				break
			}
			raw, err := client.call("textDocument/hover", map[string]any{
				"textDocument": map[string]any{"uri": uri},
				"position":     map[string]any{"line": req.Line, "character": req.Col},
			}, 10*time.Second)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
			} else {
				resp.Hover = extractHoverText(raw)
			}
		case "diagnostics":
			uri, err := client.syncFile(req.File)
			if err != nil {
				resp = lspIPCResp{OK: false, Error: err.Error(), Host: hostLabel}
				break
			}
			if raw, perr := client.call("textDocument/diagnostic", map[string]any{
				"textDocument": map[string]any{"uri": uri},
			}, 2500*time.Millisecond); perr == nil {
				var pull struct {
					Items []lspDiagnostic `json:"items"`
				}
				if json.Unmarshal(raw, &pull) == nil {
					resp.Diagnostics = pull.Items
					break
				}
			}
			waitCh := make(chan struct{})
			client.diagMu.Lock()
			existing, hasExisting := client.diags[uri]
			if !hasExisting {
				client.diagCond[uri] = append(client.diagCond[uri], waitCh)
			}
			client.diagMu.Unlock()
			if !hasExisting {
				waitDur := 1200 * time.Millisecond
				if req.Fast {
					waitDur = 250 * time.Millisecond
				}
				select {
				case <-waitCh:
				case <-time.After(waitDur):
				}
			}
			client.diagMu.Lock()
			existing = client.diags[uri]
			client.diagMu.Unlock()
			resp.Diagnostics = existing
		default:
			resp = lspIPCResp{OK: false, Error: "unknown action " + req.Action, Host: hostLabel}
		}
		_ = json.NewEncoder(conn).Encode(resp)
	}

	for {
		conn, err := ln.Accept()
		if err != nil {
			break
		}
		go handleConn(conn)
	}

	_ = os.Remove(sockPath)
	_ = os.Remove(metaPath)
	client.shutdown()
	return 0
}

func extractHoverText(raw json.RawMessage) string {
	var h struct {
		Contents json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(raw, &h) != nil || len(h.Contents) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(h.Contents, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(h.Contents, &obj) == nil && obj.Value != "" {
		return strings.TrimSpace(obj.Value)
	}
	return strings.TrimSpace(string(h.Contents))
}

// --- Client-Side Global LRU & Daemon Invocation ---

func evictExcessLSPDaemonsLocked(stateDir string, keepMax int, exceptKey string, remoteOnly bool) {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return
	}
	var live []*lspDaemonMeta
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		fp := filepath.Join(stateDir, e.Name())
		m, err := readLSPMeta(fp)
		if err != nil {
			_ = os.Remove(fp)
			continue
		}
		if !pidAlive(m.PID) || !pidAlive(m.ChildPID) {
			_ = os.Remove(fp)
			_ = os.Remove(m.SockPath)
			continue
		}
		if m.Key == exceptKey || m.Remote != remoteOnly {
			continue
		}
		live = append(live, m)
	}
	sort.Slice(live, func(i, j int) bool {
		return live[i].LastActivity < live[j].LastActivity
	})
	for len(live) > keepMax {
		victim := live[0]
		live = live[1:]
		killProcessGroup(victim.PGID)
		if pidAlive(victim.PID) {
			_ = syscall.Kill(victim.PID, syscall.SIGTERM)
		}
		_ = os.Remove(victim.SockPath)
		_ = os.Remove(filepath.Join(stateDir, victim.Key+".json"))
	}
}

func dialLSPDaemon(sockPath string, req lspIPCReq, timeout time.Duration) (*lspIPCResp, error) {
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp lspIPCResp
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return &resp, nil
}

func lspSocketPath(stateDir, key string) string {
	cand := filepath.Join(stateDir, key+".sock")
	// macOS sockaddr_un.sun_path max is 104 bytes; fall back to short /tmp path if stateDir is deep.
	if len(cand) >= 96 {
		h := sha256.Sum256([]byte(stateDir + "\n" + key))
		return fmt.Sprintf("/tmp/slsp-%d-%s.sock", os.Getuid(), hex.EncodeToString(h[:])[:12])
	}
	return cand
}

func ensureAndQueryLSP(repoRoot, lang string, req lspIPCReq, warmOnly bool) (*lspIPCResp, error) {
	stateDir := lspStateDir()
	key := lspDaemonKey(repoRoot, lang)
	sockPath := lspSocketPath(stateDir, key)
	metaPath := filepath.Join(stateDir, key+".json")

	// Fast-path: already warm
	if resp, err := dialLSPDaemon(sockPath, req, 25*time.Second); err == nil {
		return resp, nil
	}
	if warmOnly {
		return nil, errors.New("lsp not warm")
	}

	// Prefer configured remote SSH LSP host (0 MB local LSP RAM) when reachable & repo is synced;
	// transparently fall back to local LRU-capped LSP in <250ms when offline or unconfigured.
	remoteTarget := resolveRemoteLSPTarget(repoRoot, lang)
	if remoteTarget == nil {
		maxDaemons, _ := adaptiveLSPPolicy()
		if maxDaemons <= 0 {
			return nil, errors.New("system memory pressure is CRITICAL and no remote LSP host is available; local LSP paused (using AST/rg fallback)")
		}
		if _, err := resolveLSPSpec(lang); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	lockFile, err := acquireLock(filepath.Join(stateDir, "_global.lock"))
	if err != nil {
		return nil, err
	}
	defer releaseLock(lockFile)

	// Re-check after lock
	if resp, err := dialLSPDaemon(sockPath, req, 25*time.Second); err == nil {
		return resp, nil
	}

	if remoteTarget != nil {
		evictExcessLSPDaemonsLocked(stateDir, 3, key, true)
	} else {
		maxDaemons, _ := adaptiveLSPPolicy()
		evictExcessLSPDaemonsLocked(stateDir, maxDaemons-1, key, false)
	}

	selfExe, err := os.Executable()
	if err != nil {
		selfExe = os.Args[0]
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devNull.Close()

	supArgs := []string{"__lsp_supervise", key, sockPath, metaPath, canonicalPath(repoRoot), lang}
	if remoteTarget != nil {
		supArgs = append(supArgs, remoteTarget.HostAlias, remoteTarget.RemoteRoot)
	}
	cmd := exec.Command(selfExe, supArgs...)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	supDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(supDone)
	}()

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := dialLSPDaemon(sockPath, lspIPCReq{Action: "ping"}, 1*time.Second); err == nil {
			return dialLSPDaemon(sockPath, req, 25*time.Second)
		}
		select {
		case <-supDone:
			return nil, fmt.Errorf("LSP supervisor for %s exited early (see %s/%s.log)", lang, stateDir, key)
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	killProcessGroup(cmd.Process.Pid)
	return nil, fmt.Errorf("timed out starting %s LSP", lang)
}

type formattedRef struct {
	RelPath string
	Line    int
	Col     int
	Preview string
}

func formatLocations(repoRoot string, locs []lspLocation) []formattedRef {
	seen := map[string]bool{}
	var out []formattedRef
	fileLines := map[string][]string{}
	for _, loc := range locs {
		abs := uriToFile(loc.URI)
		rel, err := filepath.Rel(repoRoot, abs)
		if err != nil {
			rel = abs
		}
		rel = filepath.ToSlash(rel)
		line := loc.Range.Start.Line + 1
		col := loc.Range.Start.Character
		k := fmt.Sprintf("%s:%d:%d", rel, line, col)
		if seen[k] {
			continue
		}
		seen[k] = true
		lines, ok := fileLines[abs]
		if !ok {
			if raw, rerr := os.ReadFile(abs); rerr == nil {
				lines = strings.Split(string(raw), "\n")
			}
			fileLines[abs] = lines
		}
		preview := ""
		if line-1 >= 0 && line-1 < len(lines) {
			preview = strings.TrimSpace(lines[line-1])
			if len(preview) > 110 {
				preview = preview[:107] + "..."
			}
		}
		out = append(out, formattedRef{RelPath: rel, Line: line, Col: col, Preview: preview})
	}
	return out
}

func queryWarmLSPDiagnostics(repoRoot, lang, absFile string) []string {
	resp, err := ensureAndQueryLSP(repoRoot, lang, lspIPCReq{
		Action: "diagnostics",
		File:   absFile,
		Fast:   true,
	}, true)
	if err != nil || len(resp.Diagnostics) == 0 {
		return nil
	}
	var out []string
	base := filepath.Base(absFile)
	for _, d := range resp.Diagnostics {
		if d.Severity > 2 { // Errors & warnings only for hook brevity
			continue
		}
		sev := "error"
		if d.Severity == 2 {
			sev = "warn"
		}
		out = append(out, fmt.Sprintf("[lsp:%s] %s:%d:%d (%s) %s", lang, base, d.Range.Start.Line+1, d.Range.Start.Character+1, sev, firstLineTrimmed(d.Message)))
		if len(out) >= 5 {
			break
		}
	}
	return out
}

func queryWarmLSPReferences(repoRoot, lang, absFile string, line0, col0 int) ([]formattedRef, bool) {
	resp, err := ensureAndQueryLSP(repoRoot, lang, lspIPCReq{
		Action:      "references",
		File:        absFile,
		Line:        line0,
		Col:         col0,
		IncludeDecl: false,
	}, true)
	if err != nil {
		return nil, false
	}
	return formatLocations(repoRoot, resp.Locations), true
}

// queryLSPWithFuzzing retries bounded identifier offsets if the initial LSP position returns empty.
func queryLSPWithFuzzing(repoRoot, lang, absFile string, src []byte, sym astSymbol, action string) (*lspIPCResp, error) {
	candidates := [][2]int{{sym.NameLine - 1, sym.NameCol}}
	lines := strings.Split(string(src), "\n")
	for delta := -1; delta <= 1; delta++ {
		ln := sym.NameLine - 1 + delta
		if ln < 0 || ln >= len(lines) {
			continue
		}
		if col := strings.Index(lines[ln], sym.Name); col >= 0 && (ln != sym.NameLine-1 || col != sym.NameCol) {
			candidates = append(candidates, [2]int{ln, col})
		}
	}
	var lastResp *lspIPCResp
	var lastErr error
	for _, pos := range candidates {
		resp, err := ensureAndQueryLSP(repoRoot, lang, lspIPCReq{
			Action:      action,
			File:        absFile,
			Line:        pos[0],
			Col:         pos[1],
			IncludeDecl: false,
		}, false)
		if err != nil {
			lastErr = err
			break
		}
		lastResp = resp
		if len(resp.Locations) > 0 {
			return resp, nil
		}
	}
	return lastResp, lastErr
}

func toolFindReferencingSymbols(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path")
	sym, absFile, src, _, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	lang := detectLSPLang(absFile)
	if lang != "" {
		resp, lerr := queryLSPWithFuzzing(repoRoot, lang, absFile, src, sym, "references")
		if lerr == nil && resp != nil {
			refs := formatLocations(repoRoot, resp.Locations)
			if len(refs) == 0 {
				return fmt.Sprintf("No references to %s (%s:L%d) found via %s LSP.", sym.NamePath, rel, sym.StartLine, lang), nil
			}
			hostTag := resp.Host
			if hostTag == "" {
				hostTag = "local"
			}
			var b strings.Builder
			fmt.Fprintf(&b, "References to %s (%d via %s LSP @%s):\n", sym.NamePath, len(refs), lang, hostTag)
			for _, r := range refs {
				fmt.Fprintf(&b, "- %s:L%d  %s\n", r.RelPath, r.Line, r.Preview)
			}
			return truncateAnswer(strings.TrimSpace(b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
		}
		// Fallback to ripgrep word search if LSP unavailable/pressure critical
		hits := fastCallersViaRG(repoRoot, sym, 30)
		var b strings.Builder
		fmt.Fprintf(&b, "References to %s (fallback rg; LSP note: %v):\n", sym.NamePath, lerr)
		if len(hits) == 0 {
			b.WriteString("No references found.")
		} else {
			for _, h := range hits {
				b.WriteString(h + "\n")
			}
		}
		return truncateAnswer(strings.TrimSpace(b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
	}
	hits := fastCallersViaRG(repoRoot, sym, 30)
	if len(hits) == 0 {
		return fmt.Sprintf("No references to %s found.", sym.NamePath), nil
	}
	return truncateAnswer(strings.Join(hits, "\n"), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

func toolFindDeclaration(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	pat := argString(args, "regex")
	containing := argString(args, "containing_symbol_name_path")
	incBody := argBool(args, "include_body", false)
	absFile, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(absFile)
	if err != nil {
		return "", err
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", fmt.Errorf("invalid regex: %w", err)
	}
	searchStart := 0
	searchEnd := len(src)
	if containing != "" {
		if sym, _, _, _, serr := resolveSingleSymbolInFile(repoRoot, rel, containing); serr == nil {
			searchStart = int(sym.StartByte)
			searchEnd = int(sym.EndByte)
		}
	}
	loc := re.FindIndex(src[searchStart:searchEnd])
	if loc == nil {
		return "", fmt.Errorf("regex %q not found in %s", pat, rel)
	}
	offset := searchStart + loc[0]
	line0 := bytes.Count(src[:offset], []byte("\n"))
	lastNL := bytes.LastIndexByte(src[:offset], '\n')
	col0 := offset
	if lastNL >= 0 {
		col0 = offset - lastNL - 1
	}

	lang := detectLSPLang(absFile)
	if lang == "" {
		return "", fmt.Errorf("no LSP configured for %s", rel)
	}
	resp, err := ensureAndQueryLSP(repoRoot, lang, lspIPCReq{
		Action: "definition",
		File:   absFile,
		Line:   line0,
		Col:    col0,
	}, false)
	if err != nil {
		return "", err
	}
	refs := formatLocations(repoRoot, resp.Locations)
	if len(refs) == 0 {
		return fmt.Sprintf("No declaration found for %q at %s:L%d.", pat, rel, line0+1), nil
	}
	var b strings.Builder
	for _, r := range refs {
		fmt.Fprintf(&b, "%s:L%d:%d  %s\n", r.RelPath, r.Line, r.Col+1, r.Preview)
		if incBody {
			defAbs := filepath.Join(repoRoot, filepath.FromSlash(r.RelPath))
			if defSrc, derr := os.ReadFile(defAbs); derr == nil {
				if syms, _, serr := extractFileSymbols(defAbs, r.RelPath, defSrc); serr == nil {
					for _, s := range flattenSymbols(syms) {
						if r.Line >= s.StartLine && r.Line <= s.EndLine && int(s.EndByte) <= len(defSrc) {
							b.WriteString("```\n")
							b.Write(defSrc[s.StartByte:s.EndByte])
							b.WriteString("\n```\n")
							break
						}
					}
				}
			}
		}
	}
	return truncateAnswer(strings.TrimSpace(b.String()), defaultMaxAnswerChars), nil
}

func toolFindImplementations(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path")
	sym, absFile, src, _, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	lang := detectLSPLang(absFile)
	if lang == "" {
		return "", fmt.Errorf("no LSP configured for %s", rel)
	}
	resp, err := queryLSPWithFuzzing(repoRoot, lang, absFile, src, sym, "implementation")
	if err != nil {
		return "", err
	}
	refs := formatLocations(repoRoot, resp.Locations)
	if len(refs) == 0 {
		return fmt.Sprintf("No implementations found for %s (%s:L%d).", sym.NamePath, rel, sym.StartLine), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Implementations of %s (%d):\n", sym.NamePath, len(refs))
	for _, r := range refs {
		fmt.Fprintf(&b, "- %s:L%d  %s\n", r.RelPath, r.Line, r.Preview)
	}
	return truncateAnswer(strings.TrimSpace(b.String()), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

func toolGetDiagnosticsForFile(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	absFile, err := resolveRepoPath(repoRoot, rel)
	if err != nil {
		return "", err
	}
	startLine := argInt(args, "start_line", 0)
	endLine := argInt(args, "end_line", 0)
	minSevStr := strings.ToLower(strings.TrimSpace(argString(args, "min_severity")))
	maxSevNum := 4
	switch minSevStr {
	case "error":
		maxSevNum = 1
	case "warning", "warn":
		maxSevNum = 2
	case "info", "information":
		maxSevNum = 3
	}

	var lines []string
	// 1. Instant AST syntax check
	if src, rerr := os.ReadFile(absFile); rerr == nil {
		if syn := checkSyntaxQuick(absFile, src); syn != "" {
			lines = append(lines, strings.Split(syn, "\n")...)
		}
	}
	// 2. Live LSP diagnostics
	if lang := detectLSPLang(absFile); lang != "" {
		if resp, lerr := ensureAndQueryLSP(repoRoot, lang, lspIPCReq{Action: "diagnostics", File: absFile}, false); lerr == nil {
			for _, d := range resp.Diagnostics {
				ln := d.Range.Start.Line + 1
				if startLine > 0 && ln < startLine {
					continue
				}
				if endLine > 0 && ln > endLine {
					continue
				}
				sev := d.Severity
				if sev == 0 {
					sev = 1
				}
				if sev > maxSevNum {
					continue
				}
				sevName := map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}[sev]
				srcTag := d.Source
				if srcTag == "" {
					srcTag = lang + "-lsp"
				}
				lines = append(lines, fmt.Sprintf("%s:%d:%d [%s/%s] %s", rel, ln, d.Range.Start.Character+1, srcTag, sevName, firstLineTrimmed(d.Message)))
			}
		} else {
			// Also include fast oxlint/ty if LSP wasn't available
			if quick := checkFileQuick(absFile); quick != "" {
				lines = append(lines, strings.Split(quick, "\n")...)
			}
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("No diagnostics in %s.", rel), nil
	}
	return truncateAnswer(strings.Join(lines, "\n"), argInt(args, "max_answer_chars", defaultMaxAnswerChars)), nil
}

type lspTextEdit struct {
	Range struct {
		Start struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"start"`
		End struct {
			Line      int `json:"line"`
			Character int `json:"character"`
		} `json:"end"`
	} `json:"range"`
	NewText string `json:"newText"`
}

func lineColToByteOffset(src []byte, line0, col0 int) int {
	curLine := 0
	idx := 0
	for idx < len(src) && curLine < line0 {
		if src[idx] == '\n' {
			curLine++
		}
		idx++
	}
	// Advance col0 bytes/UTF-16 units conservatively within line
	endLine := idx
	for endLine < len(src) && src[endLine] != '\n' {
		endLine++
	}
	pos := idx + col0
	if pos > endLine {
		pos = endLine
	}
	return pos
}

func applyWorkspaceEdit(repoRoot string, raw json.RawMessage) (string, error) {
	var we struct {
		Changes         map[string][]lspTextEdit `json:"changes"`
		DocumentChanges []struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Edits []lspTextEdit `json:"edits"`
		} `json:"documentChanges"`
	}
	if err := json.Unmarshal(raw, &we); err != nil {
		return "", fmt.Errorf("parse workspaceEdit: %w", err)
	}
	byFile := map[string][]lspTextEdit{}
	for uri, edits := range we.Changes {
		byFile[uriToFile(uri)] = append(byFile[uriToFile(uri)], edits...)
	}
	for _, dc := range we.DocumentChanges {
		if dc.TextDocument.URI != "" && len(dc.Edits) > 0 {
			f := uriToFile(dc.TextDocument.URI)
			byFile[f] = append(byFile[f], dc.Edits...)
		}
	}
	if len(byFile) == 0 {
		return "", errors.New("LSP returned empty workspace edit")
	}
	var files []string
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	var b strings.Builder
	totalEdits := 0
	for _, f := range files {
		if _, err := resolveRepoPath(repoRoot, f); err != nil {
			return "", fmt.Errorf("refusing edit outside repo: %w", err)
		}
		info, err := os.Stat(f)
		if err != nil {
			return "", err
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		edits := byFile[f]
		// Sort reverse by start position so byte offsets stay valid
		sort.Slice(edits, func(i, j int) bool {
			if edits[i].Range.Start.Line != edits[j].Range.Start.Line {
				return edits[i].Range.Start.Line > edits[j].Range.Start.Line
			}
			return edits[i].Range.Start.Character > edits[j].Range.Start.Character
		})
		cur := src
		for _, e := range edits {
			sb := lineColToByteOffset(cur, e.Range.Start.Line, e.Range.Start.Character)
			eb := lineColToByteOffset(cur, e.Range.End.Line, e.Range.End.Character)
			if sb > eb || eb > len(cur) {
				continue
			}
			var buf bytes.Buffer
			buf.Write(cur[:sb])
			buf.WriteString(e.NewText)
			buf.Write(cur[eb:])
			cur = buf.Bytes()
			totalEdits++
		}
		if err := atomicWriteFile(f, cur, info.Mode().Perm()); err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(repoRoot, f)
		fmt.Fprintf(&b, "- %s (%d edits)\n", filepath.ToSlash(rel), len(edits))
	}
	return fmt.Sprintf("Renamed cleanly (%d edits across %d files):\n%s", totalEdits, len(files), strings.TrimSpace(b.String())), nil
}

func toolRenameSymbol(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path")
	newName := argString(args, "new_name")
	if newName == "" {
		return "", errors.New("new_name is required")
	}
	sym, absFile, _, _, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	lang := detectLSPLang(absFile)
	if lang == "" {
		return "", fmt.Errorf("no LSP configured for %s", rel)
	}
	resp, err := ensureAndQueryLSP(repoRoot, lang, lspIPCReq{
		Action:  "rename",
		File:    absFile,
		Line:    sym.NameLine - 1,
		Col:     sym.NameCol,
		NewName: newName,
	}, false)
	if err != nil {
		return "", err
	}
	return applyWorkspaceEdit(repoRoot, resp.Edit)
}

func toolSafeDeleteSymbol(repoRoot string, args map[string]any) (string, error) {
	rel := argString(args, "relative_path")
	np := argString(args, "name_path_pattern")
	if np == "" {
		np = argString(args, "name_path")
	}
	sym, absFile, src, perm, err := resolveSingleSymbolInFile(repoRoot, rel, np)
	if err != nil {
		return "", err
	}
	// Check external references first via LSP (fallback rg)
	var extRefs []string
	if lang := detectLSPLang(absFile); lang != "" {
		if resp, lerr := queryLSPWithFuzzing(repoRoot, lang, absFile, src, sym, "references"); lerr == nil && resp != nil {
			for _, r := range formatLocations(repoRoot, resp.Locations) {
				if r.RelPath == sym.RelPath && r.Line >= sym.StartLine && r.Line <= sym.EndLine {
					continue
				}
				extRefs = append(extRefs, fmt.Sprintf("- %s:L%d  %s", r.RelPath, r.Line, r.Preview))
			}
		} else {
			extRefs = fastCallersViaRG(repoRoot, sym, 10)
		}
	} else {
		extRefs = fastCallersViaRG(repoRoot, sym, 10)
	}
	if len(extRefs) > 0 {
		return "", fmt.Errorf("refusing safe_delete_symbol: %s still has %d reference(s):\n%s", sym.NamePath, len(extRefs), strings.Join(extRefs, "\n"))
	}
	// Delete exact AST byte span + trailing newline if present
	endB := sym.EndByte
	if int(endB) < len(src) && src[endB] == '\n' {
		endB++
	}
	var out bytes.Buffer
	out.Write(src[:sym.StartByte])
	out.Write(src[endB:])
	if err := atomicWriteFile(absFile, out.Bytes(), perm); err != nil {
		return "", err
	}
	return fmt.Sprintf("Safely deleted unreferenced symbol %s from %s (was L%d-L%d).", sym.NamePath, rel, sym.StartLine, sym.EndLine), nil
}
