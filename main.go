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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type daemonMeta struct {
	PID       int    `json:"pid"`
	ChildPID  int    `json:"child_pid"`
	PGID      int    `json:"pgid"`
	Port      int    `json:"port"`
	Project   string `json:"project"`
	Context   string `json:"context"`
	StartedAt string `json:"started_at"`
}

type parsedConfig struct {
	passthrough bool
	project     string
	contextName string
	modes       []string
	addModes    []string
	backend     string
	extraArgs   []string
}

func realSerenaBin() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_REAL_BIN")); v != "" {
		return v
	}
	cfg, _ := loadUserConfig()
	if v := strings.TrimSpace(cfg.LegacyBin); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		cand := filepath.Join(home, ".local", "share", "uv", "tools", "serena-agent", "bin", "serena")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return "serena-agent"
}

func stateBaseDir() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_STATE_DIR")); v != "" {
		return v
	}
	return fmt.Sprintf("/tmp/serena-shim-%d", os.Getuid())
}

func schemaCacheBaseDir() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_SCHEMA_CACHE_DIR")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_STATE_DIR")); v != "" {
		return v
	}
	if ucd, err := os.UserCacheDir(); err == nil && strings.TrimSpace(ucd) != "" {
		return filepath.Join(ucd, "serena-shim")
	}
	return stateBaseDir()
}

func idleTimeouts() (idle time.Duration, initOnly time.Duration) {
	idle = 180 * time.Second
	initOnly = 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_IDLE_SECS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			idle = time.Duration(n) * time.Second
		}
	}
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_INIT_IDLE_SECS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			initOnly = time.Duration(n) * time.Second
		}
	}
	if memoryPressureLevel() >= 2 && idle > 45*time.Second {
		idle = 45 * time.Second
	}
	return idle, initOnly
}

func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err == nil {
		p = abs
	}
	if realP, err := filepath.EvalSymlinks(p); err == nil {
		return realP
	}
	return p
}

func resolveProjectArg(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, string(filepath.Separator)) || raw == "." || raw == ".." {
		return canonicalPath(raw)
	}
	if _, err := os.Stat(raw); err == nil {
		return canonicalPath(raw)
	}
	return raw
}

func parseStartArgs(args []string, cwd string) parsedConfig {
	cfg := parsedConfig{contextName: "desktop-app"}
	i := 0
	for i < len(args) {
		a := args[i]
		nextVal := func() (string, bool) {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				return a[eq+1:], true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		key := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			key = a[:eq]
		}
		switch key {
		case "-h", "--help":
			cfg.passthrough = true
			return cfg
		case "--transport":
			v, ok := nextVal()
			if !ok || (v != "" && v != "stdio") {
				cfg.passthrough = true
				return cfg
			}
		case "--host", "--port":
			cfg.passthrough = true
			return cfg
		case "--enable-web-dashboard", "--open-web-dashboard":
			_, _ = nextVal()
		case "--project-from-cwd":
			if cwd != "" {
				cfg.project = canonicalPath(cwd)
			}
		case "--project", "--project-file":
			if v, ok := nextVal(); ok {
				cfg.project = resolveProjectArg(v)
			}
		case "--context":
			if v, ok := nextVal(); ok && v != "" {
				cfg.contextName = v
			}
		case "--mode":
			if v, ok := nextVal(); ok && v != "" {
				cfg.modes = append(cfg.modes, v)
			}
		case "--add-mode":
			if v, ok := nextVal(); ok && v != "" {
				cfg.addModes = append(cfg.addModes, v)
			}
		case "--language-backend":
			if v, ok := nextVal(); ok {
				cfg.backend = v
			}
		default:
			cfg.extraArgs = append(cfg.extraArgs, a)
		}
		i++
	}
	return cfg
}

