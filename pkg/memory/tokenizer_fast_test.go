package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestTokenizerJSON(t *testing.T, dir string) string {
	t.Helper()
	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type: "WordPiece",
			Vocab: map[string]int{
				"[PAD]":      0,
				"[UNK]":      100,
				"[CLS]":      101,
				"[SEP]":      102,
				"[MASK]":     103,
				"o":          104,
				"sistema":    105,
				"usa":        106,
				"j":          107,
				"##w":        108,
				"##t":        109,
				"tokens":     110,
				"deploy":     111,
				"no":         112,
				"kubernetes": 113,
				"how":        114,
				"to":         115,
				"authenticate": 116,
				"api":        117,
				"rate":       118,
				"limiting":   119,
				"hello":      120,
				"world":      121,
				"the":        122,
				"quick":      123,
				"brown":      124,
				"fox":        125,
				"a":          126,
				"test":       127,
				"café":       128,
				"naïve":      129,
				"##ï":        130,
				"##ve":       131,
			},
			UnkToken: "[UNK]",
			Prefix:   "##",
			MaxChars: 200,
		},
		Normalizer: &normalizerConfig{
			Type: "Sequence",
			Normalizers: []normalizerConfig{
				{Type: "NFD"},
				{Type: "Lowercase"},
				{Type: "StripAccents"},
			},
		},
		PreTokenizer: &preTokenizerConfig{
			Type: "BertPreTokenizer",
		},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 0, Content: "[PAD]", Special: true},
			{ID: 100, Content: "[UNK]", Special: true},
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
			{ID: 103, Content: "[MASK]", Special: true},
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal tokenizer config: %v", err)
	}
	path := filepath.Join(dir, "tokenizer.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write tokenizer.json: %v", err)
	}
	return path
}

func writeMultilingualTokenizerJSON(t *testing.T, dir string) string {
	t.Helper()

	vocab := map[string]int{
		"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102,
	}
	id := 103
	words := []string{
		"o", "sistema", "usa", "jwt", "tokens", "deploy", "no", "kubernetes",
		"how", "to", "authenticate", "api", "rate", "limiting",
		"hello", "world", "the", "quick", "brown", "fox", "a", "test",
		"são", "paulo", "coração", "definição", "usuário",
		"##ção", "##lo", "##ão",
		"é", "está", "não", "português",
	}
	for _, w := range words {
		if _, exists := vocab[w]; !exists {
			vocab[w] = id
			id++
		}
	}

	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type:      "WordPiece",
			Vocab:     vocab,
			UnkToken:  "[UNK]",
			Prefix:    "##",
			MaxChars:  200,
		},
		Normalizer:   nil,
		PreTokenizer: &preTokenizerConfig{Type: "Whitespace"},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 0, Content: "[PAD]", Special: true},
			{ID: 100, Content: "[UNK]", Special: true},
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
		},
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal tokenizer config: %v", err)
	}
	path := filepath.Join(dir, "tokenizer_multilingual.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write tokenizer_multilingual.json: %v", err)
	}
	return path
}

func TestFastTokenizer_PTBRSentences(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTokenizerJSON(t, dir)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	tests := []struct {
		name        string
		text        string
		wantFirst   int
		wantLast    int
		wantSeqLen  int
	}{
		{
			name:       "jwt tokens",
			text:       "O sistema usa JWT tokens",
			wantFirst:  101,
			wantLast:   102,
		},
		{
			name:       "kubernetes deploy",
			text:       "Deploy no Kubernetes",
			wantFirst:  101,
			wantLast:   102,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputIDs, attentionMask, tokenTypeIDs := ft.Tokenize(tt.text)
			if len(inputIDs) != 128 {
				t.Fatalf("inputIDs length = %d, want 128", len(inputIDs))
			}
			if len(attentionMask) != 128 {
				t.Fatalf("attentionMask length = %d, want 128", len(attentionMask))
			}
			if len(tokenTypeIDs) != 128 {
				t.Fatalf("tokenTypeIDs length = %d, want 128", len(tokenTypeIDs))
			}
			if inputIDs[0] != int64(tt.wantFirst) {
				t.Errorf("first token = %d, want %d (CLS)", inputIDs[0], tt.wantFirst)
			}
			seqLen := 0
			for _, m := range attentionMask {
				if m == 1 {
					seqLen++
				}
			}
			if inputIDs[seqLen-1] != int64(tt.wantLast) {
				t.Errorf("last real token = %d, want %d (SEP)", inputIDs[seqLen-1], tt.wantLast)
			}
			if inputIDs[seqLen] != 0 {
				t.Errorf("first pad token = %d, want 0 (PAD)", inputIDs[seqLen])
			}
			for i := seqLen; i < 128; i++ {
				if attentionMask[i] != 0 {
					t.Errorf("attentionMask[%d] = %d, want 0 (pad)", i, attentionMask[i])
				}
			}
		})
	}
}

