package memory

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type identityTestEmbedder struct {
	name     string
	model    string
	revision string
	dims     int
	purpose  EmbeddingPurpose
}

func (e *identityTestEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}
func (e *identityTestEmbedder) EmbedPurpose(_ context.Context, purpose EmbeddingPurpose, _ []string) ([][]float32, error) {
	e.purpose = purpose
	return [][]float32{{2}}, nil
}
func (e *identityTestEmbedder) Dimensions() int       { return e.dims }
func (e *identityTestEmbedder) Name() string          { return e.name }
func (e *identityTestEmbedder) Model() string         { return e.model }
func (e *identityTestEmbedder) ModelRevision() string { return e.revision }
func (e *identityTestEmbedder) Close() error          { return nil }

func TestEmbeddingIdentityRejectsSameDimensionDifferentModels(t *testing.T) {
	a, err := EmbeddingIdentityOf(&identityTestEmbedder{name: "test", model: "a", revision: "1", dims: 384})
	if err != nil {
		t.Fatal(err)
	}
	b, err := EmbeddingIdentityOf(&identityTestEmbedder{name: "test", model: "b", revision: "1", dims: 384})
	if err != nil {
		t.Fatal(err)
	}
	if a.Compatible(b) || a.SemanticSpaceID() == b.SemanticSpaceID() {
		t.Fatal("same dimensions from different models must not be compatible")
	}
}

func TestEmbeddingIdentityRevisionAndDimensionsDefineSpace(t *testing.T) {
	base := EmbeddingIdentity{Provider: "p", Model: "m", ModelRevision: "r1", Dimensions: 384, Normalization: "l2"}
	for _, other := range []EmbeddingIdentity{
		{Provider: "p", Model: "m", ModelRevision: "r2", Dimensions: 384, Normalization: "l2"},
		{Provider: "p", Model: "m", ModelRevision: "r1", Dimensions: 768, Normalization: "l2"},
		{Provider: "p", Model: "m", ModelRevision: "r1", Dimensions: 384, Normalization: "none"},
	} {
		if base.Compatible(other) {
			t.Fatalf("identity unexpectedly compatible with %+v", other)
		}
	}
	if !base.Compatible(base) {
		t.Fatal("identical identities must be compatible")
	}
}

func TestEmbedForPurposeUsesExtensionAndValidatesPurpose(t *testing.T) {
	e := &identityTestEmbedder{name: "p", model: "m", dims: 1}
	vecs, err := EmbedForPurpose(context.Background(), e, EmbeddingPurposeQuery, []string{"q"})
	if err != nil {
		t.Fatal(err)
	}
	if e.purpose != EmbeddingPurposeQuery || len(vecs) != 1 || vecs[0][0] != 2 {
		t.Fatalf("purpose extension was not used: purpose=%q vecs=%v", e.purpose, vecs)
	}
	if _, err := EmbedForPurpose(context.Background(), e, "unknown", []string{"q"}); err == nil {
		t.Fatal("invalid purpose should fail")
	}
}

