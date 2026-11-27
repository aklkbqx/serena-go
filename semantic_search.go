package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/odvcencio/gotreesitter/grammars"
)

const (
	semanticIndexVersion = 1
	maxIndexFiles        = 2000
	maxIndexFileBytes    = 200 * 1024
)

type indexedChunk struct {
	RelPath   string    `json:"rel_path"`
	NamePath  string    `json:"name_path"`
	Kind      string    `json:"kind"`
	StartLine int       `json:"start_line"`
	EndLine   int       `json:"end_line"`
	Signature string    `json:"signature"`
	Snippet   string    `json:"snippet"`
	Hash      string    `json:"hash"`
	Embedding []float32 `json:"embedding,omitempty"`
}

type indexedFile struct {
	MtimeNS int64          `json:"mtime_ns"`
	Size    int64          `json:"size"`
	Chunks  []indexedChunk `json:"chunks"`
}

type repoSemanticIndex struct {
	Version    int                    `json:"version"`
	RepoRoot   string                 `json:"repo_root"`
	EmbedModel string                 `json:"embed_model,omitempty"`
	Files      map[string]indexedFile `json:"files"`
}

func semanticIndexDir() string {
	if v := strings.TrimSpace(os.Getenv("SERENA_INDEX_DIR")); v != "" {
		return v
	}
	if ucd, err := os.UserCacheDir(); err == nil && ucd != "" {
		return filepath.Join(ucd, "serena-index")
	}
	return fmt.Sprintf("/tmp/serena-index-%d", os.Getuid())
}

func semanticIndexPath(repoRoot string) string {
	sum := sha256.Sum256([]byte(canonicalPath(repoRoot)))
	return filepath.Join(semanticIndexDir(), hex.EncodeToString(sum[:])[:16]+".json")
}

func loadSemanticIndex(repoRoot string) *repoSemanticIndex {
	p := semanticIndexPath(repoRoot)
	raw, err := os.ReadFile(p)
	if err != nil {
		return &repoSemanticIndex{Version: semanticIndexVersion, RepoRoot: canonicalPath(repoRoot), Files: map[string]indexedFile{}}
	}
	var idx repoSemanticIndex
	if err := json.Unmarshal(raw, &idx); err != nil || idx.Version != semanticIndexVersion {
		return &repoSemanticIndex{Version: semanticIndexVersion, RepoRoot: canonicalPath(repoRoot), Files: map[string]indexedFile{}}
	}
	if idx.Files == nil {
		idx.Files = map[string]indexedFile{}
	}
	return &idx
}

func saveSemanticIndex(repoRoot string, idx *repoSemanticIndex) {
	dir := semanticIndexDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		return
	}
	_ = atomicWriteFile(semanticIndexPath(repoRoot), raw, 0o600)
}

func shouldSkipIndexDir(name string) bool {
	switch name {
	case ".git", "node_modules", "graft", "dist", ".next", "build", "target", "Pods",
		"DerivedData", "vendor", ".expo", ".venv", "venv", "__pycache__", ".cache",
		".turbo", ".serena", "coverage", ".gradle", ".idea", ".vscode",
		".superpowers", ".worktrees", ".claude", ".gemini":
		return true
	}
	return false
}

func isIndexableCodeFile(name string) bool {
	if strings.HasSuffix(name, ".d.ts") || strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".min.css") || strings.HasSuffix(name, ".lock") {
		return false
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts",
		".rs", ".py", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".m", ".mm",
		".java", ".kt", ".sql", ".sh", ".zsh", ".rb", ".php", ".vue", ".svelte":
		return true
	}
	return false
}

func chunkHash(c indexedChunk) string {
	sum := sha256.Sum256([]byte(c.RelPath + "\n" + c.NamePath + "\n" + c.Snippet))
	return hex.EncodeToString(sum[:])[:16]
}