func daemonKey(cfg parsedConfig) string {
	modes := append([]string{}, cfg.modes...)
	sort.Strings(modes)
	addModes := append([]string{}, cfg.addModes...)
	sort.Strings(addModes)
	// Unified across all agent contexts (agy/claude/codex/grok) so sibling agents share 1 daemon per repo.
	raw := strings.Join([]string{
		cfg.project,
		strings.Join(modes, ","),
		strings.Join(addModes, ","),
		cfg.backend,
	}, "\n")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

func buildHTTPServerArgs(cfg parsedConfig, port int) []string {
	out := []string{
		"start-mcp-server",
		"--transport", "streamable-http",
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--enable-web-dashboard", "false",
		"--open-web-dashboard", "false",
		"--context", cfg.contextName,
	}
	if cfg.project != "" {
		out = append(out, "--project", cfg.project)
	}
	for _, m := range cfg.modes {
		out = append(out, "--mode", m)
	}
	for _, m := range cfg.addModes {
		out = append(out, "--add-mode", m)
	}
	if cfg.backend != "" {
		out = append(out, "--language-backend", cfg.backend)
	}
	out = append(out, cfg.extraArgs...)
	return out
}

func allocFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func portReady(port int) bool {
	if port <= 0 {
		return false
	}
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func httpEndpointReady(client *http.Client, port int) bool {
	if !portReady(port) {
		return false
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// Uvicorn/FastMCP returns HTTP (e.g. 406/400/405/200) only after lifespan finishes.
	return resp.StatusCode > 0 && resp.StatusCode < 500
}

func touchFile(path string) {
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		if f, err2 := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600); err2 == nil {
			_ = f.Close()
		}
	}
}

func acquireLock(lockPath string) (*os.File, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func readMeta(metaPath string) (*daemonMeta, error) {
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var m daemonMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func killProcessGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

func runSupervisor(args []string) int {
	// Format: __supervise <key> <port> <stateDir> <project> <context> -- <realArgs...>
	if len(args) < 7 {
		fmt.Fprintln(os.Stderr, "serena-shim supervisor: insufficient arguments")
		return 2
	}
	key := args[0]
	port, err := strconv.Atoi(args[1])
	if err != nil || port <= 0 {
		fmt.Fprintf(os.Stderr, "serena-shim supervisor: invalid port %q\n", args[1])
		return 2
	}
	stateDir := args[2]
	project := args[3]
	contextName := args[4]
	sep := -1
	for i := 5; i < len(args); i++ {
		if args[i] == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "serena-shim supervisor: missing '-- <args>'")
		return 2
	}
	realArgs := args[sep+1:]

	logPath := filepath.Join(stateDir, key+".log")
	metaPath := filepath.Join(stateDir, key+".json")
	activityPath := filepath.Join(stateDir, key+".activity")
	toolCalledPath := filepath.Join(stateDir, key+".tool_called")

	_ = os.Remove(toolCalledPath)
	touchFile(activityPath)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serena-shim supervisor: open log: %v\n", err)
		return 1
	}
	defer logFile.Close()

	cmd := exec.Command(realSerenaBin(), realArgs...)
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), "NODE_OPTIONS=--max-old-space-size=768")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "serena-shim supervisor: start real serena: %v\n", err)
		return 1
	}

	childPID := cmd.Process.Pid
	pgid := childPID
	if p, err := syscall.Getpgid(childPID); err == nil && p > 0 {
		pgid = p
	}

	meta := daemonMeta{
		PID:       os.Getpid(),
		ChildPID:  childPID,
		PGID:      pgid,
		Port:      port,
		Project:   project,
		Context:   contextName,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if raw, err := json.Marshal(meta); err == nil {
		_ = os.WriteFile(metaPath, raw, 0o600)
	}

	childDone := make(chan error, 1)
	go func() {
		childDone <- cmd.Wait()
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	idleTimeout, initIdleTimeout := idleTimeouts()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	reapAndExit := func(code int) int {
		_ = os.Remove(metaPath)
		_ = os.Remove(toolCalledPath)
		killProcessGroup(pgid)
		select {
		case <-childDone:
		case <-time.After(2 * time.Second):
		}
		return code
	}

	for {
		select {
		case <-childDone:
			_ = os.Remove(metaPath)
			_ = os.Remove(toolCalledPath)
			return 0
		case <-sigCh:
			return reapAndExit(0)
		case <-ticker.C:
			info, err := os.Stat(activityPath)
			if err != nil {
				continue
			}
			limit := initIdleTimeout
			if _, err := os.Stat(toolCalledPath); err == nil {
				limit = idleTimeout
			}
			if time.Since(info.ModTime()) >= limit {
				fmt.Fprintf(logFile, "[serena-shim] idle timeout %s reached; reaping pgid=%d\n", limit, pgid)
				return reapAndExit(0)
			}
		}
	}
}

type daemonClient struct {
	key        string
	cfg        parsedConfig
	stateDir   string
	metaPath   string
	lockPath   string
	actPath    string
	toolPath   string
	schemaPath string
	httpClient *http.Client

	cacheMu   sync.Mutex
	mu        sync.Mutex
	port      int
	sessionID string
	epoch     uint64
	initReq   []byte
	initProto string
	initNotif []byte
}

func newDaemonClient(key string, cfg parsedConfig) (*daemonClient, error) {
	dir := stateBaseDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	schemaDir := schemaCacheBaseDir()
	_ = os.MkdirAll(schemaDir, 0o700)
	return &daemonClient{
		key:        key,
		cfg:        cfg,
		stateDir:   dir,
		metaPath:   filepath.Join(dir, key+".json"),
		lockPath:   filepath.Join(dir, key+".lock"),
		actPath:    filepath.Join(dir, key+".activity"),
		toolPath:   filepath.Join(dir, key+".tool_called"),
		schemaPath: filepath.Join(schemaDir, key+".schema.json"),
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}, nil
}

func (c *daemonClient) ensureRunningLocked(forceRestart bool) (int, error) {
	if !forceRestart {
		if m, err := readMeta(c.metaPath); err == nil && pidAlive(m.ChildPID) && httpEndpointReady(c.httpClient, m.Port) {
			c.port = m.Port
			touchFile(c.actPath)
			return m.Port, nil
		}
	}

	lockFile, err := acquireLock(c.lockPath)
	if err != nil {
		return 0, err
	}
	defer releaseLock(lockFile)

	if !forceRestart {
		if m, err := readMeta(c.metaPath); err == nil && pidAlive(m.ChildPID) && httpEndpointReady(c.httpClient, m.Port) {
			c.port = m.Port
			touchFile(c.actPath)
			return m.Port, nil
		}
	}

	if m, err := readMeta(c.metaPath); err == nil {
		if pidAlive(m.ChildPID) {
			killProcessGroup(m.PGID)
		}
		_ = os.Remove(c.metaPath)
	}

	port, err := allocFreePort()
	if err != nil {
		return 0, err
	}
	selfExe, err := os.Executable()
	if err != nil {
		selfExe = os.Args[0]
	}

	serverArgs := buildHTTPServerArgs(c.cfg, port)
	supArgs := []string{"__supervise", c.key, strconv.Itoa(port), c.stateDir, c.cfg.project, c.cfg.contextName, "--"}
	supArgs = append(supArgs, serverArgs...)

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()

	cmd := exec.Command(selfExe, supArgs...)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	supDone := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(supDone)
	}()

	// Wait up to 30s for FastMCP streamable-http endpoint readiness.
	deadline := time.Now().Add(30 * time.Second)
	probeClient := &http.Client{Timeout: 1 * time.Second}
	for time.Now().Before(deadline) {
		if httpEndpointReady(probeClient, port) {
			c.port = port
			c.sessionID = ""
			c.epoch++
			touchFile(c.actPath)
			return port, nil
		}
		select {
		case <-supDone:
			return 0, fmt.Errorf("serena daemon exited before becoming ready (see %s.log)", filepath.Join(c.stateDir, c.key))
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}
	killProcessGroup(cmd.Process.Pid)
	return 0, fmt.Errorf("timed out waiting for serena HTTP daemon on 127.0.0.1:%d", port)
}

func extractSSEPayloads(raw []byte) ([][]byte, error) {
	var payloads [][]byte
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		joined := strings.TrimSpace(strings.Join(cur, "\n"))
		cur = nil
		if joined != "" {
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(joined)); err == nil {
				payloads = append(payloads, compact.Bytes())
			} else {
				payloads = append(payloads, []byte(strings.ReplaceAll(joined, "\n", " ")))
			}
		}
	}

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			cur = append(cur, val)
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return payloads, nil
}

