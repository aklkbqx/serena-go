package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	defaultMaxIndexFiles     = 8000
	defaultMaxIndexFileBytes = 256 * 1024
	maxEmbedBatch            = 32
)

func semanticIndexDir() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_INDEX_DIR")); v != "" {
		return v
	}
	if ucd, err := os.UserCacheDir(); err == nil && ucd != "" {
		return filepath.Join(ucd, "serena-index")
	}
	return fmt.Sprintf("/tmp/serena-index-%d", os.Getuid())
}

func legacySemanticIndexPath(repoRoot string) string {
	sum := sha256.Sum256([]byte(canonicalPath(repoRoot)))
	return filepath.Join(semanticIndexDir(), hex.EncodeToString(sum[:])[:16]+".json")
}

func maxIndexFilesLimit() int {
	if v := strings.TrimSpace(os.Getenv("SERENA_MAX_INDEX_FILES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxIndexFiles
}

func maxIndexFileBytesLimit() int64 {
	if v := strings.TrimSpace(os.Getenv("SERENA_MAX_INDEX_FILE_KB")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n * 1024
		}
	}
	return defaultMaxIndexFileBytes
}

// isForbiddenIndexRoot protects user privacy by refusing to crawl/embed $HOME, /, /tmp, /etc, or /Volumes
// when MCP clients invoke --project-from-cwd outside an actual repository.
func isForbiddenIndexRoot(repoRoot string) bool {
	if strings.TrimSpace(os.Getenv("SERENA_ALLOW_HOME_INDEX")) == "1" {
		return false
	}
	c := canonicalPath(repoRoot)
	if c == "" || c == "/" {
		return true
	}
	home, _ := os.UserHomeDir()
	forbidden := []string{
		canonicalPath(home),
		canonicalPath("/tmp"),
		canonicalPath("/var"),
		canonicalPath("/etc"),
		canonicalPath("/usr"),
		canonicalPath("/Volumes"),
	}
	for _, f := range forbidden {
		if f != "" && c == f {
			return true
		}
	}
	return false
}

func shouldSkipIndexDir(name string) bool {
	switch name {
	case ".git", "node_modules", "graft", "dist", ".next", "build", "target", "Pods",
		"DerivedData", "vendor", ".expo", ".venv", "venv", "__pycache__", ".cache",
		".turbo", ".serena", "coverage", ".gradle", ".idea", ".vscode",
		".superpowers", ".worktrees", ".claude", ".gemini", ".codex", ".grok", "artifacts":
		return true
	}
	return false
}

func isIndexableCodeFile(name string) bool {
	low := strings.ToLower(name)
	if strings.HasSuffix(low, ".d.ts") || strings.HasSuffix(low, ".min.js") || strings.HasSuffix(low, ".min.css") ||
		strings.HasSuffix(low, ".lock") || strings.HasSuffix(low, ".bundle.js") {
		return false
	}
	switch filepath.Ext(low) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts",
		".rs", ".py", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".m", ".mm",
		".java", ".kt", ".sql", ".sh", ".zsh", ".rb", ".php", ".vue", ".svelte":
		return true
	}
	return false
}

func computeChunkHash(rel, namePath, snippet string) string {
	sum := sha256.Sum256([]byte(rel + "\n" + namePath + "\n" + snippet))
	return hex.EncodeToString(sum[:])[:16]
}

// --- Code-Aware + Thai-Aware Tokenizer, Stemming & Domain Synonyms ---

var codeStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true,
	"this": true, "where": true, "what": true, "how": true, "are": true, "does": true,
	"return": true, "const": true, "var": true, "let": true, "func": true, "function": true,
	"import": true, "export": true, "true": true, "false": true, "null": true, "nil": true,
	"string": true, "number": true, "bool": true, "error": true, "err": true, "any": true,
}

var domainSynonymGroups = [][]string{
	{"auth", "login", "signin", "jwt", "session", "token", "oauth", "credential"},
	{"db", "database", "sql", "postgres", "drizzle", "query", "migration", "schema", "pool"},
	{"pay", "payment", "stripe", "billing", "invoice", "checkout", "charge", "refund"},
	{"webhook", "callback", "event", "hook", "dedup", "idempotency"},
	{"retry", "backoff", "attempt", "resilient", "timeout", "circuit"},
	{"cache", "redis", "ttl", "memo", "evict", "lru"},
	{"perm", "permission", "rbac", "role", "guard", "policy", "admin", "access"},
	{"notify", "notification", "email", "smtp", "sms", "line", "push", "alert"},
	{"upload", "storage", "s3", "r2", "bucket", "media", "presign"},
	{"ws", "websocket", "realtime", "socket", "stream", "sse", "pubsub"},
	{"deploy", "docker", "compose", "traefik", "nginx", "ship", "healthcheck"},
	{"lsp", "ast", "symbol", "diagnostic", "lint", "treesitter", "parser"},
	{"rate", "ratelimit", "throttle", "limiter", "quota", "burst"},
	{"memory", "pressure", "ram", "oom", "evict", "cap", "limit"},
}

var synonymLookup = func() map[string][]string {
	m := map[string][]string{}
	for _, grp := range domainSynonymGroups {
		for _, w := range grp {
			for _, other := range grp {
				if other != w {
					m[w] = append(m[w], other)
				}
			}
		}
	}
	return m
}()

