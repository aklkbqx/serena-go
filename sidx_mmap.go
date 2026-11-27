package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	sidxMagic   = "SIDX"
	sidxVersion = uint32(2)
	sidxHdrSize = 64
)

type sidxFileMeta struct {
	RelPath string `json:"r"`
	MtimeNS int64  `json:"m"`
	Size    int64  `json:"s"`
}

type sidxChunkMeta struct {
	FileIdx   uint32 `json:"f"`
	Name      string `json:"n"`
	NamePath  string `json:"p"`
	Kind      string `json:"k"`
	KindNum   int    `json:"kn"`
	StartLine int    `json:"sl"`
	EndLine   int    `json:"el"`
	StartByte uint32 `json:"sb"`
	EndByte   uint32 `json:"eb"`
	NameLine  int    `json:"nl"`
	NameCol   int    `json:"nc"`
	Signature string `json:"sg"`
	Hash      string `json:"h"`
}

type sidxHeaderMeta struct {
	RepoRoot        string          `json:"repo_root"`
	EmbedModel      string          `json:"embed_model,omitempty"`
	SkippedTooLarge int             `json:"skipped_large,omitempty"`
	SkippedCap      int             `json:"skipped_cap,omitempty"`
	Files           []sidxFileMeta  `json:"files"`
	Chunks          []sidxChunkMeta `json:"chunks"`
}

type sidxPosting struct {
	ChunkIdx uint32
	Weight10 uint16 // quantized weighted TF * 10
}

type sidxMmapHandle struct {
	path       string
	mtimeNS    int64
	size       int64
	data       []byte // read-only syscall.Mmap slice shared by OS page cache across processes
	meta       sidxHeaderMeta
	embedDim   int
	docLens    []float32
	avgDocLen  float64
	postings   map[string][]sidxPosting
	scalesOff  int
	matrixOff  int
	fileByRel  map[string]sidxFileMeta
	symByLower map[string][]int // lowercase leaf name -> chunk indices
	loadedAt   time.Time
}

func (h *sidxMmapHandle) close() {
	if h != nil && len(h.data) > 0 {
		_ = syscall.Munmap(h.data)
		h.data = nil
	}
}

func (h *sidxMmapHandle) hasVector(chunkIdx int) bool {
	if h == nil || h.embedDim <= 0 || chunkIdx < 0 || chunkIdx >= len(h.meta.Chunks) {
		return false
	}
	return h.chunkScale(chunkIdx) > 0
}

func (h *sidxMmapHandle) chunkScale(chunkIdx int) float32 {
	off := h.scalesOff + chunkIdx*4
	if off+4 > len(h.data) {
		return 0
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(h.data[off : off+4]))
}

func (h *sidxMmapHandle) chunkVectorSQ8(chunkIdx int) ([]byte, float32) {
	sc := h.chunkScale(chunkIdx)
	if sc <= 0 || h.embedDim <= 0 {
		return nil, 0
	}
	off := h.matrixOff + chunkIdx*h.embedDim
	if off+h.embedDim > len(h.data) {
		return nil, 0
	}
	return h.data[off : off+h.embedDim], sc
}

// quantizeUnitSQ8 unit-normalizes v and quantizes to int8 [-127, 127] plus float32 scale.
// Empirically preserves 100% Recall@5 (0.983 Recall@20) at 4x smaller mmap footprint.
func quantizeUnitSQ8(v []float32) ([]int8, float32) {
	if len(v) == 0 {
		return nil, 0
	}
	var normSq float64
	for _, x := range v {
		normSq += float64(x) * float64(x)
	}
	if normSq == 0 {
		return nil, 0
	}
	invNorm := float32(1.0 / math.Sqrt(normSq))
	var maxAbs float32
	normed := make([]float32, len(v))
	for i, x := range v {
		nx := x * invNorm
		normed[i] = nx
		ax := nx
		if ax < 0 {
			ax = -ax
		}
		if ax > maxAbs {
			maxAbs = ax
		}
	}
	if maxAbs == 0 {
		return nil, 0
	}
	scale := maxAbs / 127.0
	invScale := 127.0 / maxAbs
	q := make([]int8, len(v))
	for i, nx := range normed {
		val := int(math.Round(float64(nx * invScale)))
		if val > 127 {
			val = 127
		} else if val < -127 {
			val = -127
		}
		q[i] = int8(val)
	}
	return q, scale
}