type errSessionGone struct {
	status int
	body   string
}

func (e errSessionGone) Error() string {
	return fmt.Sprintf("session invalid (HTTP %d): %s", e.status, e.body)
}

func (c *daemonClient) postOnce(port int, sid string, payload []byte) (newSID string, lines [][]byte, err error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return sid, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return sid, nil, errSessionGone{status: 0, body: err.Error()}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return sid, nil, err
	}

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return sid, nil, errSessionGone{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	if resp.StatusCode >= 500 {
		return sid, nil, errSessionGone{status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}

	retSID := sid
	if got := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); got != "" {
		retSID = got
	}

	if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
		return retSID, nil, nil
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") || bytes.Contains(body, []byte("\ndata:")) || bytes.HasPrefix(bytes.TrimSpace(body), []byte("event:")) || bytes.HasPrefix(bytes.TrimSpace(body), []byte("data:")) {
		parsed, err := extractSSEPayloads(body)
		return retSID, parsed, err
	}

	trimmed := bytes.TrimSpace(body)
	var compact bytes.Buffer
	if err := json.Compact(&compact, trimmed); err == nil {
		return retSID, [][]byte{compact.Bytes()}, nil
	}
	return retSID, [][]byte{trimmed}, nil
}

type schemaEntry struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type serenaSchemaCache struct {
	UpdatedAtUnix int64                  `json:"updated_at_unix"`
	BinMtimeNS    int64                  `json:"bin_mtime_ns"`
	InitByProto   map[string]schemaEntry `json:"init_by_proto,omitempty"`
	Methods       map[string]schemaEntry `json:"methods,omitempty"`
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

func schemaCacheEnabled() bool {
	return os.Getenv("SERENA_SHIM_NO_SCHEMA_CACHE") != "1"
}

func schemaCacheTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("SERENA_SHIM_SCHEMA_TTL_SECS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 24 * time.Hour
}

func realBinMtimeNS() int64 {
	if info, err := os.Stat(realSerenaBin()); err == nil {
		return info.ModTime().UnixNano()
	}
	return 0
}

func hasRPCID(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

func extractProtoVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	return p.ProtocolVersion
}

func hasCursorParam(params json.RawMessage) bool {
	if len(bytes.TrimSpace(params)) == 0 {
		return false
	}
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)
	return p.Cursor != ""
}