func stemEnglishToken(tok string) string {
	if len(tok) < 5 {
		return tok
	}
	// Only stem ASCII English words
	for i := 0; i < len(tok); i++ {
		if tok[i] < 'a' || tok[i] > 'z' {
			return tok
		}
	}
	switch {
	case strings.HasSuffix(tok, "ation") && len(tok) >= 8:
		return tok[:len(tok)-5]
	case strings.HasSuffix(tok, "ies") && len(tok) >= 6:
		return tok[:len(tok)-3] + "y"
	case strings.HasSuffix(tok, "ing") && len(tok) >= 6:
		return tok[:len(tok)-3]
	case strings.HasSuffix(tok, "ed") && len(tok) >= 5:
		return tok[:len(tok)-2]
	case strings.HasSuffix(tok, "es") && len(tok) >= 5 && !strings.HasSuffix(tok, "ses"):
		return tok[:len(tok)-2]
	case strings.HasSuffix(tok, "s") && !strings.HasSuffix(tok, "ss") && !strings.HasSuffix(tok, "us") && len(tok) >= 5:
		return tok[:len(tok)-1]
	}
	return tok
}

func isThaiRune(r rune) bool {
	return r >= 0x0E00 && r <= 0x0E7F
}

// splitThaiGraphemes groups Thai base letters with their trailing combining marks (unicode.Mn)
// so 3-4 grapheme n-grams never split vowels or tone marks off their consonant.
func splitThaiGraphemes(s string) []string {
	var clusters []string
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) && len(clusters) > 0 {
			clusters[len(clusters)-1] += string(r)
		} else {
			clusters = append(clusters, string(r))
		}
	}
	return clusters
}

// tokenizeCode splits CamelCase, PascalCase, snake_case, paths, English stems,
// and Thai text (preserving unicode.Mn combining marks + generating 3-4 grapheme n-grams).
func tokenizeCode(s string) []string {
	var out []string
	var cur []rune
	emitToken := func(tok string) {
		if len(tok) < 2 || codeStopWords[tok] {
			return
		}
		out = append(out, tok)
		if stem := stemEnglishToken(tok); stem != tok && len(stem) >= 3 {
			out = append(out, stem)
		}
	}
	flush := func() {
		if len(cur) == 0 {
			return
		}
		raw := strings.ToLower(string(cur))
		cur = cur[:0]
		// Check if token contains Thai script
		hasThai := false
		for _, r := range raw {
			if isThaiRune(r) {
				hasThai = true
				break
			}
		}
		if !hasThai {
			emitToken(raw)
			return
		}
		// Keep full Thai run + emit 3- and 4-grapheme n-grams for wordless Thai matching
		clusters := splitThaiGraphemes(raw)
		if len(clusters) >= 2 {
			out = append(out, raw)
		}
		if len(clusters) >= 3 {
			for i := 0; i+3 <= len(clusters); i++ {
				out = append(out, strings.Join(clusters[i:i+3], ""))
			}
		}
		if len(clusters) >= 4 && len(clusters) <= 48 {
			for i := 0; i+4 <= len(clusters); i++ {
				out = append(out, strings.Join(clusters[i:i+4], ""))
			}
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		isWord := unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.Mn, r)
		if !isWord {
			flush()
			continue
		}
		// Split between Thai and Latin scripts cleanly
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			if isThaiRune(r) != isThaiRune(prev) && !unicode.Is(unicode.Mn, r) {
				flush()
			}
		}
		if i > 0 && unicode.IsUpper(r) && len(cur) > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func computeChunkTokenWeights(relPath, namePath, signature, snippet string) map[string]float64 {
	tf := map[string]float64{}
	addW := func(toks []string, w float64) {
		for _, t := range toks {
			tf[t] += w
		}
	}
	addW(tokenizeCode(namePath), 3.0)
	addW(tokenizeCode(relPath), 2.0)
	addW(tokenizeCode(signature), 1.8)
	addW(tokenizeCode(snippet), 1.0)
	return tf
}

// --- File Discovery Honoring .gitignore ---

func discoverIndexableFiles(repoRoot string) (files []string, skippedTooLarge int, skippedCap int) {
	maxFiles := maxIndexFilesLimit()
	maxBytes := maxIndexFileBytesLimit()
	var rels []string

	rgBin := findRipgrep()
	if rgBin != "" {
		args := []string{
			"--files", "--color", "never",
			"-g", "!.git/*", "-g", "!node_modules/*", "-g", "!graft/*", "-g", "!.serena/*",
			"-g", "!dist/*", "-g", "!.next/*", "-g", "!build/*", "-g", "!target/*",
			"-g", "!Pods/*", "-g", "!DerivedData/*", "-g", "!vendor/*", "-g", "!.expo/*",
			"-g", "!.venv/*", "-g", "!venv/*", "-g", "!__pycache__/*", "-g", "!coverage/*",
			"-g", "!.superpowers/*", "-g", "!.worktrees/*", "-g", "!.claude/*", "-g", "!.gemini/*",
			"-g", "!.codex/*", "-g", "!.grok/*", "-g", "!artifacts/*",
			repoRoot,
		}
		if out, err := exec.Command(rgBin, args...).Output(); err == nil {
			prefix := repoRoot + string(filepath.Separator)
			for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if ln == "" {
					continue
				}
				rel := filepath.ToSlash(strings.TrimPrefix(ln, prefix))
				if isIndexableCodeFile(filepath.Base(rel)) {
					rels = append(rels, rel)
				}
			}
		}
	}
	if len(rels) == 0 {
		_ = filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != repoRoot && shouldSkipIndexDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !isIndexableCodeFile(d.Name()) {
				return nil
			}
			rel, rerr := filepath.Rel(repoRoot, path)
			if rerr == nil {
				rels = append(rels, filepath.ToSlash(rel))
			}
			return nil
		})
	}

	sort.Strings(rels)
	for _, rel := range rels {
		fi, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		if fi.Size() > maxBytes {
			skippedTooLarge++
			continue
		}
		if len(files) >= maxFiles {
			skippedCap++
			continue
		}
		files = append(files, rel)
	}
	return files, skippedTooLarge, skippedCap
}