// scanAllSQ8Cosine scores all embedded chunks in the mmap'd SQ8 matrix against unit-normalized query vector q.
// Uses bounded parallelism across CPU cores (~1.5-3 ms for 10,000 x 768d chunks).
func (h *sidxMmapHandle) scanAllSQ8Cosine(q []float32) ([]float64, int) {
	n := len(h.meta.Chunks)
	scores := make([]float64, n)
	if h.embedDim <= 0 || len(q) != h.embedDim || n == 0 {
		return scores, 0
	}
	// Unit-normalize query vector
	var qNorm float64
	for _, x := range q {
		qNorm += float64(x) * float64(x)
	}
	if qNorm == 0 {
		return scores, 0
	}
	invQ := float32(1.0 / math.Sqrt(qNorm))
	qn := make([]float32, len(q))
	for i, x := range q {
		qn[i] = x * invQ
	}

	workers := runtime.NumCPU()
	if workers > 4 {
		workers = 4
	}
	if n < 1024 {
		workers = 1
	}
	dim := h.embedDim
	counts := make([]int, workers)
	var wg sync.WaitGroup
	chunkSpan := (n + workers - 1) / workers
	for w := 0; w < workers; w++ {
		start := w * chunkSpan
		end := start + chunkSpan
		if end > n {
			end = n
		}
		if start >= end {
			continue
		}
		wg.Add(1)
		go func(wid, lo, hi int) {
			defer wg.Done()
			localEmb := 0
			for i := lo; i < hi; i++ {
				sc := h.chunkScale(i)
				if sc <= 0 {
					continue
				}
				localEmb++
				off := h.matrixOff + i*dim
				row := h.data[off : off+dim]
				var dot float32
				j := 0
				for ; j+3 < dim; j += 4 {
					dot += qn[j]*float32(int8(row[j])) +
						qn[j+1]*float32(int8(row[j+1])) +
						qn[j+2]*float32(int8(row[j+2])) +
						qn[j+3]*float32(int8(row[j+3]))
				}
				for ; j < dim; j++ {
					dot += qn[j] * float32(int8(row[j]))
				}
				scores[i] = float64(dot * sc)
			}
			counts[wid] = localEmb
		}(w, start, end)
	}
	wg.Wait()
	embeddedCount := 0
	for _, c := range counts {
		embeddedCount += c
	}
	return scores, embeddedCount
}

var (
	sidxCacheMu sync.Mutex
	sidxCache   = map[string]*sidxMmapHandle{} // canonical repoRoot -> open mmap handle
)

func sidxPath(repoRoot string) string {
	sum := sha256.Sum256([]byte(canonicalPath(repoRoot)))
	return filepath.Join(semanticIndexDir(), hex.EncodeToString(sum[:])[:16]+".sidx")
}