func hasNextCursorResult(res json.RawMessage) bool {
	if len(bytes.TrimSpace(res)) == 0 {
		return false
	}
	var r struct {
		NextCursor string `json:"nextCursor"`
	}
	_ = json.Unmarshal(res, &r)
	return r.NextCursor != ""
}

func isCacheableSchemaMethod(method string) bool {
	switch method {
	case "server/discover", "tools/list", "resources/list", "resources/templates/list", "prompts/list":
		return true
	default:
		return false
	}
}

func marshalCachedRPCResponse(id json.RawMessage, entry schemaEntry) ([]byte, error) {
	out := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   json.RawMessage `json:"error,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      bytes.TrimSpace(id),
		Result:  bytes.TrimSpace(entry.Result),
		Error:   bytes.TrimSpace(entry.Error),
	}
	return json.Marshal(out)
}

func (c *daemonClient) loadSchemaCache() (*serenaSchemaCache, bool) {
	if !schemaCacheEnabled() {
		return nil, false
	}
	ttl := schemaCacheTTL()
	if ttl <= 0 {
		return nil, false
	}
	c.cacheMu.Lock()
	raw, err := os.ReadFile(c.schemaPath)
	c.cacheMu.Unlock()
	if err != nil {
		return nil, false
	}
	var sc serenaSchemaCache
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, false
	}
	if time.Since(time.Unix(sc.UpdatedAtUnix, 0)) > ttl {
		return nil, false
	}
	curMT := realBinMtimeNS()
	if curMT != 0 && sc.BinMtimeNS != 0 && curMT != sc.BinMtimeNS {
		return nil, false
	}
	if sc.InitByProto == nil {
		sc.InitByProto = map[string]schemaEntry{}
	}
	if sc.Methods == nil {
		sc.Methods = map[string]schemaEntry{}
	}
	return &sc, true
}

func (c *daemonClient) saveSchemaCache(mutator func(*serenaSchemaCache)) {
	if !schemaCacheEnabled() {
		return
	}
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	var sc serenaSchemaCache
	if raw, err := os.ReadFile(c.schemaPath); err == nil {
		_ = json.Unmarshal(raw, &sc)
	}
	if sc.InitByProto == nil {
		sc.InitByProto = map[string]schemaEntry{}
	}
	if sc.Methods == nil {
		sc.Methods = map[string]schemaEntry{}
	}
	mutator(&sc)
	sc.UpdatedAtUnix = time.Now().Unix()
	if mt := realBinMtimeNS(); mt != 0 {
		sc.BinMtimeNS = mt
	}
	data, err := json.Marshal(sc)
	if err != nil {
		return
	}
	schemaDir := filepath.Dir(c.schemaPath)
	if err := os.MkdirAll(schemaDir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(schemaDir, ".schema-*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpName)
		return
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, c.schemaPath); err != nil {
		_ = os.Remove(tmpName)
	}
}

func (c *daemonClient) cacheInitResponseLocked(proto string, respLines [][]byte) {
	if proto == "" {
		proto = "_default"
	}
	for _, ln := range respLines {
		var env rpcEnvelope
		if json.Unmarshal(ln, &env) == nil && len(bytes.TrimSpace(env.Result)) > 0 {
			resCopy := bytes.TrimSpace(env.Result)
			c.saveSchemaCache(func(sc *serenaSchemaCache) {
				sc.InitByProto[proto] = schemaEntry{Result: resCopy}
			})
			return
		}
	}
}

func (c *daemonClient) cacheMethodResponse(method string, params []byte, respLines [][]byte) {
	if !isCacheableSchemaMethod(method) || hasCursorParam(params) {
		return
	}
	for _, ln := range respLines {
		var env rpcEnvelope
		if json.Unmarshal(ln, &env) != nil {
			continue
		}
		if len(bytes.TrimSpace(env.Result)) == 0 && len(bytes.TrimSpace(env.Error)) == 0 {
			continue
		}
		if hasNextCursorResult(env.Result) {
			return
		}
		resCopy := bytes.TrimSpace(env.Result)
		errCopy := bytes.TrimSpace(env.Error)
		c.saveSchemaCache(func(sc *serenaSchemaCache) {
			sc.Methods[method] = schemaEntry{Result: resCopy, Error: errCopy}
		})
		return
	}
}

func (c *daemonClient) daemonLiveQuick() bool {
	if m, err := readMeta(c.metaPath); err == nil && pidAlive(m.ChildPID) && portReady(m.Port) {
		return true
	}
	return false
}

func (c *daemonClient) replaySessionLocked() error {
	port, err := c.ensureRunningLocked(false)
	if err != nil {
		return err
	}
	initReq := c.initReq
	initProto := c.initProto
	initNotif := c.initNotif
	if len(initReq) == 0 {
		initProto = "2024-11-05"
		initReq = []byte(`{"jsonrpc":"2.0","id":"__serena_init","method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"serena-shim","version":"1.0"}}}`)
	}
	sid, initLines, err := c.postOnce(port, "", initReq)
	if err != nil {
		// Force restart once if existing process was half-dead.
		port, err = c.ensureRunningLocked(true)
		if err != nil {
			return err
		}
		sid, initLines, err = c.postOnce(port, "", initReq)
		if err != nil {
			return err
		}
	}
	c.cacheInitResponseLocked(initProto, initLines)
	c.sessionID = sid
	if len(initNotif) == 0 {
		initNotif = []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	}
	_, _, _ = c.postOnce(port, c.sessionID, initNotif)
	return nil
}