// --- Incremental V2 Build + Singleflight Concurrency Protection ---

var (
	repoBuildMu sync.Map // canonical repoRoot -> *sync.Mutex
)

func lockRepoIndexBuild(repoRoot string) func() {
	v, _ := repoBuildMu.LoadOrStore(canonicalPath(repoRoot), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func extractSidxChunksForFile(fileIdx uint32, absPath, relPath string, src []byte) []sidxBuildChunk {
	var out []sidxBuildChunk
	if grammars.DetectLanguage(absPath) != nil {
		if syms, _, err := extractFileSymbolsCached(absPath, relPath, src); err == nil {
			for _, s := range flattenSymbols(syms) {
				if int(s.EndByte) > len(src) || s.StartByte >= s.EndByte {
					continue
				}
				snip := truncateUTF8(string(src[s.StartByte:s.EndByte]), 1200)
				cm := sidxChunkMeta{
					FileIdx:   fileIdx,
					Name:      s.Name,
					NamePath:  s.NamePath,
					Kind:      s.Kind,
					KindNum:   s.KindNum,
					StartLine: s.StartLine,
					EndLine:   s.EndLine,
					StartByte: s.StartByte,
					EndByte:   s.EndByte,
					NameLine:  s.NameLine,
					NameCol:   s.NameCol,
					Signature: s.Signature,
					Hash:      computeChunkHash(relPath, s.NamePath, snip),
				}
				out = append(out, sidxBuildChunk{
					meta:   cm,
					rel:    relPath,
					tokens: computeChunkTokenWeights(relPath, s.NamePath, s.Signature, snip),
				})
			}
		}
	}
	if len(out) == 0 && len(bytes.TrimSpace(src)) > 0 {
		snip := truncateUTF8(string(src), 1200)
		lines := bytes.Count(src, []byte("\n")) + 1
		base := filepath.Base(relPath)
		cm := sidxChunkMeta{
			FileIdx:   fileIdx,
			Name:      base,
			NamePath:  base,
			Kind:      "file",
			KindNum:   symKindModule,
			StartLine: 1,
			EndLine:   lines,
			StartByte: 0,
			EndByte:   uint32(len(src)),
			NameLine:  1,
			NameCol:   0,
			Signature: firstLineTrimmed(snip),
			Hash:      computeChunkHash(relPath, base, snip),
		}
		out = append(out, sidxBuildChunk{
			meta:   cm,
			rel:    relPath,
			tokens: computeChunkTokenWeights(relPath, base, cm.Signature, snip),
		})
	}
	return out
}

func readChunkLiveSnippet(repoRoot string, h *sidxMmapHandle, chunkIdx int, maxBytes int) string {
	c := h.meta.Chunks[chunkIdx]
	if int(c.FileIdx) >= len(h.meta.Files) {
		return c.Signature
	}
	rel := h.meta.Files[c.FileIdx].RelPath
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		return c.Signature
	}
	if int(c.StartByte) >= len(raw) {
		return c.Signature
	}
	eb := int(c.EndByte)
	if eb > len(raw) || eb <= int(c.StartByte) {
		eb = len(raw)
	}
	return truncateUTF8(string(raw[c.StartByte:eb]), maxBytes)
}

// ensureFreshSidx returns an up-to-date read-only mmap handle, rebuilding incrementally only if files changed.
func ensureFreshSidx(repoRoot string) (*sidxMmapHandle, error) {
	if isForbiddenIndexRoot(repoRoot) {
		return nil, fmt.Errorf("refusing to index non-project root %q (open a git repository or pass --project <repo>)", repoRoot)
	}
	rels, skippedLarge, skippedCap := discoverIndexableFiles(repoRoot)

	// Fast check: if existing .sidx matches exact file list & mtime/size, return zero-copy mmap immediately!
	if prev, err := openSidxMmap(repoRoot); err == nil && len(prev.meta.Files) == len(rels) {
		same := true
		for i, rel := range rels {
			fm := prev.meta.Files[i]
			if fm.RelPath != rel {
				same = false
				break
			}
			fi, serr := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
			if serr != nil || fi.ModTime().UnixNano() != fm.MtimeNS || fi.Size() != fm.Size {
				same = false
				break
			}
		}
		if same {
			return prev, nil
		}
	}

	unlock := lockRepoIndexBuild(repoRoot)
	defer unlock()

	// Re-check under lock in case another goroutine just rebuilt it
	prev, prevErr := openSidxMmap(repoRoot)
	if prevErr == nil && len(prev.meta.Files) == len(rels) {
		same := true
		for i, rel := range rels {
			fm := prev.meta.Files[i]
			if fm.RelPath != rel {
				same = false
				break
			}
			fi, serr := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel)))
			if serr != nil || fi.ModTime().UnixNano() != fm.MtimeNS || fi.Size() != fm.Size {
				same = false
				break
			}
		}
		if same {
			return prev, nil
		}
	}

	// Preserve existing SQ8 vectors by chunk hash when embedModel/dim stays valid
	type oldVec struct {
		q     []int8
		scale float32
	}
	oldVecByHash := map[string]oldVec{}
	embedModel := ""
	embedDim := 0
	if prevErr == nil && prev != nil {
		embedModel = prev.meta.EmbedModel
		embedDim = prev.embedDim
		if embedDim > 0 {
			for i, c := range prev.meta.Chunks {
				if row, sc := prev.chunkVectorSQ8(i); sc > 0 && len(row) == embedDim {
					q := make([]int8, embedDim)
					for j, b := range row {
						q[j] = int8(b)
					}
					oldVecByHash[c.Hash] = oldVec{q: q, scale: sc}
				}
			}
		}
	}

	// Parallel AST chunk extraction across repo files
	fileMetas := make([]sidxFileMeta, len(rels))
	perFileChunks := make([][]sidxBuildChunk, len(rels))
	workers := runtime.NumCPU()
	if workers > 6 {
		workers = 6
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int, len(rels))
	for i := range rels {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				rel := rels[idx]
				abs := filepath.Join(repoRoot, filepath.FromSlash(rel))
				fi, err := os.Stat(abs)
				if err != nil {
					continue
				}
				src, err := os.ReadFile(abs)
				if err != nil {
					continue
				}
				fileMetas[idx] = sidxFileMeta{RelPath: rel, MtimeNS: fi.ModTime().UnixNano(), Size: fi.Size()}
				chunks := extractSidxChunksForFile(uint32(idx), abs, rel, src)
				for ci := range chunks {
					if ov, ok := oldVecByHash[chunks[ci].meta.Hash]; ok {
						chunks[ci].vecQ = ov.q
						chunks[ci].scale = ov.scale
					}
				}
				perFileChunks[idx] = chunks
			}
		}()
	}
	wg.Wait()

	var allChunks []sidxBuildChunk
	for _, list := range perFileChunks {
		allChunks = append(allChunks, list...)
	}

	meta := sidxHeaderMeta{
		RepoRoot:        canonicalPath(repoRoot),
		EmbedModel:      embedModel,
		SkippedTooLarge: skippedLarge,
		SkippedCap:      skippedCap,
		Files:           fileMetas,
	}
	if err := writeSidxFile(repoRoot, meta, allChunks, embedDim); err != nil {
		return nil, err
	}
	return openSidxMmap(repoRoot)
}