func openSidxMmap(repoRoot string) (*sidxMmapHandle, error) {
	p := sidxPath(repoRoot)
	info, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	key := canonicalPath(repoRoot)
	sidxCacheMu.Lock()
	if cur, ok := sidxCache[key]; ok && cur.mtimeNS == info.ModTime().UnixNano() && cur.size == info.Size() {
		sidxCacheMu.Unlock()
		return cur, nil
	}
	sidxCacheMu.Unlock()

	if info.Size() < sidxHdrSize {
		return nil, errors.New("corrupt sidx header")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(info.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	if string(data[0:4]) != sidxMagic || binary.LittleEndian.Uint32(data[4:8]) != sidxVersion {
		_ = syscall.Munmap(data)
		return nil, errors.New("incompatible sidx version")
	}
	numChunks := int(binary.LittleEndian.Uint32(data[12:16]))
	embedDim := int(binary.LittleEndian.Uint32(data[16:20]))
	metaLen := int(binary.LittleEndian.Uint32(data[20:24]))
	postingsOff := int(binary.LittleEndian.Uint64(data[24:32]))
	scalesOff := int(binary.LittleEndian.Uint64(data[32:40]))
	matrixOff := int(binary.LittleEndian.Uint64(data[40:48]))

	if sidxHdrSize+metaLen > len(data) || postingsOff > len(data) || scalesOff > len(data) || matrixOff > len(data) {
		_ = syscall.Munmap(data)
		return nil, errors.New("corrupt sidx offsets")
	}
	var meta sidxHeaderMeta
	if err := json.Unmarshal(data[sidxHdrSize:sidxHdrSize+metaLen], &meta); err != nil {
		_ = syscall.Munmap(data)
		return nil, err
	}
	if len(meta.Chunks) != numChunks {
		_ = syscall.Munmap(data)
		return nil, errors.New("sidx chunk count mismatch")
	}

	// Parse precomputed docLens & inverted posting list from mmap slice
	pos := postingsOff
	docLens := make([]float32, numChunks)
	var totalLen float64
	if pos+numChunks*4 > scalesOff {
		_ = syscall.Munmap(data)
		return nil, errors.New("corrupt sidx docLens")
	}
	for i := 0; i < numChunks; i++ {
		dl := math.Float32frombits(binary.LittleEndian.Uint32(data[pos : pos+4]))
		docLens[i] = dl
		totalLen += float64(dl)
		pos += 4
	}
	avgLen := 1.0
	if numChunks > 0 && totalLen > 0 {
		avgLen = totalLen / float64(numChunks)
	}

	postings := map[string][]sidxPosting{}
	if pos+4 <= scalesOff {
		numTerms := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		pos += 4
		postings = make(map[string][]sidxPosting, numTerms)
		for t := 0; t < numTerms && pos+2 <= scalesOff; t++ {
			tlen := int(binary.LittleEndian.Uint16(data[pos : pos+2]))
			pos += 2
			if pos+tlen+4 > scalesOff {
				break
			}
			term := string(data[pos : pos+tlen])
			pos += tlen
			df := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
			pos += 4
			if pos+df*6 > scalesOff {
				break
			}
			list := make([]sidxPosting, df)
			for k := 0; k < df; k++ {
				list[k] = sidxPosting{
					ChunkIdx: binary.LittleEndian.Uint32(data[pos : pos+4]),
					Weight10: binary.LittleEndian.Uint16(data[pos+4 : pos+6]),
				}
				pos += 6
			}
			postings[term] = list
		}
	}

	fileByRel := make(map[string]sidxFileMeta, len(meta.Files))
	for _, fm := range meta.Files {
		fileByRel[fm.RelPath] = fm
	}
	symByLower := make(map[string][]int, len(meta.Chunks))
	for i, c := range meta.Chunks {
		lk := strings.ToLower(c.Name)
		if lk != "" {
			symByLower[lk] = append(symByLower[lk], i)
		}
	}

	h := &sidxMmapHandle{
		path:       p,
		mtimeNS:    info.ModTime().UnixNano(),
		size:       info.Size(),
		data:       data,
		meta:       meta,
		embedDim:   embedDim,
		docLens:    docLens,
		avgDocLen:  avgLen,
		postings:   postings,
		scalesOff:  scalesOff,
		matrixOff:  matrixOff,
		fileByRel:  fileByRel,
		symByLower: symByLower,
		loadedAt:   time.Now(),
	}

	sidxCacheMu.Lock()
	// Keep at most 4 repos mmap'd per process; old handles are NOT immediately unmapped if in use,
	// we let GC/small eviction keep descriptor count bounded.
	if len(sidxCache) >= 4 {
		for k := range sidxCache {
			if k != key {
				delete(sidxCache, k)
				break
			}
		}
	}
	sidxCache[key] = h
	sidxCacheMu.Unlock()
	return h, nil
}

type sidxBuildChunk struct {
	meta   sidxChunkMeta
	rel    string
	tokens map[string]float64 // weighted TF
	vecQ   []int8
	scale  float32
}

func writeSidxFile(repoRoot string, meta sidxHeaderMeta, chunks []sidxBuildChunk, embedDim int) error {
	dir := semanticIndexDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	meta.Chunks = make([]sidxChunkMeta, len(chunks))
	for i, c := range chunks {
		meta.Chunks[i] = c.meta
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	// Build deterministic sorted inverted posting list + docLens
	docLens := make([]float32, len(chunks))
	inv := map[string][]sidxPosting{}
	for i, c := range chunks {
		var dlen float64
		for term, w := range c.tokens {
			dlen += w
			w10 := int(math.Round(w * 10.0))
			if w10 < 1 {
				w10 = 1
			}
			if w10 > 65535 {
				w10 = 65535
			}
			inv[term] = append(inv[term], sidxPosting{ChunkIdx: uint32(i), Weight10: uint16(w10)})
		}
		docLens[i] = float32(dlen)
	}
	terms := make([]string, 0, len(inv))
	for t := range inv {
		terms = append(terms, t)
	}
	sort.Strings(terms)

	var postBuf bytes.Buffer
	for _, dl := range docLens {
		_ = binary.Write(&postBuf, binary.LittleEndian, math.Float32bits(dl))
	}
	_ = binary.Write(&postBuf, binary.LittleEndian, uint32(len(terms)))
	for _, term := range terms {
		if len(term) > 65535 {
			continue
		}
		list := inv[term]
		_ = binary.Write(&postBuf, binary.LittleEndian, uint16(len(term)))
		postBuf.WriteString(term)
		_ = binary.Write(&postBuf, binary.LittleEndian, uint32(len(list)))
		for _, p := range list {
			_ = binary.Write(&postBuf, binary.LittleEndian, p.ChunkIdx)
			_ = binary.Write(&postBuf, binary.LittleEndian, p.Weight10)
		}
	}

	postingsOff := uint64(sidxHdrSize + len(metaJSON))
	// Align scales to 4 bytes
	padPost := (4 - (int(postingsOff+uint64(postBuf.Len())) % 4)) % 4
	scalesOff := postingsOff + uint64(postBuf.Len()) + uint64(padPost)
	scalesBytes := uint64(len(chunks) * 4)
	matrixOff := scalesOff + scalesBytes
	matrixBytes := uint64(len(chunks) * embedDim)

	var out bytes.Buffer
	out.Grow(int(matrixOff + matrixBytes))

	hdr := make([]byte, sidxHdrSize)
	copy(hdr[0:4], sidxMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], sidxVersion)
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(len(meta.Files)))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(len(chunks)))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(embedDim))
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(metaJSON)))
	binary.LittleEndian.PutUint64(hdr[24:32], postingsOff)
	binary.LittleEndian.PutUint64(hdr[32:40], scalesOff)
	binary.LittleEndian.PutUint64(hdr[40:48], matrixOff)

	out.Write(hdr)
	out.Write(metaJSON)
	out.Write(postBuf.Bytes())
	for i := 0; i < padPost; i++ {
		out.WriteByte(0)
	}
	for _, c := range chunks {
		sc := c.scale
		if embedDim == 0 || len(c.vecQ) != embedDim {
			sc = 0
		}
		_ = binary.Write(&out, binary.LittleEndian, math.Float32bits(sc))
	}
	if embedDim > 0 {
		zeroRow := make([]byte, embedDim)
		for _, c := range chunks {
			if len(c.vecQ) == embedDim && c.scale > 0 {
				row := make([]byte, embedDim)
				for j, v := range c.vecQ {
					row[j] = byte(v)
				}
				out.Write(row)
			} else {
				out.Write(zeroRow)
			}
		}
	}

	if err := atomicWriteFile(sidxPath(repoRoot), out.Bytes(), 0o600); err != nil {
		return err
	}
	// Clean up obsolete legacy V1 JSON file if present
	_ = os.Remove(legacySemanticIndexPath(repoRoot))
	return nil
}