func TestONNXArtifactRevisionChangesWithModelOrTokenizer(t *testing.T) {
	dir := t.TempDir()
	paths := &ONNXPaths{
		ModelFile:     filepath.Join(dir, "model.onnx"),
		TokenizerFile: filepath.Join(dir, "tokenizer.json"),
		VocabFile:     filepath.Join(dir, "vocab.txt"),
	}
	for path, content := range map[string]string{
		paths.ModelFile: "model-a", paths.TokenizerFile: "tokenizer-a", paths.VocabFile: "vocab-a",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, err := onnxArtifactRevision(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TokenizerFile, []byte("tokenizer-b"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := onnxArtifactRevision(paths)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first == "" || second == "" {
		t.Fatalf("artifact revisions first=%q second=%q", first, second)
	}
	otherPipeline, err := onnxArtifactRevisionForPipeline(paths, "different-pipeline")
	if err != nil {
		t.Fatal(err)
	}
	if second == otherPipeline {
		t.Fatalf("pipeline contract did not affect artifact revision: %q", second)
	}
}

func TestONNXAssetURLsKeepLegacyTokenizerWithLegacyModel(t *testing.T) {
	model, tokenizer, vocab := onnxAssetURLs(true)
	for name, value := range map[string]string{
		"model": model, "tokenizer": tokenizer, "vocab": vocab,
	} {
		if !strings.Contains(value, legacyModelName) ||
			strings.Contains(value, onnxModelName) {
			t.Fatalf("legacy %s URL uses incompatible model root: %s", name, value)
		}
		if !strings.Contains(value, legacyModelRevision) {
			t.Fatalf("legacy %s URL is not revision-pinned: %s", name, value)
		}
	}
	if want := onnxLegacyModelBaseURL + "/vocab.txt"; vocab != want {
		t.Fatalf("legacy vocab URL=%q, want %q", vocab, want)
	}
}

// Every vector up to v0.19 carries the legacy pipeline revision; changing the
// string would orphan the active generation of every installation.
func TestLegacyPipelineRevisionNeverChanges(t *testing.T) {
	if onnxPipelineRevision != "wordpiece-v1:maxseq128:mean-pool:l2-v1" {
		t.Fatalf("legacy pipeline revision changed to %q", onnxPipelineRevision)
	}
}

// The v2 pipeline names the tokenizer that loaded: the HF tokenizer.json path
// and the vocab.txt fallback produce different vectors. The legacy pipeline
// keeps v0.19's single revision for both.
func TestLoadONNXTokenizer_PipelineNamesTheTokenizerThatLoaded(t *testing.T) {
	dir := t.TempDir()
	withJSON := &ONNXPaths{TokenizerFile: writeTestTokenizerJSON(t, dir), VocabFile: filepath.Join(dir, "missing-vocab.txt")}
	vocabOnly := &ONNXPaths{TokenizerFile: filepath.Join(dir, "missing.json"), VocabFile: filepath.Join(dir, "vocab.txt")}
	if err := os.WriteFile(vocabOnly.VocabFile, []byte("[PAD]\n[UNK]\n[CLS]\n[SEP]\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cases := []struct {
		name   string
		paths  *ONNXPaths
		legacy bool
		want   string
		fast   bool
	}{
		{"tokenizer.json, legacy", withJSON, true, onnxPipelineRevision, true},
		{"tokenizer.json, v2", withJSON, false, onnxPipelineRevisionHF, true},
		{"vocab.txt, legacy", vocabOnly, true, onnxPipelineRevision, false},
		{"vocab.txt, v2", vocabOnly, false, onnxPipelineRevisionVocabV2, false},
	}
	for _, c := range cases {
		tok, pipeline, err := loadONNXTokenizer(c.paths, c.legacy, logger)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ft, isFast := tok.(*FastTokenizer)
		if pipeline != c.want || isFast != c.fast || (isFast && ft.legacy != c.legacy) {
			t.Errorf("%s: pipeline %q fast=%v, want %q fast=%v", c.name, pipeline, isFast, c.want, c.fast)
		}
	}
}

// Pipelines built from one load share the session: closing one (twice) leaves
// the others working, and the last Close releases it.
func TestONNXPipeline_RefcountsTheSharedSession(t *testing.T) {
	dir := t.TempDir()
	paths := &ONNXPaths{TokenizerFile: writeTestTokenizerJSON(t, dir), VocabFile: filepath.Join(dir, "none.txt"), ModelFile: filepath.Join(dir, "model.onnx")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tok, pipeline, err := loadONNXTokenizer(paths, false, logger)
	if err != nil {
		t.Fatal(err)
	}
	rev, err := onnxArtifactRevisionForPipeline(paths, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	v2 := &ONNXEmbedder{tokenizer: tok, modelRevision: rev, paths: paths, logger: logger,
		mu: &sync.Mutex{}, refs: &onnxRefs{n: 1}, dims: onnxModelDims, modelName: onnxModelName}
	le, err := v2.Pipeline(true)
	if err != nil {
		t.Fatal(err)
	}
	legacy := le
	if !le.Legacy() || le.ModelRevision() == v2.ModelRevision() || le.mu != v2.mu || v2.refs.n != 2 {
		t.Fatalf("legacy pipeline: legacy=%v same revision=%v shared lock=%v refs=%d",
			le.Legacy(), le.ModelRevision() == v2.ModelRevision(), le.mu == v2.mu, v2.refs.n)
	}
	_ = legacy.Close()
	_ = legacy.Close()
	if v2.refs.n != 1 {
		t.Fatalf("refs after closing the legacy pipeline twice: %d", v2.refs.n)
	}
	_ = v2.Close()
	if v2.refs.n != 0 {
		t.Fatalf("refs after the last close: %d", v2.refs.n)
	}
}