// --- OpenAI-Compatible GPU Embeddings (Base64 + Model-Aware Prefixes + 4-Worker Batch=32) ---

func resolveEmbedEndpoint() string {
	v := strings.TrimSpace(os.Getenv("SERENA_EMBED_URL"))
	if strings.EqualFold(v, "off") || v == "0" || strings.EqualFold(v, "false") {
		return ""
	}
	candidates := []string{}
	if v != "" {
		candidates = append(candidates, v)
	} else {
		cfg, _ := loadUserConfig()
		if cfgURL := strings.TrimSpace(cfg.Embeddings.URL); cfgURL != "" {
			if strings.EqualFold(cfgURL, "off") || cfgURL == "0" || strings.EqualFold(cfgURL, "false") {
				return ""
			}
			candidates = append(candidates, cfgURL)
		} else {
			candidates = append(candidates, "http://127.0.0.1:18081/v1/embeddings")
		}
	}
	for _, raw := range candidates {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		hostPort := u.Host
		if !strings.Contains(hostPort, ":") {
			if u.Scheme == "https" {
				hostPort += ":443"
			} else {
				hostPort += ":80"
			}
		}
		if tcpReachableFast(hostPort, 180*time.Millisecond) {
			return raw
		}
	}
	return ""
}

func configuredEmbedModel() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_EMBED_MODEL")); v != "" {
		return v
	}
	cfg, _ := loadUserConfig()
	if v := strings.TrimSpace(cfg.Embeddings.Model); v != "" {
		return v
	}
	return "nomic-embed-text"
}

func formatEmbedInput(model, text string, isQuery bool) string {
	low := strings.ToLower(model)
	switch {
	case strings.Contains(low, "nomic"):
		if isQuery {
			return "search_query: " + text
		}
		return "search_document: " + text
	case strings.Contains(low, "qwen"):
		if isQuery {
			return "Instruct: Given a code search query, retrieve relevant code snippets\nQuery: " + text
		}
		return text
	case strings.Contains(low, "e5"):
		if isQuery {
			return "query: " + text
		}
		return "passage: " + text
	default:
		return text
	}
}