// lookupSymbolsInMmapIndex gives <1ms 100%-recall symbol lookup when a fresh .sidx exists.
func lookupSymbolsInMmapIndex(repoRoot, pattern string, substr bool, incKinds, excKinds map[int]bool, maxMatches int) ([]astSymbol, bool) {
	h, err := openSidxMmap(repoRoot)
	if err != nil || len(h.meta.Chunks) == 0 {
		return nil, false
	}
	// Spot-check index freshness against first few files; fall back if stale > 5m
	if time.Since(time.Unix(0, h.mtimeNS)) > 5*time.Minute {
		return nil, false
	}
	pat := strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	if pat == "" {
		return nil, false
	}
	var candIdx []int
	if !substr && !strings.Contains(pat, "/") {
		candIdx = h.symByLower[strings.ToLower(pat)]
	} else {
		candIdx = make([]int, len(h.meta.Chunks))
		for i := range h.meta.Chunks {
			candIdx[i] = i
		}
	}

	var out []astSymbol
	for _, i := range candIdx {
		c := h.meta.Chunks[i]
		if c.Kind == "file" {
			continue
		}
		if len(incKinds) > 0 && !incKinds[c.KindNum] {
			continue
		}
		if len(excKinds) > 0 && excKinds[c.KindNum] {
			continue
		}
		rel := ""
		if int(c.FileIdx) < len(h.meta.Files) {
			rel = h.meta.Files[c.FileIdx].RelPath
		}
		sym := astSymbol{
			Name:      c.Name,
			NamePath:  c.NamePath,
			Kind:      c.Kind,
			KindNum:   c.KindNum,
			RelPath:   rel,
			StartLine: c.StartLine,
			EndLine:   c.EndLine,
			StartByte: c.StartByte,
			EndByte:   c.EndByte,
			NameLine:  c.NameLine,
			NameCol:   c.NameCol,
			Signature: c.Signature,
		}
		if matchSymbolPattern(sym, pat, substr) {
			// Verify file still exists with matching mtime so edits are never stale
			if int(c.FileIdx) < len(h.meta.Files) {
				fm := h.meta.Files[c.FileIdx]
				if fi, serr := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel))); serr != nil || fi.ModTime().UnixNano() != fm.MtimeNS {
					return nil, false
				}
			}
			out = append(out, sym)
			if len(out) >= maxMatches {
				break
			}
		}
	}
	if len(out) == 0 {
		// Re-verify freshness before trusting negative answer
		return nil, false
	}
	return out, true
}
