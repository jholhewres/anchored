# Embedding Model

## Choice: sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2

| Property | Value |
|---|---|
| HuggingFace | [sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2](https://huggingface.co/sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2) |
| Parameters | 471M |
| Dimensions | 384 |
| Max sequence length | 512 tokens |
| Languages | 50+ (including PT-BR and EN) |
| MTEB EN Retrieval | 51.7 |
| MTEB PT Retrieval | 52.4 |
| ONNX size (INT8) | ~33MB |
| CPU latency | ~12ms per embedding |

## Why multilingual instead of English-only?

Anchored users write in mixed Portuguese and English. An English-only model like bge-small-en-v1.5 degrades badly on Portuguese content:

| | EN Retrieval | PT Retrieval | Portuguese? |
|---|---:|---:|---:|
| **paraphrase-multilingual** | **51.7** | **52.4** | **Native support** |
| bge-small-en-v1.5 | 51.7 | ❌ degrades | English only |
| all-MiniLM-L6-v2 | 42.0 | ❌ degrades | English only |

The multilingual model matches bge-small in English retrieval and **outperforms it in Portuguese** (52.4 vs degrading). For mixed-language usage, this is strictly better.

## Why not all-MiniLM-L6-v2?

| | all-MiniLM-L6-v2 | paraphrase-multilingual | Delta |
|---|---:|---:|---:|
| EN Retrieval | 42.0 | **51.7** | **+23%** |
| PT Retrieval | ❌ degrades | **52.4** | **infinite** |
| Dimensions | 384 | 384 | same |
| Size (INT8) | ~23MB | ~33MB | +10MB |
| Languages | EN only | **50+** | — |

## Why not larger multilingual models?

| Model | Dims | Languages | Size (INT8) | Latency |
|---|---:|---|---:|---:|
| bge-m3 | 1024 | 100+ | **~570MB** | ~80ms |
| multilingual-e5-small | 384 | 50+ | ~33MB | ~15ms |

bge-m3 is the best multilingual model available, but at 570MB it's too large for a lightweight binary. multilingual-e5-small is a viable alternative with similar size to our choice, but paraphrase-multilingual has proven ONNX availability with optimized INT8 variants and slightly better retrieval scores.

## Quantization

Float32 → uint8 (1 byte per dimension instead of 4):

- **4x memory reduction** (384 × 4 bytes → 384 × 1 byte = 1536 bytes per vector)
- **≥0.98 cosine similarity correlation** with float32
- **Asymmetric estimator**: queries stay float32, data in uint8

## ONNX Setup

The installer downloads from HuggingFace to `~/.anchored/data/onnx/`:

```
~/.anchored/data/onnx/
├── onnxruntime/                        # ONNX Runtime library (platform-specific)
│   ├── libonnxruntime.so             # Linux
│   ├── libonnxruntime.dylib          # macOS
│   └── ...
└── paraphrase-multilingual-MiniLM-L12-v2/
    ├── model_qint8_avx2.onnx         # INT8 quantized, x86 (preferred)
    ├── model_qint8_avx512_vnni.onnx  # INT8 quantized, x86 with VNNI
    ├── model_qint8_arm64.onnx         # INT8 quantized, ARM64 (macOS)
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
WordPiece tokenization (vocab.txt, 512 max tokens)
    │   Max 512 tokens, CLS/SEP tokens added
    │
    ▼
ONNX Runtime inference (paraphrase-multilingual-MiniLM-L12-v2)
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
Quantization → uint8 (4x reduction, ≥0.98 cosine correlation)
    │
    ▼
Store in SQLite (BLOB) + in-memory vector cache
```

Queries use float32 for better recall. Stored data is uint8 for compression. The quantization algorithm handles the asymmetry.

## Future upgrade path

| Model | When it makes sense |
|---|---|
| multilingual-e5-small | If ONNX availability becomes better |
| bge-m3 | If 570MB is acceptable and 8K context is needed |
| nomic-embed-text-v1.5 | If Matryoshka truncation (768→256) is needed |
