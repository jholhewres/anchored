# Design

Anchored combines several memory architectures, each solving a specific problem. Most are adapted from [DevClaw](https://github.com/jholhewres/devclaw), where they were built and battle-tested in production.

---

## Memory Stack

**What it does:** Composes multiple memory layers into a single context block injected into the agent's prompt. Each layer has a priority. When the token budget is exceeded, less important layers are trimmed first.

**How it works:**

| Layer | Source | Behavior |
|---|---|---|
| L0 Identity | `~/.anchored/identity.md` | Your persistent preferences. Never trimmed, always included in full. Hot-reloaded when the file changes. |
| L1 Essential | SQLite cache (6h TTL) | Deterministic summary of top facts, decisions, events, and preferences for the current project. Template-only, zero LLM calls. Trimmed second when budget runs out. |
| L2 On-demand | FTS5 + entity detection | Detects entities and topic changes in the user's message, fetches relevant memories via full-text search. Diversified across categories. Trimmed first. |

**Why this matters:** Instead of dumping everything into the prompt, the stack selects only what's relevant for the current turn. L0 gives the agent your stable preferences, L1 grounds it in project context, and L2 surfaces specific facts, all within ~900 tokens.

**Reference:** DevClaw's `MemoryStack` (v1.19.0), inspired by hippocampal memory models and OS memory management (L1/L2 cache hierarchy).

---

## Hybrid Search

**What it does:** Merges two search approaches (semantic and lexical) into a single ranked result set, then applies entity boosting and diversification.

**How it works:**

1. **Vector search** — encodes the query into a 384-dimensional vector and finds memories with similar meaning (cosine similarity on quantized embeddings, served from an in-memory vector cache)
2. **BM25 search** — matches query keywords against an FTS5 full-text index, scoring by term frequency and rarity
3. **Entity boost** — detects project names, tools, and topics in the query via regex-based entity detection, boosting memories containing those entities by 1.1x
4. **Fusion** — combines both rankings using Weighted Reciprocal Rank Fusion: `score = 0.7 x 1/(rank_vec + 1) + 0.3 x 1/(rank_bm25 + 1)`

Post-fusion, results are re-ranked with MMR (Maximal Marginal Relevance) for diversity, boosted by project affinity, and filtered by temporal decay so stale memories rank lower.

**Why this matters:** Vector search alone misses exact matches (function names, error codes). Keyword search alone misses synonyms ("deploy" vs "ship to production"). Hybrid search catches both. Entity boosting surfaces memories about the specific project or tool you're asking about.

**Reference:** Hybrid IR is a well-established technique in information retrieval. RRF fusion was introduced by Cormack, Clarke & Buettcher (2009).

---

## Entity Detection

**What it does:** Extracts entity names (projects, tools, topics) from the user's query and uses them to boost search results and drive on-demand retrieval.

**How it works:**

1. Builds a snapshot from `projects.name` + `memories.keywords` + `memories.content` (top 500)
2. Tokens the query, filters against stop words (EN/PT/ES/FR, ~380 entries)
3. Matches remaining tokens against the entity snapshot
4. Matched entities boost results in hybrid search and trigger L2 on-demand retrieval

The snapshot is cached with a TTL and refreshed via double-checked locking to avoid cache stampede.

**Why this matters:** When you ask "how does devclaw handle auth?", entity detection knows "devclaw" is a project and boosts memories from that project, instead of treating it as a generic word.

---

## Topic Change Detection

**What it does:** Identifies when the conversation shifts to a new topic, triggering broader retrieval to surface diverse memories.

**How it works:** Compares the current query against previous queries using entity overlap. When overlap drops below a threshold, the L2 on-demand layer increases its retrieval limit and applies category diversification.

**Why this matters:** Without it, a topic shift like going from "fix the auth bug" to "update the README" would still surface mostly auth-related memories. Topic change detection resets the retrieval focus.

---

## Knowledge Graph

**What it does:** Stores structured relationships between entities (projects, services, people, APIs) as triples: subject, predicate, object. Each triple has a validity period (bitemporal tracking).

**How it works:**

- **Entities** — named things with optional aliases (e.g., "devclaw" = "the agent")
- **Triples** — relationships like `devclaw runs on Hetzner`, `API uses port 8080`
- **Functional predicates** — when a fact changes (e.g., new port), old triples are automatically invalidated
- **Temporal tracking** — each triple has `valid_from` and `valid_to`, so you can query "what was true in March 2026?"

Pattern-based extraction runs automatically, no LLM needed. The KG extractor uses regex patterns to detect dates, emails, URLs, deployments, and other structured relationships from memory text. Rate-limited to 5 triples per extraction to avoid noise.

**Why this matters:** Flat memories can't answer relational questions like "what services depend on this API?" The KG can.

**Reference:** Bitemporal knowledge graphs are used in Temporal KG research (Jiang et al., 2022). DevClaw's KG was inspired by Git's bitemporal data model (content-addressable with validity ranges).

---

## Vector Cache

**What it does:** Keeps all memory embeddings in RAM for fast similarity search without hitting the database on every query.

**How it works:** On startup, loads all embedding vectors from SQLite into a `map[string][]float32`. Lookups by memory ID are O(1) with read-write mutex protection. The cache stays in sync with the database.

**Why this matters:** Without the cache, every search would require loading thousands of vectors from SQLite. The RAM cache makes similarity search sub-millisecond.

---

## Incremental Indexer

**What it does:** Watches configured directories and incrementally indexes new or changed files into the memory store.

**How it works:**

1. Polling-based scan at configurable intervals
2. SHA-256 hash comparison to detect changes (delta sync)
3. Heading-aware markdown chunking (splits at `#` boundaries)
4. mtime-based debounce to skip recently-unchanged files
5. Indexed files tracked in SQLite for deduplication

Disabled by default, enabled via `config.yaml`.

---

## Quantized Embeddings

**What it does:** Compresses 384-dimensional vectors from float32 (4 bytes per dimension = 1536 bytes) to uint8 (1 byte per dimension = 384 bytes), a 4x memory reduction, while maintaining >=98% retrieval accuracy.

**How it works:** Uses an asymmetric estimator. Stored vectors are uint8, but queries are kept in float32. The quantization is trained to minimize cosine similarity error. A lookup table maps between uint8 values and reconstructed float32 ranges.

**Why this matters:** A typical project accumulates thousands of memories. Without quantization, the vector cache alone would consume hundreds of MB. With uint8, 10K memories use ~37MB instead of ~150MB.

---

## Content Sanitization

**What it does:** Scans every memory before storage and redacts secrets, tokens, passwords, and credentials to `[REDACTED]`.

**How it works:** Regex patterns match common secret formats: API keys (`sk-...`, `AKIA...`), Bearer tokens, passwords, private keys (`-----BEGIN ... PRIVATE KEY-----`), SSH keys, and generic credential patterns. Matched content is replaced in-place before the memory reaches the database.

**Why this matters:** Anchored imports sessions from Claude Code, OpenCode, and Cursor that may contain API keys, deploy secrets, and credentials from previous sessions. Without sanitization, importing everything would be a security risk.

---

## ONNX Local Embeddings

**What it does:** Runs text embeddings entirely on the user's machine using ONNX Runtime. No API keys, no network calls, no external dependency. The model downloads once (~470MB including runtime) and runs via CPU.

**How it works:** Text is tokenized via PreTrainedTokenizerFast (with WordPiece fallback), passed through the ONNX model, and the output hidden states are mean-pooled and L2-normalized into a 384-dimensional vector. The quantized version is stored in SQLite, and float32 vectors are used for queries.

**Why this model:** It supports 50+ languages (including Portuguese and English) with equal retrieval quality in both, essential for users who write in mixed languages. The INT8 quantized variant is only ~33MB and runs in ~12ms per embedding on a modern CPU.

**Reference:** The model is from the sentence-transformers library by Reimers & Gurevych (2019). ONNX Runtime is developed by Microsoft.
