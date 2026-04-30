# Embedding Model

## Choice: BAAI/bge-small-en-v1.5

| Property | Value |
|---|---|
| HuggingFace | [BAAI/bge-small-en-v1.5](https://huggingface.co/BAAI/bge-small-en-v1.5) |
| Parameters | 33M |
| Dimensions | 384 |
| Max sequence length | 512 tokens |
| MTEB Average | 62.17 |
| MTEB Retrieval | 51.7 |
| ONNX size (INT8) | ~33MB |
| CPU latency | ~12ms per embedding |

## Why not all-MiniLM-L6-v2?

| | all-MiniLM-L6-v2 | bge-small-en-v1.5 | Delta |
|---|---:|---:|---:|
| MTEB Avg | 56.26 | **62.17** | **+10.5%** |
| Retrieval | 42.0 | **51.7** | **+23%** |
| Dimensions | 384 | 384 | same |
| Size (INT8) | ~23MB | ~33MB | +10MB |

Same 384 dimensions means drop-in migration — no reindexing needed if changing from MiniLM.

## Why not larger models?

| Model | Dims | MTEB | Size (INT8) | Latency |
|---|---:|---:|---:|---:|
| gte-base-en-v1.5 | 768 | 64.11 | ~131MB | ~48ms |
| nomic-embed-text-v1.5 | 768 | 62.28 | ~131MB | ~48ms |
| gte-modernbert-base | 768 | 64.38 | ~149MB | ~50ms |

These offer modest quality gains (+1-2 MTEB) at 4x the size and 4x the latency. For a coding memory system where entries are short (decisions, commands, facts), 384 dims with bge-small is the optimal tradeoff.

The 768-dim models make sense if you need 8K context length to embed entire documents without chunking — a future consideration, not the default.

## Quantization

Float32 → uint8 (1 byte per dimension instead of 4):

- **4x memory reduction** (384 × 4 bytes → 384 × 1 byte = 1536 bytes per vector)
- **≥0.98 cosine similarity correlation** with float32
- **Asymmetric estimator**: queries stay float32, data in uint8

## ONNX Setup

The installer downloads from HuggingFace to `~/.anchored/data/onnx/`:

```
~/.anchored/data/onnx/
├── onnxruntime/             # ONNX Runtime library (platform-specific)
│   ├── libonnxruntime.so     # Linux
│   ├── libonnxruntime.dylib  # macOS
│   └── ...
└── bge-small-en-v1.5/
    ├── model.onnx            # ~33MB (INT8 quantized)
    ├── tokenizer.json
    ├── tokenizer_config.json
    ├── vocab.txt
    └── special_tokens_map.json
```

## Inference Pipeline

```
Input text
    │
    ▼
WordPiece tokenization (vocab.txt)
    │   Max 512 tokens, CLS/SEP tokens added
    │
    ▼
ONNX Runtime session
    │   Graph optimizations: O1
    │   Threading: intra-op parallel
    │
    ▼
Last hidden state → Mean pooling → L2 normalize
    │
    ▼
Float32 vector [384]
    │
    ▼
Quantize to uint8 (store)
    │
    ▼
Keep as float32 (query)
```

Queries use float32 for better recall. Stored data is uint8 for compression. The quantization algorithm handles the asymmetry.
