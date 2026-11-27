# Optional Hybrid GPU Semantic Search

Out of the box, `semantic_code_search` runs **100% locally** using AST-chunked BM25 (`0 MB` extra RAM, `<50 ms`).

To upgrade search accuracy with dense vector embeddings (**Hybrid BM25 + GPU Vector RRF**), run any OpenAI-compatible `/v1/embeddings` server (such as `llama-server`, Ollama, or vLLM) locally or via SSH tunnel.

## Recommended Setup (`llama.cpp` `llama-server` + `nomic-embed-text-v1.5`)

1. Download `nomic-embed-text-v1.5.Q8_0.gguf` (~140 MB, 768-dim).
2. Start `llama-server` with GPU offload:

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

3. If running on a remote GPU workstation, forward only loopback over SSH:

```bash
ssh -N -L 127.0.0.1:18081:127.0.0.1:18081 gpu-box
```

`serena-go` automatically detects `http://127.0.0.1:18081/v1/embeddings` (or custom `SERENA_EMBED_URL` / `config.json`), caches chunk embeddings per AST content hash on disk, and falls back to local AST-BM25 in `<180 ms` if the server is stopped.
