# Optional Hybrid GPU Semantic Search

Out of the box, `semantic_code_search` runs **100% locally** using a V2 shared `mmap` inverted index (`<6 ms` warm BM25, `0 MB` duplicated RAM across sessions).

To upgrade retrieval accuracy across English, multilingual queries (e.g., Thai → English code), and deep conceptual intent (**100% Corpus Hybrid BM25 + GPU Vector RRF**), run any OpenAI-compatible `/v1/embeddings` server (`llama-server`, Ollama, or vLLM) locally or via SSH loopback tunnel.

## Recommended Setup (`llama.cpp` `llama-server`)

### Option 1 — Multilingual + Code (Recommended: `Qwen3-Embedding-0.6B-Q8_0`)
Strong across English, Thai, CJK, and source code (1024-dim, ~610 MB GGUF, uses `--pooling last`):

```bash
llama-server \
  --model Qwen3-Embedding-0.6B-Q8_0.gguf \
  --alias qwen3-embedding-0.6b \
  --embeddings \
  --pooling last \
  --host 127.0.0.1 \
  --port 18081 \
  --ctx-size 8192 \
  --batch-size 8192 \
  --ubatch-size 8192 \
  -ngl 99
```

### Option 2 — Ultra-Light English-Only (`nomic-embed-text-v1.5.Q8_0`)
Tiny ~140 MB GGUF (768-dim, uses `--pooling mean`):

```bash
llama-server \
  --model nomic-embed-text-v1.5.Q8_0.gguf \
  --alias nomic-embed-text \
  --embeddings \
  --pooling mean \
  --host 127.0.0.1 \
  --port 18081 \
  --ctx-size 8192 \
  --batch-size 8192 \
  --ubatch-size 8192 \
  -ngl 99
```

## Pre-Embedding 100% of a Repository

```bash
# Forward remote GPU loopback port (if GPU is on another workstation)
ssh -N -L 127.0.0.1:18081:127.0.0.1:18081 gpu-box

# Build V2 mmap index + embed 100% of AST chunks (~250-300 chunks/s via base64 4-worker batching)
serena-go index --embed /path/to/repo
```

Vectors are stored as **full-dimension SQ8 (`int8`)** off-heap in the `.sidx` `mmap` file (`100% Recall@5` vs float32, `4×` smaller than float32), automatically invalidated whenever you switch models or dimensions, and scanned in `~2–4 ms` across the entire repository.
