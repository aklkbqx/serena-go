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
  --ctx-size 4096 \
  --batch-size 2048 \
  --ubatch-size 2048 \
  --parallel 4 \
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

## Zero-Touch Auto-Start, Idle Unload & Auto-Indexing

Configure `~/.config/serena-go/config.json` so you never need to start the GPU server or run `index --embed` manually:

```json
{
  "embeddings": {
    "url": "http://127.0.0.1:18081/v1/embeddings",
    "model": "qwen3-embedding-0.6b",
    "auto_start_cmd": "/path/to/start-embed-gpu.sh --bg --auto",
    "auto_index_bg": true,
    "activity_file": "/tmp/serena-embed.act"
  }
}
```

- **`auto_start_cmd`**: Triggered automatically (at most once per 60s) on the first `semantic_code_search` when the endpoint is offline; waits up to 4.5s for HTTP `/health` readiness and falls back immediately to local BM25 if the helper exits without starting the GPU.
- **`activity_file`**: Touched on every embedding call so an external idle watchdog can unload GPU VRAM after inactivity.
- **`auto_index_bg`** (default `true`): Whenever a repo has `<100%` vector coverage while the embedding endpoint is online, spawns a singleflight-locked background worker (`serena-go index --embed --bg-auto`) using gentle concurrency (`2 workers × batch=16`).