func TestFastTokenizer_ENSentences(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTokenizerJSON(t, dir)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	tests := []struct {
		name string
		text string
	}{
		{"authenticate", "How to authenticate"},
		{"rate limiting", "API rate limiting"},
		{"hello world", "hello world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputIDs, attentionMask, tokenTypeIDs := ft.Tokenize(tt.text)
			if len(inputIDs) != 128 {
				t.Fatalf("inputIDs length = %d, want 128", len(inputIDs))
			}
			if inputIDs[0] != 101 {
				t.Errorf("first token = %d, want 101 (CLS)", inputIDs[0])
			}
			seqLen := 0
			for _, m := range attentionMask {
				if m == 1 {
					seqLen++
				}
			}
			if inputIDs[seqLen-1] != 102 {
				t.Errorf("last real token = %d, want 102 (SEP)", inputIDs[seqLen-1])
			}
			_ = tokenTypeIDs
		})
	}
}

func TestFastTokenizer_MultilingualNoLowercase(t *testing.T) {
	dir := t.TempDir()
	path := writeMultilingualTokenizerJSON(t, dir)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	if ft.doLowerCase {
		t.Error("doLowerCase should be false for multilingual tokenizer (no normalizer)")
	}

	inputIDs, _, _ := ft.Tokenize("Deploy no Kubernetes")
	if inputIDs[0] != 101 {
		t.Errorf("first token = %d, want 101 (CLS)", inputIDs[0])
	}
}

func TestFastTokenizer_NormalizerPipeline(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTokenizerJSON(t, dir)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	if !ft.doLowerCase {
		t.Error("doLowerCase should be true for BERT-style tokenizer")
	}

	inputIDs, _, _ := ft.Tokenize("Hello World")
	if inputIDs[0] != 101 {
		t.Errorf("CLS = %d, want 101", inputIDs[0])
	}
}

func TestFastTokenizer_Truncation(t *testing.T) {
	dir := t.TempDir()
	path := writeTestTokenizerJSON(t, dir)

	ft, err := NewFastTokenizer(path, 8)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	inputIDs, attentionMask, _ := ft.Tokenize("hello world the quick brown fox a test hello world")

	if len(inputIDs) != 8 {
		t.Fatalf("inputIDs length = %d, want 8", len(inputIDs))
	}

	seqLen := 0
	for _, m := range attentionMask {
		if m == 1 {
			seqLen++
		}
	}
	if seqLen != 8 {
		t.Errorf("seqLen = %d, want 8 (truncated)", seqLen)
	}
	if inputIDs[0] != 101 {
		t.Errorf("first token = %d, want 101 (CLS)", inputIDs[0])
	}
}

func TestFastTokenizer_NoNormalizer(t *testing.T) {
	dir := t.TempDir()

	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type: "WordPiece",
			Vocab: map[string]int{
				"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102,
				"Hello": 103, "World": 104,
			},
			UnkToken: "[UNK]",
		},
		PreTokenizer: &preTokenizerConfig{Type: "Whitespace"},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 0, Content: "[PAD]", Special: true},
			{ID: 100, Content: "[UNK]", Special: true},
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
		},
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(dir, "tokenizer.json")
	os.WriteFile(path, data, 0o644)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	inputIDs, _, _ := ft.Tokenize("Hello World")
	if inputIDs[0] != 101 {
		t.Errorf("CLS = %d, want 101", inputIDs[0])
	}
	if inputIDs[1] != 103 {
		t.Errorf("Hello = %d, want 103", inputIDs[1])
	}
	if inputIDs[2] != 104 {
		t.Errorf("World = %d, want 104", inputIDs[2])
	}
	if inputIDs[3] != 102 {
		t.Errorf("SEP = %d, want 102", inputIDs[3])
	}
}

func TestFastTokenizer_WordPieceSubwords(t *testing.T) {
	dir := t.TempDir()

	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type: "WordPiece",
			Vocab: map[string]int{
				"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102,
				"unigram": 103,
				"un": 104, "##ig": 105, "##ram": 106,
			},
			UnkToken: "[UNK]",
			Prefix:   "##",
		},
		PreTokenizer: &preTokenizerConfig{Type: "Whitespace"},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 0, Content: "[PAD]", Special: true},
			{ID: 100, Content: "[UNK]", Special: true},
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
		},
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(dir, "tokenizer.json")
	os.WriteFile(path, data, 0o644)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	inputIDs, _, _ := ft.Tokenize("unigram")
	if inputIDs[0] != 101 {
		t.Errorf("CLS = %d, want 101", inputIDs[0])
	}
	if inputIDs[1] != 103 {
		t.Errorf("unigram = %d, want 103", inputIDs[1])
	}
	if inputIDs[2] != 102 {
		t.Errorf("SEP = %d, want 102", inputIDs[2])
	}
}