func decodeEmbeddingItem(raw json.RawMessage) ([]float32, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty embedding")
	}
	if trimmed[0] == '"' {
		var b64 string
		if err := json.Unmarshal(trimmed, &b64); err != nil {
			return nil, err
		}
		buf, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(buf)%4 != 0 {
			return nil, errors.New("invalid base64 float32 embedding")
		}
		vec := make([]float32, len(buf)/4)
		for i := range vec {
			vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4 : i*4+4]))
		}
		return vec, nil
	}
	var vec []float32
	if err := json.Unmarshal(trimmed, &vec); err != nil {
		return nil, err
	}
	return vec, nil
}

func fetchEmbeddingsBatch(client *http.Client, endpoint, model string, inputs []string) ([][]float32, string, error) {
	payload, err := json.Marshal(map[string]any{
		"model":           model,
		"input":           inputs,
		"encoding_format": "base64",
	})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("embed HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int             `json:"index"`
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, "", err
	}
	if len(parsed.Data) != len(inputs) {
		return nil, "", fmt.Errorf("embed count mismatch: got %d want %d", len(parsed.Data), len(inputs))
	}
	out := make([][]float32, len(inputs))
	for _, item := range parsed.Data {
		if item.Index >= 0 && item.Index < len(out) {
			vec, derr := decodeEmbeddingItem(item.Embedding)
			if derr != nil {
				return nil, "", derr
			}
			out[item.Index] = vec
		}
	}
	if parsed.Model != "" {
		model = parsed.Model
	}
	return out, model, nil
}

func fetchEmbeddings(endpoint string, inputs []string) ([][]float32, string, error) {
	if len(inputs) == 0 {
		return nil, "", nil
	}
	model := configuredEmbedModel()
	client := &http.Client{Timeout: 20 * time.Second}
	if len(inputs) <= maxEmbedBatch {
		return fetchEmbeddingsBatch(client, endpoint, model, inputs)
	}

	type batchTask struct {
		start int
		end   int
	}
	var tasks []batchTask
	for s := 0; s < len(inputs); s += maxEmbedBatch {
		e := s + maxEmbedBatch
		if e > len(inputs) {
			e = len(inputs)
		}
		tasks = append(tasks, batchTask{start: s, end: e})
	}

	workers := 4
	if len(tasks) < workers {
		workers = len(tasks)
	}
	out := make([][]float32, len(inputs))
	var (
		mu            sync.Mutex
		firstErr      error
		resolvedModel = model
		wg            sync.WaitGroup
	)
	taskCh := make(chan batchTask, len(tasks))
	for _, t := range tasks {
		taskCh <- t
	}
	close(taskCh)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range taskCh {
				mu.Lock()
				failed := firstErr != nil
				mu.Unlock()
				if failed {
					return
				}
				vecs, m, err := fetchEmbeddingsBatch(client, endpoint, model, inputs[t.start:t.end])
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				if m != "" {
					resolvedModel = m
				}
				copy(out[t.start:t.end], vecs)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, "", firstErr
	}
	return out, resolvedModel, nil
}