func extractChunksForFile(absPath, relPath string, src []byte) []indexedChunk {
	var chunks []indexedChunk
	if grammars.DetectLanguage(absPath) != nil {
		if syms, _, err := extractFileSymbols(absPath, relPath, src); err == nil {
			for _, s := range flattenSymbols(syms) {
				if int(s.EndByte) > len(src) || s.StartByte >= s.EndByte {
					continue
				}
				snip := string(src[s.StartByte:s.EndByte])
				if len(snip) > 1200 {
					snip = snip[:1200]
				}
				c := indexedChunk{
					RelPath:   relPath,
					NamePath:  s.NamePath,
					Kind:      s.Kind,
					StartLine: s.StartLine,
					EndLine:   s.EndLine,
					Signature: s.Signature,
					Snippet:   snip,
				}
				c.Hash = chunkHash(c)
				chunks = append(chunks, c)
			}
		}
	}
	// File-level fallback for files without named AST symbols
	if len(chunks) == 0 && len(bytes.TrimSpace(src)) > 0 {
		snip := string(src)
		if len(snip) > 1200 {
			snip = snip[:1200]
		}
		lines := bytes.Count(src, []byte("\n")) + 1
		c := indexedChunk{
			RelPath:   relPath,
			NamePath:  filepath.Base(relPath),
			Kind:      "file",
			StartLine: 1,
			EndLine:   lines,
			Signature: firstLineTrimmed(snip),
			Snippet:   snip,
		}
		c.Hash = chunkHash(c)
		chunks = append(chunks, c)
	}
	return chunks
}

func refreshSemanticIndex(repoRoot string) (*repoSemanticIndex, bool) {
	idx := loadSemanticIndex(repoRoot)
	seen := map[string]bool{}
	dirty := false
	fileCount := 0

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
		if fileCount >= maxIndexFiles {
			return filepath.SkipAll
		}
		if !isIndexableCodeFile(d.Name()) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() == 0 || info.Size() > maxIndexFileBytes {
			return nil
		}
		rel, rerr := filepath.Rel(repoRoot, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		fileCount++

		mtime := info.ModTime().UnixNano()
		if prev, ok := idx.Files[rel]; ok && prev.MtimeNS == mtime && prev.Size == info.Size() {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		newChunks := extractChunksForFile(path, rel, src)
		// Preserve cached embeddings for unchanged chunk hashes
		if prev, ok := idx.Files[rel]; ok {
			oldEmb := map[string][]float32{}
			for _, oc := range prev.Chunks {
				if len(oc.Embedding) > 0 {
					oldEmb[oc.Hash] = oc.Embedding
				}
			}
			for i := range newChunks {
				if emb, ok := oldEmb[newChunks[i].Hash]; ok {
					newChunks[i].Embedding = emb
				}
			}
		}
		idx.Files[rel] = indexedFile{MtimeNS: mtime, Size: info.Size(), Chunks: newChunks}
		dirty = true
		return nil
	})

	for rel := range idx.Files {
		if !seen[rel] {
			delete(idx.Files, rel)
			dirty = true
		}
	}
	return idx, dirty
}

// --- Code-Aware Tokenizer + Engineering Domain Synonyms + BM25 ---

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