func parseMethod(line []byte) string {
	var msg struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(line, &msg)
	return msg.Method
}

func (c *daemonClient) forward(line []byte) ([][]byte, error) {
	var env rpcEnvelope
	if json.Unmarshal(line, &env) != nil {
		return nil, nil
	}
	method := env.Method

	if method == "initialize" {
		proto := extractProtoVersion(env.Params)
		c.mu.Lock()
		c.initReq = append([]byte(nil), line...)
		c.initProto = proto
		c.mu.Unlock()

		if hasRPCID(env.ID) {
			if sc, ok := c.loadSchemaCache(); ok {
				p := proto
				if p == "" {
					p = "_default"
				}
				if entry, found := sc.InitByProto[p]; found {
					if b, merr := marshalCachedRPCResponse(env.ID, entry); merr == nil {
						return [][]byte{b}, nil
					}
				}
			}
		}
	} else if method == "notifications/initialized" {
		c.mu.Lock()
		c.initNotif = append([]byte(nil), line...)
		sid := c.sessionID
		port := c.port
		c.mu.Unlock()
		if sid == "" || !c.daemonLiveQuick() {
			// Deferred until first wake request replays initialize+notifications/initialized.
			return nil, nil
		}
		_, _, _ = c.postOnce(port, sid, line)
		return nil, nil
	} else if !hasRPCID(env.ID) || method == "" {
		// Drop notifications / client responses when daemon is asleep so idle sessions never respawn it.
		c.mu.Lock()
		sid := c.sessionID
		port := c.port
		c.mu.Unlock()
		if sid == "" || !c.daemonLiveQuick() {
			return nil, nil
		}
		_, _, _ = c.postOnce(port, sid, line)
		return nil, nil
	} else if method == "server/discover" && hasRPCID(env.ID) {
		if sc, ok := c.loadSchemaCache(); ok {
			if entry, found := sc.Methods[method]; found {
				if b, merr := marshalCachedRPCResponse(env.ID, entry); merr == nil {
					return [][]byte{b}, nil
				}
			}
		}
		// FastMCP streamable-http does not support pre-init server/discover; never wake daemon for it.
		return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}`, bytes.TrimSpace(env.ID)))}, nil
	} else if method == "subscriptions/listen" && hasRPCID(env.ID) {
		// Long-polling subscriptions must never wake or pin the lazy Serena daemon.
		return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}`, bytes.TrimSpace(env.ID)))}, nil
	} else if method == "ping" && hasRPCID(env.ID) {
		// Answer keepalive pings locally without waking the daemon or resetting .activity.
		return [][]byte{[]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, bytes.TrimSpace(env.ID)))}, nil
	} else if isCacheableSchemaMethod(method) && hasRPCID(env.ID) && !hasCursorParam(env.Params) {
		c.mu.Lock()
		sid := c.sessionID
		c.mu.Unlock()
		if sid == "" || !c.daemonLiveQuick() {
			if sc, ok := c.loadSchemaCache(); ok {
				if entry, found := sc.Methods[method]; found {
					if b, merr := marshalCachedRPCResponse(env.ID, entry); merr == nil {
						return [][]byte{b}, nil
					}
				}
			}
		}
	}

	if method == "tools/call" {
		touchFile(c.toolPath)
	}
	touchFile(c.actPath)

	c.mu.Lock()
	var port int
	var err error
	if method != "initialize" && c.sessionID == "" {
		if err = c.replaySessionLocked(); err == nil {
			port = c.port
		}
	} else {
		port, err = c.ensureRunningLocked(false)
	}
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	sid := c.sessionID
	startEpoch := c.epoch
	c.mu.Unlock()

	newSID, respLines, err := c.postOnce(port, sid, line)
	if err == nil {
		if newSID != "" && newSID != sid {
			c.mu.Lock()
			if c.epoch == startEpoch {
				c.sessionID = newSID
			}
			c.mu.Unlock()
		}
		if method == "initialize" {
			c.cacheInitResponseLocked(extractProtoVersion(env.Params), respLines)
		} else {
			c.cacheMethodResponse(method, env.Params, respLines)
		}
		touchFile(c.actPath)
		return respLines, nil
	}

	var gone errSessionGone
	if !errors.As(err, &gone) || method == "initialize" {
		return nil, err
	}

	// Re-wake & replay MCP handshake transparently.
	c.mu.Lock()
	if c.epoch == startEpoch {
		if rerr := c.replaySessionLocked(); rerr != nil {
			c.mu.Unlock()
			return nil, rerr
		}
	}
	port = c.port
	sid = c.sessionID
	c.mu.Unlock()

	_, respLines, err = c.postOnce(port, sid, line)
	if err == nil {
		c.cacheMethodResponse(method, env.Params, respLines)
		touchFile(c.actPath)
	}
	return respLines, err
}