func cosineSim(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		fa, fb := float64(a[i]), float64(b[i])
		dot += fa * fb
		na += fa * fa
		nb += fb * fb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// persistChunkEmbeddings updates the .sidx file with newly computed SQ8 vectors (invalidating old model vectors on model/dim change).
func persistChunkEmbeddings(repoRoot string, h *sidxMmapHandle, model string, dim int, newVecs map[int][]float32) error {
	unlock := lockRepoIndexBuild(repoRoot)
	defer unlock()

	// Re-open latest handle under lock
	latest, err := openSidxMmap(repoRoot)
	if err != nil {
		latest = h
	}
	invalidateOld := (latest.embedDim != 0 && latest.embedDim != dim) ||
		(latest.meta.EmbedModel != "" && model != "" && latest.meta.EmbedModel != model)

	// Reconstruct tokens from existing postings to preserve exact inverted index without re-parsing files!
	chunkTokens := make([]map[string]float64, len(latest.meta.Chunks))
	for i := range chunkTokens {
		chunkTokens[i] = map[string]float64{}
	}
	for term, plist := range latest.postings {
		for _, p := range plist {
			if int(p.ChunkIdx) < len(chunkTokens) {
				chunkTokens[p.ChunkIdx][term] = float64(p.Weight10) / 10.0
			}
		}
	}

	buildChunks := make([]sidxBuildChunk, len(latest.meta.Chunks))
	for i, cm := range latest.meta.Chunks {
		rel := ""
		if int(cm.FileIdx) < len(latest.meta.Files) {
			rel = latest.meta.Files[cm.FileIdx].RelPath
		}
		bc := sidxBuildChunk{meta: cm, rel: rel, tokens: chunkTokens[i]}
		if vec, ok := newVecs[i]; ok && len(vec) == dim {
			bc.vecQ, bc.scale = quantizeUnitSQ8(vec)
		} else if !invalidateOld && latest.embedDim == dim {
			if row, sc := latest.chunkVectorSQ8(i); sc > 0 && len(row) == dim {
				q := make([]int8, dim)
				for j, b := range row {
					q[j] = int8(b)
				}
				bc.vecQ = q
				bc.scale = sc
			}
		}
		buildChunks[i] = bc
	}
	meta := latest.meta
	if model != "" {
		meta.EmbedModel = model
	}
	return writeSidxFile(repoRoot, meta, buildChunks, dim)
}

type searchCandidate struct {
	idx      int
	bm25     float64
	vecSim   float64
	combined float64
}

func toolSemanticCodeSearch(repoRoot string, args map[string]any) (string, error) {
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return "", errors.New("query is required")
	}
	relScope := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(argString(args, "relative_path"))), "./")
	topK := argInt(args, "top_k", 8)
	if topK <= 0 {
		topK = 8
	}
	if topK > 25 {
		topK = 25
	}
	incBody := argBool(args, "include_body", true)
	maxChars := argInt(args, "max_chars", 6000)
	if maxChars <= 0 {
		maxChars = 6000
	}

	h, err := ensureFreshSidx(repoRoot)
	if err != nil {
		return "", err
	}
	numChunks := len(h.meta.Chunks)
	if numChunks == 0 {
		return "No indexable code chunks found in scope.", nil
	}

	inScope := func(chunkIdx int) bool {
		if relScope == "" || relScope == "." {
			return true
		}
		c := h.meta.Chunks[chunkIdx]
		if int(c.FileIdx) >= len(h.meta.Files) {
			return false
		}
		rel := h.meta.Files[c.FileIdx].RelPath
		return rel == relScope || strings.HasPrefix(rel, strings.TrimRight(relScope, "/")+"/")
	}

	// 1. Precomputed Inverted-Index BM25 (<2ms!)
	qTokens := tokenizeCode(query)
	if len(qTokens) == 0 {
		qTokens = strings.Fields(strings.ToLower(query))
	}
	qWeights := map[string]float64{}
	for _, qt := range qTokens {
		qWeights[qt] = 1.0
		for _, syn := range synonymLookup[qt] {
			if _, exists := qWeights[syn]; !exists {
				qWeights[syn] = 0.45
			}
		}
	}

	N := float64(numChunks)
	avgLen := h.avgDocLen
	if avgLen < 1 {
		avgLen = 1
	}
	const k1, b = 1.2, 0.75
	bm25Scores := map[int]float64{}
	for qt, qw := range qWeights {
		plist := h.postings[qt]
		if len(plist) == 0 {
			continue
		}
		df := float64(len(plist))
		idf := math.Log((N-df+0.5)/(df+0.5) + 1.0)
		for _, p := range plist {
			ci := int(p.ChunkIdx)
			if !inScope(ci) {
				continue
			}
			f := float64(p.Weight10) / 10.0
			dl := float64(h.docLens[ci])
			num := f * (k1 + 1.0)
			den := f + k1*(1.0-b+b*(dl/avgLen))
			bm25Scores[ci] += qw * idf * (num / den)
		}
	}

	// Apply gentle test-file dampener (0.55x) so production code surfaces above fuzz/unit tests
	bm25List := make([]searchCandidate, 0, len(bm25Scores))
	for ci, sc := range bm25Scores {
		c := h.meta.Chunks[ci]
		if int(c.FileIdx) < len(h.meta.Files) && isTestCodePath(h.meta.Files[c.FileIdx].RelPath) {
			sc *= 0.55
		}
		bm25List = append(bm25List, searchCandidate{idx: ci, bm25: sc, combined: sc})
	}
	sort.Slice(bm25List, func(i, j int) bool {
		if bm25List[i].bm25 != bm25List[j].bm25 {
			return bm25List[i].bm25 > bm25List[j].bm25
		}
		return bm25List[i].idx < bm25List[j].idx
	})

	modeTag := "AST-BM25 (local mmap)"
	finalList := bm25List

	// 2. Dual-Channel Full-Corpus GPU Vector + BM25 Reciprocal Rank Fusion
	if embedURL := resolveEmbedEndpoint(); embedURL != "" {
		cfgModel := configuredEmbedModel()
		modelChanged := h.meta.EmbedModel != "" && cfgModel != "" && !strings.EqualFold(h.meta.EmbedModel, cfgModel)

		// Collect up to 64 missing chunks to embed incrementally on the fly
		missingIdx := []int{}
		seenMissing := map[int]bool{}
		addMissing := func(ci int) {
			if len(missingIdx) >= 64 || seenMissing[ci] || !inScope(ci) {
				return
			}
			if modelChanged || !h.hasVector(ci) {
				seenMissing[ci] = true
				missingIdx = append(missingIdx, ci)
			}
		}
		for i := 0; i < len(bm25List) && i < 48; i++ {
			addMissing(bm25List[i].idx)
		}
		// Deterministic scan for any unembedded chunks when BM25 found few/zero hits (e.g. cross-lingual Thai -> English)
		if len(missingIdx) < 64 {
			for ci := 0; ci < numChunks && len(missingIdx) < 64; ci++ {
				addMissing(ci)
			}
		}

		toEmbed := make([]string, 0, 1+len(missingIdx))
		toEmbed = append(toEmbed, formatEmbedInput(cfgModel, query, true))
		for _, ci := range missingIdx {
			c := h.meta.Chunks[ci]
			rel := ""
			if int(c.FileIdx) < len(h.meta.Files) {
				rel = h.meta.Files[c.FileIdx].RelPath
			}
			snip := readChunkLiveSnippet(repoRoot, h, ci, 900)
			docTxt := fmt.Sprintf("%s %s %s\n%s", c.Kind, c.NamePath, rel, snip)
			toEmbed = append(toEmbed, formatEmbedInput(cfgModel, truncateUTF8(docTxt, 1000), false))
		}

		if embs, resolvedModel, eerr := fetchEmbeddings(embedURL, toEmbed); eerr == nil && len(embs) == len(toEmbed) && len(embs[0]) > 0 {
			qVec := embs[0]
			dim := len(qVec)
			dimMatches := !modelChanged && h.embedDim == dim

			var vecScores []float64
			embeddedTotal := 0
			if dimMatches {
				vecScores, embeddedTotal = h.scanAllSQ8Cosine(qVec)
			} else {
				vecScores = make([]float64, numChunks)
			}

			// Overlay newly embedded chunks immediately into vecScores
			newVecs := make(map[int][]float32, len(missingIdx))
			for k, ci := range missingIdx {
				v := embs[k+1]
				if len(v) == dim {
					newVecs[ci] = v
					vecScores[ci] = cosineSim(qVec, v)
					if !h.hasVector(ci) || !dimMatches {
						embeddedTotal++
					}
				}
			}

			// Persist newly computed vectors (sync on tiny repos/tests <2ms; async on large repos for 0 turn latency)
			if len(newVecs) > 0 || (resolvedModel != "" && h.meta.EmbedModel != resolvedModel) {
				if numChunks <= 500 {
					_ = persistChunkEmbeddings(repoRoot, h, resolvedModel, dim, newVecs)
				} else {
					go func(root string, handle *sidxMmapHandle, m string, d int, nv map[int][]float32) {
						_ = persistChunkEmbeddings(root, handle, m, d, nv)
					}(repoRoot, h, resolvedModel, dim, newVecs)
				}
			}

			// Independent Vector Ranking across ALL embedded chunks in scope
			var vecList []searchCandidate
			for ci, vs := range vecScores {
				if vs <= 0 || !inScope(ci) {
					continue
				}
				adj := vs
				c := h.meta.Chunks[ci]
				if int(c.FileIdx) < len(h.meta.Files) && isTestCodePath(h.meta.Files[c.FileIdx].RelPath) {
					adj *= 0.88
				}
				vecList = append(vecList, searchCandidate{idx: ci, vecSim: vs, combined: adj})
			}
			sort.Slice(vecList, func(i, j int) bool {
				if vecList[i].combined != vecList[j].combined {
					return vecList[i].combined > vecList[j].combined
				}
				return vecList[i].idx < vecList[j].idx
			})

			// Dual-Channel Reciprocal Rank Fusion (BM25 top-60 + Vector top-60)
			bm25Rank := map[int]int{}
			for i := 0; i < len(bm25List) && i < 60; i++ {
				bm25Rank[bm25List[i].idx] = i + 1
			}
			vecRank := map[int]int{}
			for i := 0; i < len(vecList) && i < 60; i++ {
				vecRank[vecList[i].idx] = i + 1
			}
			mergedSet := map[int]bool{}
			for ci := range bm25Rank {
				mergedSet[ci] = true
			}
			for ci := range vecRank {
				mergedSet[ci] = true
			}
			merged := make([]searchCandidate, 0, len(mergedSet))
			for ci := range mergedSet {
				rrf := 0.0
				if vr, ok := vecRank[ci]; ok {
					rrf += 1.3 / float64(60+vr)
				}
				if br, ok := bm25Rank[ci]; ok {
					rrf += 1.0 / float64(60+br)
				}
				merged = append(merged, searchCandidate{
					idx:      ci,
					bm25:     bm25Scores[ci],
					vecSim:   vecScores[ci],
					combined: rrf,
				})
			}
			sort.Slice(merged, func(i, j int) bool {
				if merged[i].combined != merged[j].combined {
					return merged[i].combined > merged[j].combined
				}
				return merged[i].idx < merged[j].idx
			})
			finalList = merged
			covPct := 0
			if numChunks > 0 {
				covPct = (embeddedTotal * 100) / numChunks
				if covPct > 100 {
					covPct = 100
				}
			}
			modeTag = fmt.Sprintf("Hybrid BM25 + GPU Vector (%s, %d%% vec)", resolvedModel, covPct)
		}
	}

	if len(finalList) == 0 {
		return fmt.Sprintf("No matching code chunks for %q (%d AST chunks indexed).", query, numChunks), nil
	}

	// Deduplicate overlapping parent/child spans in the same file so topK maximizes distinct hits
	var deduped []searchCandidate
	for _, cand := range finalList {
		c := h.meta.Chunks[cand.idx]
		overlap := false
		for _, kept := range deduped {
			kc := h.meta.Chunks[kept.idx]
			if kc.FileIdx == c.FileIdx && c.StartLine >= kc.StartLine && c.EndLine <= kc.EndLine {
				overlap = true
				break
			}
		}
		if !overlap {
			deduped = append(deduped, cand)
			if len(deduped) >= topK {
				break
			}
		}
	}

	var out strings.Builder
	skipNote := ""
	if h.meta.SkippedTooLarge > 0 || h.meta.SkippedCap > 0 {
		skipNote = fmt.Sprintf(" | skipped: %d >%dKB, %d cap", h.meta.SkippedTooLarge, maxIndexFileBytesLimit()/1024, h.meta.SkippedCap)
	}
	fmt.Fprintf(&out, "Semantic Code Search (%s | %d chunks%s | top %d):\n", modeTag, numChunks, skipNote, len(deduped))

	// Tiered disclosure: full body for top-3 hits, signature+span for ranks #4..topK (~45% token savings)
	fullBodySlots := 3
	if topK < fullBodySlots {
		fullBodySlots = topK
	}
	perBodyCap := (maxChars - 500) / fullBodySlots
	if perBodyCap < 300 {
		perBodyCap = 300
	}
	if perBodyCap > 1000 {
		perBodyCap = 1000
	}

	for i, s := range deduped {
		c := h.meta.Chunks[s.idx]
		rel := ""
		if int(c.FileIdx) < len(h.meta.Files) {
			rel = h.meta.Files[c.FileIdx].RelPath
		}
		if s.vecSim > 0 {
			fmt.Fprintf(&out, "\n%d. [%s] %s — %s:L%d-L%d (rrf=%.4f vec=%.2f)\n", i+1, c.Kind, c.NamePath, rel, c.StartLine, c.EndLine, s.combined, s.vecSim)
		} else {
			fmt.Fprintf(&out, "\n%d. [%s] %s — %s:L%d-L%d (score=%.2f)\n", i+1, c.Kind, c.NamePath, rel, c.StartLine, c.EndLine, s.bm25)
		}
		if incBody && i < fullBodySlots {
			snip := readChunkLiveSnippet(repoRoot, h, s.idx, perBodyCap)
			if len(snip) >= perBodyCap {
				snip += "\n... [truncated]"
			}
			out.WriteString("```\n")
			out.WriteString(snip)
			if !strings.HasSuffix(snip, "\n") {
				out.WriteByte('\n')
			}
			out.WriteString("```\n")
		} else {
			fmt.Fprintf(&out, "   %s\n", c.Signature)
		}
	}
	return truncateAnswer(strings.TrimSpace(out.String()), maxChars), nil
}