// tokenizeCode splits CamelCase, PascalCase, snake_case, paths, and identifiers into normalized tokens.
func tokenizeCode(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		tok := strings.ToLower(string(cur))
		cur = cur[:0]
		if len(tok) >= 2 && !codeStopWords[tok] {
			out = append(out, tok)
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
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

type scoredChunk struct {
	chunk    *indexedChunk
	bm25     float64
	vecSim   float64
	combined float64
}

// --- Optional OpenAI-Compatible GPU Embeddings ---

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
			// Safe loopback-only probe (no unexpected LAN scans by default)
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

const maxEmbedBatch = 8

func fetchEmbeddingsBatch(client *http.Client, endpoint, model string, inputs []string) ([][]float32, string, error) {
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"input": inputs,
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
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
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
			out[item.Index] = item.Embedding
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
	model := strings.TrimSpace(os.Getenv("SERENA_EMBED_MODEL"))
	if model == "" {
		cfg, _ := loadUserConfig()
		model = strings.TrimSpace(cfg.Embeddings.Model)
	}
	if model == "" {
		model = "nomic-embed-text"
	}
	client := &http.Client{Timeout: 12 * time.Second}
	out := make([][]float32, 0, len(inputs))
	resolvedModel := model
	for start := 0; start < len(inputs); start += maxEmbedBatch {
		end := start + maxEmbedBatch
		if end > len(inputs) {
			end = len(inputs)
		}
		batch, m, err := fetchEmbeddingsBatch(client, endpoint, model, inputs[start:end])
		if err != nil {
			return nil, "", err
		}
		if m != "" {
			resolvedModel = m
		}
		out = append(out, batch...)
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

	idx, dirty := refreshSemanticIndex(repoRoot)

	// Flatten chunks within scope
	var chunks []*indexedChunk
	for rel, f := range idx.Files {
		if relScope != "" && relScope != "." && rel != relScope && !strings.HasPrefix(rel, strings.TrimRight(relScope, "/")+"/") {
			continue
		}
		for i := range f.Chunks {
			chunks = append(chunks, &f.Chunks[i])
		}
	}
	if len(chunks) == 0 {
		if dirty {
			saveSemanticIndex(repoRoot, idx)
		}
		return "No indexable code chunks found in scope.", nil
	}

	// Build query term weights (exact query tokens = 1.0, domain synonyms = 0.45)
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

	// Compute weighted term frequencies & document frequencies for BM25
	docFreq := map[string]int{}
	docLens := make([]float64, len(chunks))
	docTFs := make([]map[string]float64, len(chunks))
	totalLen := 0.0

	for i, c := range chunks {
		tf := map[string]float64{}
		addWeighted := func(toks []string, w float64) {
			for _, t := range toks {
				tf[t] += w
			}
		}
		addWeighted(tokenizeCode(c.NamePath), 3.0)
		addWeighted(tokenizeCode(c.RelPath), 2.0)
		addWeighted(tokenizeCode(c.Signature), 1.8)
		addWeighted(tokenizeCode(c.Snippet), 1.0)
		docTFs[i] = tf
		dlen := 0.0
		for t, f := range tf {
			dlen += f
			if _, inQ := qWeights[t]; inQ {
				docFreq[t]++
			}
		}
		docLens[i] = dlen
		totalLen += dlen
	}
	avgLen := totalLen / float64(len(chunks))
	if avgLen < 1 {
		avgLen = 1
	}
	N := float64(len(chunks))
	const k1, b = 1.2, 0.75

	scored := make([]scoredChunk, 0, len(chunks))
	for i, c := range chunks {
		score := 0.0
		tf := docTFs[i]
		dl := docLens[i]
		for qt, qw := range qWeights {
			f := tf[qt]
			if f <= 0 {
				continue
			}
			df := float64(docFreq[qt])
			idf := math.Log((N-df+0.5)/(df+0.5) + 1.0)
			num := f * (k1 + 1.0)
			den := f + k1*(1.0-b+b*(dl/avgLen))
			score += qw * idf * (num / den)
		}
		if score > 0 {
			scored = append(scored, scoredChunk{chunk: c, bm25: score, combined: score})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].bm25 != scored[j].bm25 {
			return scored[i].bm25 > scored[j].bm25
		}
		return scored[i].chunk.RelPath < scored[j].chunk.RelPath
	})

	modeTag := "AST-BM25 (local)"
	// Optional GPU Dense Vector Reciprocal Rank Fusion when Windows RTX 3070 Ti embedding endpoint is live!
	if embedURL := resolveEmbedEndpoint(); embedURL != "" {
		// Candidate pool: top 40 BM25 hits (or first 40 chunks if BM25 missed vocabulary completely)
		pool := scored
		if len(pool) == 0 {
			limit := len(chunks)
			if limit > 40 {
				limit = 40
			}
			pool = make([]scoredChunk, limit)
			for i := 0; i < limit; i++ {
				pool[i] = scoredChunk{chunk: chunks[i]}
			}
		} else if len(pool) > 40 {
			pool = pool[:40]
		}

		// Embed query + any pool chunks missing cached embeddings (chunked safely for GPU ubatch)
		var toEmbed []string
		toEmbed = append(toEmbed, "search_query: "+query)
		missingIdx := []int{}
		for i := range pool {
			if len(pool[i].chunk.Embedding) == 0 {
				text := fmt.Sprintf("search_document: %s %s %s\n%s", pool[i].chunk.Kind, pool[i].chunk.NamePath, pool[i].chunk.RelPath, pool[i].chunk.Snippet)
				if len(text) > 1000 {
					text = text[:1000]
				}
				toEmbed = append(toEmbed, text)
				missingIdx = append(missingIdx, i)
			}
		}
		if embs, model, err := fetchEmbeddings(embedURL, toEmbed); err == nil && len(embs) == len(toEmbed) && len(embs[0]) > 0 {
			qVec := embs[0]
			for k, idxInPool := range missingIdx {
				pool[idxInPool].chunk.Embedding = embs[k+1]
				dirty = true
			}
			if model != "" && idx.EmbedModel != model {
				idx.EmbedModel = model
				dirty = true
			}
			// Compute cosine similarities & vector ranks
			vecOrder := make([]int, len(pool))
			for i := range pool {
				pool[i].vecSim = cosineSim(qVec, pool[i].chunk.Embedding)
				vecOrder[i] = i
			}
			sort.Slice(vecOrder, func(i, j int) bool {
				return pool[vecOrder[i]].vecSim > pool[vecOrder[j]].vecSim
			})
			vecRank := make([]int, len(pool))
			for rank, idxInPool := range vecOrder {
				vecRank[idxInPool] = rank + 1
			}
			// Reciprocal Rank Fusion (RRF): blend BM25 rank + GPU vector rank
			for i := range pool {
				rrf := 1.2 / float64(60+vecRank[i])
				if pool[i].bm25 > 0 {
					rrf += 1.0 / float64(60+i+1)
				}
				pool[i].combined = rrf
			}
			sort.Slice(pool, func(i, j int) bool {
				return pool[i].combined > pool[j].combined
			})
			scored = pool
			modeTag = fmt.Sprintf("Hybrid BM25 + GPU Vector (%s)", model)
		}
	}

	if dirty {
		saveSemanticIndex(repoRoot, idx)
	}

	if len(scored) == 0 {
		return fmt.Sprintf("No matching code chunks for %q (%d AST chunks indexed).", query, len(chunks)), nil
	}
	if len(scored) > topK {
		scored = scored[:topK]
	}

	var out strings.Builder
	fmt.Fprintf(&out, "Semantic Code Search (%s | %d chunks indexed | top %d):\n", modeTag, len(chunks), len(scored))
	perBodyCap := (maxChars - 400) / len(scored)
	if perBodyCap < 220 {
		perBodyCap = 220
	}
	if perBodyCap > 900 {
		perBodyCap = 900
	}
	for i, s := range scored {
		c := s.chunk
		if s.vecSim > 0 {
			fmt.Fprintf(&out, "\n%d. [%s] %s — %s:L%d-L%d (rrf=%.4f vec=%.2f)\n", i+1, c.Kind, c.NamePath, c.RelPath, c.StartLine, c.EndLine, s.combined, s.vecSim)
		} else {
			fmt.Fprintf(&out, "\n%d. [%s] %s — %s:L%d-L%d (score=%.2f)\n", i+1, c.Kind, c.NamePath, c.RelPath, c.StartLine, c.EndLine, s.bm25)
		}
		if incBody {
			snip := c.Snippet
			if len(snip) > perBodyCap {
				snip = snip[:perBodyCap] + "\n... [truncated]"
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
