package memory

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// goldenTokenizerPath locates the tokenizer.json the golden set was generated
// from: ANCHORED_TOKENIZER_JSON, or the one the ONNX embedder downloads. The
// file is 9 MB and not in the repo, so the test skips where it is absent (CI).
func goldenTokenizerPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("ANCHORED_TOKENIZER_JSON"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to find tokenizer.json in")
	}
	return filepath.Join(home, ".anchored", "data", "onnx", onnxModelName, "tokenizer.json")
}

type goldenCase struct {
	Text string `json:"t"`
	IDs  []int  `json:"ids"`
}

func loadTokenizerGolden(t *testing.T) (sha string, cases []goldenCase) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "tokenizer", "golden.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }() // read-only
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for first := true; sc.Scan(); first = false {
		if first {
			var meta struct {
				Meta struct {
					SHA string `json:"tokenizer_sha256"`
				} `json:"meta"`
			}
			if err := json.Unmarshal(sc.Bytes(), &meta); err != nil {
				t.Fatal(err)
			}
			sha = meta.Meta.SHA
			continue
		}
		var c goldenCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return sha, cases
}

// TestTokenizerGolden holds the fast tokenizer to the HuggingFace reference:
// every sentence of testdata/tokenizer/golden.jsonl (see gen.py) must produce
// the ids the `tokenizers` library produced for the same tokenizer.json. A
// mismatch means documents and queries land in a different place of the
// model's space than the model was trained for.
func TestTokenizerGolden(t *testing.T) {
	path := goldenTokenizerPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("tokenizer.json not available (%v); set ANCHORED_TOKENIZER_JSON", err)
	}
	sum := sha256.Sum256(raw)
	wantSHA, cases := loadTokenizerGolden(t)
	if got := hex.EncodeToString(sum[:]); got != wantSHA {
		t.Skipf("%s is not the tokenizer the golden set was generated from (sha %s, want %s)", path, got[:12], wantSHA[:12])
	}

	tok, err := NewFastTokenizer(path, onnxMaxSeqLen)
	if err != nil {
		t.Fatal(err)
	}
	matched, negative, logged := 0, 0, 0
	for _, c := range cases {
		ids, mask, _ := tok.Tokenize(c.Text)
		var got []int
		for i := range ids {
			if mask[i] == 1 {
				got = append(got, int(ids[i]))
				if ids[i] < 0 {
					negative++
				}
			}
		}
		if equalInts(got, c.IDs) {
			matched++
			continue
		}
		if logged < 15 {
			logged++
			t.Logf("mismatch %q\n  want %v\n  got  %v", truncateForLog(c.Text), c.IDs, got)
		}
	}
	rate := float64(matched) / float64(len(cases))
	t.Logf("golden: %d/%d exact (%.2f%%), %d negative ids", matched, len(cases), 100*rate, negative)
	if negative > 0 {
		t.Errorf("%d token ids are negative: the model receives garbage for them", negative)
	}
	// The rubric allows 99%, but the set matches in full: any sentence that
	// stops matching is a regression on a rare path (added tokens, graphemes,
	// Metaspace), so the bar is every sentence.
	if matched != len(cases) {
		t.Errorf("%d of %d golden sentences diverge from the HuggingFace tokenizer (%.2f%%)", len(cases)-matched, len(cases), 100*rate)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncateForLog(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

// TestLegacyTokenizerUnchanged pins the legacy pipeline to what v0.19 did:
// testdata/tokenizer/legacy.sha holds, per golden sentence, the first 8 bytes
// of the SHA-256 of the ids the pre-v0.20 tokenizer produced. Vectors of the
// active generation were built that way, so queries against it must keep
// being tokenized that way until a generation built by NewFastTokenizer
// replaces it.
func TestLegacyTokenizerUnchanged(t *testing.T) {
	path := goldenTokenizerPath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("tokenizer.json not available (%v); set ANCHORED_TOKENIZER_JSON", err)
	}
	sum := sha256.Sum256(raw)
	wantSHA, cases := loadTokenizerGolden(t)
	if hex.EncodeToString(sum[:]) != wantSHA {
		t.Skip("not the tokenizer the legacy hashes were recorded with")
	}
	want, err := os.ReadFile(filepath.Join("testdata", "tokenizer", "legacy.sha"))
	if err != nil {
		t.Fatal(err)
	}
	hashes := strings.Fields(string(want))
	if len(hashes) != len(cases) {
		t.Fatalf("legacy.sha has %d hashes for %d sentences", len(hashes), len(cases))
	}
	tok, err := NewLegacyFastTokenizer(path, onnxMaxSeqLen)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for i, c := range cases {
		ids, mask, _ := tok.Tokenize(c.Text)
		var parts []string
		for j := range ids {
			if mask[j] == 1 {
				parts = append(parts, strconv.FormatInt(ids[j], 10))
			}
		}
		h := sha256.Sum256([]byte(strings.Join(parts, ",")))
		if hex.EncodeToString(h[:8]) != hashes[i] {
			if changed < 5 {
				t.Logf("legacy output changed for %q", truncateForLog(c.Text))
			}
			changed++
		}
	}
	if changed > 0 {
		t.Errorf("legacy tokenizer changed its output for %d of %d sentences", changed, len(cases))
	}
}

// Two segmentations of "▁ab" score the same (-2): "▁ab" and "▁a"+"b". The
// reference keeps the first one found — pieces are tried shortest first from
// each position and only a strictly better path replaces the best so far —
// so the whole-word piece wins.
func TestUnigramViterbi_KeepsTheFirstPathOnATie(t *testing.T) {
	vocab, _ := json.Marshal([][]interface{}{
		{"<s>", 0.0}, {"<pad>", 0.0}, {"</s>", 0.0}, {"<unk>", 0.0},
		{"▁a", -1.0}, {"b", -1.0}, {"▁ab", -2.0},
	})
	unk := 3
	cfg := tokenizerConfig{
		Model:        modelConfig{Type: "Unigram", Vocab: vocab, UnkID: &unk},
		PreTokenizer: &preTokenizerConfig{Type: "Metaspace"},
	}
	raw, _ := json.Marshal(cfg)
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := NewFastTokenizer(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	if got := tok.encodeUnigramViterbi("▁ab"); !equalInts(got, []int{6}) {
		t.Fatalf("tie resolved to %v, want [6] (▁ab)", got)
	}
}

// Added tokens are cut out of the raw text before normalization; lstrip and
// rstrip ones take the whitespace beside them with them.
func TestSplitAddedTokens_StripsWhitespaceBesideLstripAndRstripTokens(t *testing.T) {
	ft := &FastTokenizer{specials: []addedToken{
		{content: "<l>", id: 7, lstrip: true},
		{content: "<r>", id: 8, rstrip: true},
		{content: "<plain>", id: 9},
	}}
	sortAddedTokens(ft.specials)
	got := ft.splitAddedTokens("a  <l>  b <r>  c <plain> d")
	want := []textSegment{
		{text: "a", id: -1}, {id: 7}, {text: "  b ", id: -1}, {id: 8},
		{text: "c ", id: -1}, {id: 9}, {text: " d", id: -1},
	}
	if len(got) != len(want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d = %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}

// The word cap never shortens what the Unigram model can see.
func TestHFWordCap_CoversTheModelWindow(t *testing.T) {
	ft := &FastTokenizer{modelType: "Unigram", maxLen: 128, maxPieceBytes: 48}
	if ft.hfWordCap() < 126*48 {
		t.Fatalf("cap %d below what 126 pieces can span", ft.hfWordCap())
	}
	if (&FastTokenizer{modelType: "BPE", maxLen: 128}).hfWordCap() != maxWordBytes {
		t.Fatal("BPE keeps the legacy per-word cap")
	}
}