// runIndexCLI implements `serena-go index [--embed] [repo]` to pre-build V2 mmap + 100% corpus GPU vectors.
func runIndexCLI(args []string) int {
	doEmbed := false
	repo := ""
	for _, a := range args {
		if a == "--embed" {
			doEmbed = true
		} else if !strings.HasPrefix(a, "-") {
			repo = a
		}
	}
	if repo == "" {
		cwd, _ := os.Getwd()
		repo = cwd
	}
	repo = canonicalPath(repo)
	t0 := time.Now()
	h, err := ensureFreshSidx(repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "index error: %v\n", err)
		return 1
	}
	fmt.Printf("AST V2 mmap index ready: %s (%d files, %d chunks, skipped=%d large/%d cap) in %s\n",
		h.path, len(h.meta.Files), len(h.meta.Chunks), h.meta.SkippedTooLarge, h.meta.SkippedCap, time.Since(t0).Round(time.Millisecond))

	if !doEmbed {
		return 0
	}
	embedURL := resolveEmbedEndpoint()
	if embedURL == "" {
		fmt.Fprintln(os.Stderr, "embedding endpoint offline (set SERENA_EMBED_URL or start embedding server)")
		return 1
	}
	cfgModel := configuredEmbedModel()
	modelChanged := h.meta.EmbedModel != "" && cfgModel != "" && !strings.EqualFold(h.meta.EmbedModel, cfgModel)

	var missingIdx []int
	var inputs []string
	for ci, c := range h.meta.Chunks {
		if !modelChanged && h.hasVector(ci) {
			continue
		}
		rel := ""
		if int(c.FileIdx) < len(h.meta.Files) {
			rel = h.meta.Files[c.FileIdx].RelPath
		}
		snip := readChunkLiveSnippet(repo, h, ci, 900)
		docTxt := fmt.Sprintf("%s %s %s\n%s", c.Kind, c.NamePath, rel, snip)
		missingIdx = append(missingIdx, ci)
		inputs = append(inputs, formatEmbedInput(cfgModel, truncateUTF8(docTxt, 1000), false))
	}
	if len(missingIdx) == 0 {
		fmt.Printf("All %d chunks already embedded (model=%s, dim=%d).\n", len(h.meta.Chunks), h.meta.EmbedModel, h.embedDim)
		return 0
	}
	fmt.Printf("Embedding %d/%d chunks via %s (model=%s, 4 workers x batch=%d, base64)...\n",
		len(missingIdx), len(h.meta.Chunks), embedURL, cfgModel, maxEmbedBatch)
	t1 := time.Now()
	vecs, resolvedModel, err := fetchEmbeddings(embedURL, inputs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "embedding failed: %v\n", err)
		return 1
	}
	dim := len(vecs[0])
	newVecs := make(map[int][]float32, len(missingIdx))
	for k, ci := range missingIdx {
		newVecs[ci] = vecs[k]
	}
	if err := persistChunkEmbeddings(repo, h, resolvedModel, dim, newVecs); err != nil {
		fmt.Fprintf(os.Stderr, "persist failed: %v\n", err)
		return 1
	}
	dt := time.Since(t1)
	rate := float64(len(missingIdx)) / dt.Seconds()
	fi, _ := os.Stat(sidxPath(repo))
	szMB := float64(0)
	if fi != nil {
		szMB = float64(fi.Size()) / (1024 * 1024)
	}
	fmt.Printf("Embedded %d chunks (model=%s, dim=%d SQ8) in %s (%.1f chunks/s) -> %.2f MB mmap index\n",
		len(missingIdx), resolvedModel, dim, dt.Round(time.Millisecond), rate, szMB)
	return 0
}
