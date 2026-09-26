#!/usr/bin/env python3
"""Generate the tokenizer golden set used by TestTokenizerGolden.

The reference is the HuggingFace `tokenizers` library reading the same
tokenizer.json the ONNX embedder loads. Every sentence is synthetic: the eval
fixture's texts, Unicode edge cases, and seeded combinations of PT/EN/ES
phrases, so the committed output holds no user data.

    python3 -m venv /tmp/tok && /tmp/tok/bin/pip install tokenizers
    /tmp/tok/bin/python gen.py ~/.anchored/data/onnx/paraphrase-multilingual-MiniLM-L12-v2/tokenizer.json

Writes golden.jsonl next to this file: a meta line, then one {"t", "ids"} per
sentence. Re-run it whenever the corpus or the tokenizer changes.
"""
import hashlib
import json
import os
import random
import re
import sys
import unicodedata

import tokenizers
from tokenizers import Tokenizer

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURE = os.path.join(HERE, "..", "..", "..", "eval", "fixtures", "recall_real.yaml")


def fixture_texts():
    """Pull the quoted content/query strings out of the eval fixture (flow-style
    YAML maps, one per line) without a YAML dependency."""
    pattern = re.compile(r'\b(?:content|query): "((?:[^"\\]|\\.)*)"')
    out = []
    with open(FIXTURE, encoding="utf-8") as f:
        for line in f:
            out.extend(json.loads('"' + m + '"') for m in pattern.findall(line))
    return out


EDGE = [
    # accents, composed and decomposed
    "ação, coração, informação", unicodedata.normalize("NFD", "ação, coração, informação"),
    "Ñandú, pingüino, acción", unicodedata.normalize("NFD", "Ñandú, pingüino, acción"),
    "ÀÉÎÕÜ àéîõü Ç ç", "façade naïve résumé coöperate",
    "e\u0301 a\u0303 o\u0302 n\u0303", "Z\u0335\u0321a\u0336lgo text",
    # case
    "DEPLOY THE SERVICE NOW", "iPhone macOS GitHub PostgreSQL", "ÉPOCA ÁRVORE ÓRGÃO",
    # whitespace
    "tab\tseparated\tvalues", "line one\nline two\r\nline three", "  leading and trailing  ",
    "many     spaces    between", "non\u00a0breaking\u00a0space", "ideographic\u3000space",
    "thin\u2009space and em\u2003space", "zero\u200bwidth\u200bspace", "soft\u00adhyphen",
    "word\u2028separator\u2029para", "\u200d\u200c joiners", "\ufeffbom at start",
    # width, compatibility, ligatures
    "ＦＵＬＬＷＩＤＴＨ ｔｅｘｔ １２３", "ｶﾀｶﾅ halfwidth", "ﬁnance ﬂow ﬀ ﬃ", "x² y³ H₂O",
    "½ ¼ ¾ ⅓", "Ⅻ ⅳ ⑴ ① ㈱ ㎏ ℃ ™ ℡", "ℌ𝔢𝔩𝔩𝔬 𝐛𝐨𝐥𝐝 𝕕𝕠𝕦𝕓𝕝𝕖",
    # scripts
    "東京で会議があります", "中文分词测试，标点符号。", "한국어 형태소 분석기", "ㄱㄴㄷ ᄀ ᅡ ᆨ",
    "مرحبا بالعالم", "שלום עולם", "Привет, мир! Ёжик", "Γειά σου Κόσμε", "नमस्ते दुनिया",
    "สวัสดีชาวโลก", "Xin chào thế giới", "Türkçe İstanbul ıi", "straße STRASSE ß ẞ",
    "ქართული ენა", "Հայերեն", "አማርኛ", "ᏣᎳᎩ",
    # emoji
    "deploy ok 🚀🔥", "👍🏽 skin tone", "👨‍👩‍👧‍👦 family zwj", "🇧🇷🇺🇸 flags", "❤️ vs ❤",
    "1️⃣ keycap", "🏳️‍🌈 flag", "emoji😀sem😀espaço",
    # punctuation and symbols
    "«aspas» “curly” ‘single’ „low“", "—em dash– en dash - hyphen", "…ellipsis...", "¿Qué? ¡Sí!",
    "a/b\\c|d", "~!@#$%^&*()_+`-={}[]:\";'<>?,./", "€100 R$ 50,00 US$1,234.56 ¥500 £3",
    "±×÷≠≤≥∞∑√∫", "→ ← ↑ ↓ ⇒ ⇔", "•·°§¶†‡",
    # code and technical text
    "func (s *Service) Search(ctx context.Context, q string) ([]Hit, error) {",
    "SELECT id, content FROM memories WHERE project_id = ? AND deleted_at IS NULL;",
    "git rebase -i HEAD~3 && git push --force-with-lease origin feat/x",
    "https://example.com/path/to/page?query=value&x=1#fragment",
    "/home/user/.config/app/config.yaml", "C:\\Users\\name\\AppData\\Local",
    '{"key": "value", "n": 42, "ok": true, "list": [1, 2, 3]}',
    "snake_case camelCase PascalCase kebab-case SCREAMING_CASE",
    "v0.20.0-rc.1+build.5", "192.168.0.1:8080", "2026-09-25T18:52:05Z",
    "user@example.com", "#hashtag @mention", "sha256:3f5d2d858bde81ba62639ff7c97f9ab4",
    "x=1;y=2;z=x+y", "a->b a=>b a::b a..b a...b", "<div class=\"x\">hi</div>",
    # special tokens as literal text
    "<s> literal start", "end </s>", "a <mask> token", "<unk> and <pad>", "<s></s><mask>",
    # numbers
    "1234567890", "3.14159 2,71828", "1e-9 6.02e23", "007 0x1F 0b1010", "100% 50‰",
    # long words and long text
    "a" * 300, "supercalifragilisticexpialidocious" * 5, "x" * 20 + " " + "y" * 250,
    "Pneumoultramicroscopicossilicovulcanoconiótico", "https://example.com/" + "segment/" * 40,
    " ".join(["palavra"] * 200), "memória " * 150,
    # degenerate
    "", " ", "\t\n", ".", "a", "1", "🚀", "\u0301", "\u200b", "\x00nul\x01ctl\x7f",
    "a\u0000b", "\ud7ff\ue000", "\U0010ffff",
]