func TestFastTokenizer_NFDAccentStripping(t *testing.T) {
	dir := t.TempDir()

	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type: "WordPiece",
			Vocab: map[string]int{
				"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102,
				"cafe": 103,
			},
			UnkToken: "[UNK]",
		},
		Normalizer: &normalizerConfig{
			Type: "Sequence",
			Normalizers: []normalizerConfig{
				{Type: "NFD"},
				{Type: "Lowercase"},
				{Type: "StripAccents"},
			},
		},
		PreTokenizer: &preTokenizerConfig{Type: "Whitespace"},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 0, Content: "[PAD]", Special: true},
			{ID: 100, Content: "[UNK]", Special: true},
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
		},
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(dir, "tokenizer.json")
	os.WriteFile(path, data, 0o644)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	inputIDs, _, _ := ft.Tokenize("café")
	if inputIDs[0] != 101 {
		t.Errorf("CLS = %d, want 101", inputIDs[0])
	}
	if inputIDs[1] != 103 {
		t.Errorf("café (stripped to cafe) = %d, want 103", inputIDs[1])
	}
	if inputIDs[2] != 102 {
		t.Errorf("SEP = %d, want 102", inputIDs[2])
	}
}

func TestFastTokenizer_BPEModel(t *testing.T) {
	dir := t.TempDir()

	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type: "BPE",
			Vocab: map[string]int{
				"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102,
				"h": 103, "e": 104, "l": 105, "o": 106,
				"he": 107, "ll": 108, "hello": 109,
			},
			UnkToken: "[UNK]",
			Merges:   []string{"h e", "l l", "he ll", "hell o"},
		},
		PreTokenizer: &preTokenizerConfig{Type: "Whitespace"},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 0, Content: "[PAD]", Special: true},
			{ID: 100, Content: "[UNK]", Special: true},
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
		},
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(dir, "tokenizer.json")
	os.WriteFile(path, data, 0o644)

	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}

	inputIDs, _, _ := ft.Tokenize("hello")
	if inputIDs[0] != 101 {
		t.Errorf("CLS = %d, want 101", inputIDs[0])
	}
	if inputIDs[1] != 109 {
		t.Errorf("hello = %d, want 109", inputIDs[1])
	}
	if inputIDs[2] != 102 {
		t.Errorf("SEP = %d, want 102", inputIDs[2])
	}
}

func TestNormalizeNFD(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"café", "cafe\u0301"},
		{"naïve", "nai\u0308ve"},
		{"São Paulo", "Sa\u0303o Paulo"},
		{"hello", "hello"},
		{"ABC", "ABC"},
		{"", ""},
	}
	for _, tt := range tests {
		got := normalizeNFD(tt.input)
		if got != tt.want {
			t.Errorf("normalizeNFD(%q) = %q (runes: %v), want %q (runes: %v)",
				tt.input, got, []rune(got), tt.want, []rune(tt.want))
		}
	}
}

func TestStripAccents(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"cafe\u0301", "cafe"},
		{"nai\u0308ve", "naive"},
		{"Sa\u0303o", "Sao"},
		{"hello", "hello"},
		{"", ""},
	}
	for _, tt := range tests {
		got := stripAccents(tt.input)
		if got != tt.want {
			t.Errorf("stripAccents(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFastTokenizer_FileNotFound(t *testing.T) {
	_, err := NewFastTokenizer("/nonexistent/tokenizer.json", 128)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestFastTokenizer_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokenizer.json")
	os.WriteFile(path, []byte("not json"), 0o644)

	_, err := NewFastTokenizer(path, 128)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestTokenizerInterface(t *testing.T) {
	dir := t.TempDir()

	cfg := tokenizerConfig{
		Version: "1.0",
		Model: modelConfig{
			Type: "WordPiece",
			Vocab: map[string]int{
				"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102, "hello": 103,
			},
			UnkToken: "[UNK]",
		},
		PreTokenizer: &preTokenizerConfig{Type: "Whitespace"},
		PostProcessor: &postProcessorConfig{
			Type: "BertProcessing",
			Sep:  []interface{}{"[SEP]", float64(102)},
			Cls:  []interface{}{"[CLS]", float64(101)},
		},
		AddedTokens: []addedTokenConfig{
			{ID: 101, Content: "[CLS]", Special: true},
			{ID: 102, Content: "[SEP]", Special: true},
		},
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(dir, "tokenizer.json")
	os.WriteFile(path, data, 0o644)

	var tok Tokenizer = &FastTokenizer{}
	ft, err := NewFastTokenizer(path, 128)
	if err != nil {
		t.Fatalf("NewFastTokenizer: %v", err)
	}
	tok = ft

	inputIDs, _, _ := tok.Tokenize("hello")
	if inputIDs[0] != 101 {
		t.Errorf("via interface: CLS = %d, want 101", inputIDs[0])
	}
}