func runProxy(cfg parsedConfig) int {
	key := daemonKey(cfg)
	client, err := newDaemonClient(key, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "serena-shim: %v\n", err)
		return 1
	}

	reader := bufio.NewReaderSize(os.Stdin, 256*1024)
	var stdoutMu sync.Mutex
	var wg sync.WaitGroup

	writeError := func(rawReq []byte, err error) {
		var reqObj struct {
			JSONRPC string `json:"jsonrpc"`
			ID      any    `json:"id"`
		}
		if json.Unmarshal(rawReq, &reqObj) != nil || reqObj.ID == nil {
			return
		}
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      reqObj.ID,
			"error": map[string]any{
				"code":    -32603,
				"message": err.Error(),
			},
		}
		b, _ := json.Marshal(resp)
		stdoutMu.Lock()
		_, _ = os.Stdout.Write(append(b, '\n'))
		stdoutMu.Unlock()
	}

	writePayloads := func(lines [][]byte) {
		if len(lines) == 0 {
			return
		}
		stdoutMu.Lock()
		for _, ln := range lines {
			_, _ = os.Stdout.Write(append(ln, '\n'))
		}
		stdoutMu.Unlock()
	}

	for {
		lineBytes, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(lineBytes)
		if len(trimmed) > 0 {
			msgCopy := append([]byte(nil), trimmed...)
			method := parseMethod(msgCopy)
			if method == "server/discover" || method == "initialize" || method == "notifications/initialized" {
				outLines, ferr := client.forward(msgCopy)
				if ferr != nil {
					writeError(msgCopy, ferr)
				} else {
					writePayloads(outLines)
				}
			} else {
				wg.Add(1)
				go func(req []byte) {
					defer wg.Done()
					outLines, ferr := client.forward(req)
					if ferr != nil {
						writeError(req, ferr)
					} else {
						writePayloads(outLines)
					}
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

func execRealSerena() int {
	realBin := realSerenaBin()
	if err := syscall.Exec(realBin, append([]string{realBin}, os.Args[1:]...), os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "serena-shim: exec %s: %v\n", realBin, err)
		return 127
	}
	return 0
}

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "__supervise":
			os.Exit(runSupervisor(os.Args[2:]))
		case "__lsp_supervise":
			os.Exit(runLSPSupervisor(os.Args[2:]))
		case "check-file":
			if len(os.Args) >= 3 {
				if diag := checkFileQuick(canonicalPath(os.Args[2])); diag != "" {
					fmt.Println(diag)
				}
			}
			os.Exit(0)
		case "--version", "-v", "version":
			printVersion()
			os.Exit(0)
		case "--help", "-h", "help":
			printUsage()
			os.Exit(0)
		case "doctor":
			os.Exit(runDoctor())
		case "status":
			os.Exit(runStatus())
		case "stop-lsp":
			os.Exit(runStopLSP())
		}
	}
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}
	if os.Getenv("SERENA_SHIM_DISABLE") == "1" || os.Args[1] != "start-mcp-server" {
		os.Exit(execRealSerena())
	}
	cwd, _ := os.Getwd()
	cfg := parseStartArgs(os.Args[2:], cwd)
	if cfg.passthrough {
		os.Exit(execRealSerena())
	}
	// Escape hatch: SERENA_ENGINE=legacy or custom SERENA_SHIM_REAL_BIN (e.g., legacy shim tests) uses legacy HTTP proxy.
	if strings.EqualFold(os.Getenv("SERENA_ENGINE"), "legacy") || strings.TrimSpace(os.Getenv("SERENA_SHIM_REAL_BIN")) != "" {
		os.Exit(runProxy(cfg))
	}
	os.Exit(runNativeMCP(cfg))
}