PT = ["o deploy", "a migração do banco", "o serviço de pagamentos", "a fila de mensagens",
      "o cache", "a busca híbrida", "o índice", "a memória", "o usuário", "a sessão",
      "o token de acesso", "a configuração", "o dashboard", "a release", "o teste"]
EN = ["the deploy", "the database migration", "the payment service", "the message queue",
      "the cache", "hybrid search", "the index", "memory", "the user", "the session",
      "the access token", "the config", "the dashboard", "the release", "the test"]
ES = ["el despliegue", "la migración de la base", "el servicio de pagos", "la cola de mensajes",
      "la caché", "la búsqueda híbrida", "el índice", "la memoria", "el usuario", "la sesión",
      "el token de acceso", "la configuración", "el panel", "la versión", "la prueba"]
VERBS_PT = ["falhou em produção", "ficou lento", "precisa de rollback", "foi corrigido",
            "quebra com NULL", "roda às 3h", "estourou o timeout", "usa Postgres 16"]
VERBS_EN = ["failed in production", "got slow", "needs a rollback", "was fixed",
            "breaks on NULL", "runs at 3am", "hit the timeout", "uses Postgres 16"]
VERBS_ES = ["falló en producción", "se volvió lento", "necesita rollback", "fue corregido",
            "rompe con NULL", "corre a las 3h", "superó el timeout", "usa Postgres 16"]
TAILS = ["", ".", "!", "?", " — ver LED-54711", " (v0.19.2)", " #urgente", " 🚨", ":", "...",
         " em 25/09/2026", " at 14:30 UTC", " por R$ 1.200,00", " [TODO]", ' "quote"']


def combos(n, seed=20260925):
    rnd = random.Random(seed)
    out = []
    langs = [(PT, VERBS_PT), (EN, VERBS_EN), (ES, VERBS_ES)]
    for _ in range(n):
        parts = []
        for _ in range(rnd.randint(1, 4)):
            subj, verbs = rnd.choice(langs)
            s = rnd.choice(subj) + " " + rnd.choice(verbs)
            r = rnd.random()
            if r < 0.15:
                s = s.upper()
            elif r < 0.3:
                s = s.capitalize()
            elif r < 0.38:
                s = unicodedata.normalize("NFD", s)
            parts.append(s)
        sep = rnd.choice([" ", ", ", "; ", ". ", " e ", " and ", " y ", "\n", "  "])
        out.append(sep.join(parts) + rnd.choice(TAILS))
    return out


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    path = os.path.expanduser(sys.argv[1])
    tok = Tokenizer.from_file(path)
    tok.no_padding()
    with open(path, "rb") as f:
        sha = hashlib.sha256(f.read()).hexdigest()

    texts, seen = [], set()
    for t in fixture_texts() + EDGE + combos(1800):
        if t not in seen:
            seen.add(t)
            texts.append(t)

    out = os.path.join(HERE, "golden.jsonl")
    with open(out, "w", encoding="utf-8") as f:
        meta = {"meta": {"tokenizer_sha256": sha, "tokenizers": tokenizers.__version__,
                         "max_length": 128, "count": len(texts)}}
        f.write(json.dumps(meta, ensure_ascii=False) + "\n")
        for t in texts:
            ids = tok.encode(t).ids
            f.write(json.dumps({"t": t, "ids": ids}, ensure_ascii=False) + "\n")
    print(f"{len(texts)} sentences → {out}")


if __name__ == "__main__":
    main()
