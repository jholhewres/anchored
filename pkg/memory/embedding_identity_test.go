package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
