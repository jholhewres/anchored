# Design

## Anchored combines several memory architectures, each solving a specific problem. Most are adapted from [DevClaw](https://github.com/jholhewres/devclaw), where they were built and battle-tested in production.

---

## Memory Stack

**What it does:** Composes multiple memory layers into a single context block injected into the agent's prompt. Each layer has a priority — when the token budget is exceeded, less important layers are trimmed first.

**How it works:**

| Layer | Source | Behavior |
|---|---|---|
| L0 Identity | `~/.anchored/identity.md` | Your persistent preferences. Never trimmed — always included in full. Hot-reloaded when the file changes. |
| L1 Project | SQLite cache (6h TTL) | A short summary of the most relevant knowledge for the current project. Deterministic template, no LLM calls. Trimmed second when budget runs out. |
| L2 On-demand | Regex + SQL lookup | Detects entities (project names, topics) in your current message and fetches relevant memories from the knowledge base. Trimmed first. |

**Why this matters:** Instead of dumping everything into the prompt, the stack selects only what's relevant for the current turn. L0 gives the agent your stable preferences, L1 grounds it in project context, and L2 surfaces specific facts — all within ~900 tokens.

**Reference:** DevClaw's `MemoryStack` (v1.19.0), inspired by hippocampal memory models and OS memory management (L1/L2 cache hierarchy).

---

## Hybrid Search

**What it does:** Merges two search approaches — semantic (understands meaning) and lexical (matches exact words) — into a single ranked result set.

**How it works:**

1. **Vector search** — encodes the query into a 384-dimensional vector and finds memories with similar meaning (cosine similarity on quantized embeddings)
2. **BM25 search** — matches query keywords against an FTS5 full-text index, scoring by term frequency and rarity
3. **Fusion** — combines both rankings using Weighted Reciprocal Rank Fusion (RRF): `score = 0.7 × 1/(rank_vec + 1) + 0.3 × 1/(rank_bm25 + 1)`

Post-fusion, results are re-ranked with MMR (Maximal Marginal Relevance) for diversity and filtered by temporal decay so stale memories rank lower.

**Why this matters:** Vector search alone misses exact matches (function names, error codes). Keyword search alone misses synonyms ("deploy" vs "ship to production"). Hybrid search catches both.

**Reference:** Hybrid IR is a well-established technique in information retrieval. RRF fusion was introduced by Cormack, Clarke & Buettcher (2009). DevClaw's implementation adds wing-aware boosting and MMR re-ranking.

---

## Knowledge Graph

**What it does:** Stores structured relationships between entities (projects, services, people, APIs) as triples: subject → predicate → object. Each triple has a validity period (bitemporal tracking).

**How it works:**

- **Entities** — named things with optional aliases (e.g., "devclaw" = "the agent")
- **Triples** — relationships like `devclaw — runs on → Hetzner`, `API — uses → port 8080`
- **Functional predicates** — when a fact changes (e.g., new port), old triples are automatically invalidated
- **Temporal tracking** — each triple has `valid_from` and `valid_to`, so you can query "what was true in March 2026?"

Pattern-based extraction runs automatically — no LLM needed for basic relationships. Future versions may add LLM-based extraction (opt-in).

**Why it matters:** Flat memories can't answer relational questions like "what services depend on this API?" The KG can.

**Reference:** Bitemporal knowledge graphs are used in Temporal KG research (Jiang et al., 2022). DevClaw's KG was inspired by Git's bitemporal data model (content-addressable with validity ranges).

---

## Quantized Embeddings

**What it does:** Compresses 384-dimensional vectors from float32 (4 bytes per dimension = 1536 bytes) to uint8 (1 byte per dimension = 384 bytes) — a 4x memory reduction — while maintaining ≥98% retrieval accuracy.

**How it works:** Uses an asymmetric estimator — stored vectors are uint8, but queries are kept in float32. The quantization is trained to minimize cosine similarity error. A lookup table maps between uint8 values and reconstructed float32 ranges.

**Why this matters:** A typical project accumulates thousands of memories. Without quantization, the vector cache alone would consume hundreds of MB. With uint8, 10K memories use ~37MB instead of ~150MB.

**Reference:** Quantized embeddings (often called product quantization) were popularized by Douze et al. (2021) and Zhu et al. (2023). The asymmetric estimator approach was refined by Stock et al. (2023) in the Sim-DQ framework.

---

## Content Sanitization

**What it does:** Scans every memory before storage and redacts secrets, tokens, passwords, and credentials to `[REDACTED]`. The agent is instructed to never save secrets, and the sanitizer provides a safety net.

**How it works:** Regex patterns match common secret formats — API keys (`sk-...`, `AKIA...`), Bearer tokens, passwords, private keys (`-----BEGIN ... PRIVATE KEY-----`), SSH keys, and generic credential patterns. Matched content is replaced in-place before the memory reaches the database.

**Why it matters:** Anchored imports sessions from Claude Code, OpenCode, and Cursor that may contain API keys, deploy secrets, and credentials from previous sessions. Without sanitization, importing everything would be a security risk.

---

## ONNX Local Embeddings

**What it does:** Runs text embeddings entirely on the user's machine using ONNX Runtime — no API keys, no network calls, no external dependency. The model (`paraphrase-multilingual-MiniLM-L12-v2`) downloads once (~33MB) and runs via CPU.

**How it works:** Text is tokenized with WordPiece, passed through the ONNX model, and the output hidden states are mean-pooled and L2-normalized into a 384-dimensional vector. The quantized version is stored in SQLite, and float32 vectors are used for queries.

**Why this model:** It supports 50+ languages (including Portuguese and English) with equal retrieval quality in both — essential for users who write in mixed languages. The INT8 quantized variant is only ~33MB and runs in ~12ms per embedding on a modern CPU.

**Reference:** The model is from the sentence-transformers library by Reimers & Gurevych (2019). ONNX Runtime is developed by Microsoft. DevClaw's ONNX integration (`embeddings_onnx.go`) handles automatic download, extraction, and session management.
