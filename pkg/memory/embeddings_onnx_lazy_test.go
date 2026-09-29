package memory

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installedONNXDir is the model directory of an installed anchored, so the
// test never downloads the 470 MB model: it skips where none is installed (CI).
func installedONNXDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("ANCHORED_ONNX_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home directory")
		}
		dir = filepath.Join(home, ".anchored", "data", "onnx")
	}
	paths := resolveONNXPaths(dir)
	for _, f := range []string{paths.ModelFile, paths.TokenizerFile, paths.RuntimeLib} {
		if !fileExists(f) {
			t.Skipf("ONNX model not installed (%s missing); set ANCHORED_ONNX_DIR", f)
		}
	}
	return dir
}

// The model loads on the first embed, unloads after the idle delay, and loads
// again on the next embed with the same vectors.
func TestONNXEmbedder_LoadsOnFirstEmbedAndUnloadsWhenIdle(t *testing.T) {
	e, err := NewONNXEmbedderV2(installedONNXDir(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	if e.Loaded() {
		t.Fatal("the model loaded before any embed")
	}

	e.SetIdleUnload(150 * time.Millisecond)
	ctx := context.Background()
	first, err := e.Embed(ctx, []string{"o modelo carrega no primeiro uso"})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Loaded() {
		t.Fatal("the model is not loaded after an embed")
	}

	deadline := time.Now().Add(5 * time.Second)
	for e.Loaded() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if e.Loaded() {
		t.Fatal("the model stayed loaded past the idle delay")
	}

	again, err := e.Embed(ctx, []string{"o modelo carrega no primeiro uso"})
	if err != nil {
		t.Fatal(err)
	}
	if e.rt.loads != 2 {
		t.Fatalf("loads = %d, want 2", e.rt.loads)
	}
	for i := range first[0] {
		if first[0][i] != again[0][i] {
			t.Fatalf("dimension %d differs after reloading: %v vs %v", i, first[0][i], again[0][i])
		}
	}
}

// With the idle unload off, the model stays loaded once used.
func TestONNXEmbedder_StaysLoadedWithoutIdleUnload(t *testing.T) {
	e, err := NewONNXEmbedderV2(installedONNXDir(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	e.SetIdleUnload(0)
	if _, err := e.Embed(context.Background(), []string{"fica carregado"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !e.Loaded() {
		t.Fatal("the model unloaded with the idle unload off")
	}
}

// Both pipelines of one load share the session: using one keeps the other's
// model loaded too, and each keeps producing its own vectors.
func TestONNXEmbedder_PipelinesShareTheLoadedModel(t *testing.T) {
	v2, err := NewONNXEmbedderV2(installedONNXDir(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v2.Close() }()
	legacy, err := v2.Pipeline(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacy.Close() }()
	ctx := context.Background()
	text := "pipelines diferentes, o mesmo modelo"
	a, err := v2.Embed(ctx, []string{text})
	if err != nil {
		t.Fatal(err)
	}
	b, err := legacy.Embed(ctx, []string{text})
	if err != nil {
		t.Fatal(err)
	}
	if v2.rt.loads != 1 || !legacy.Loaded() {
		t.Fatalf("loads = %d, legacy loaded = %v: the pipelines should share one session", v2.rt.loads, legacy.Loaded())
	}
	same := true
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("the legacy and v2 pipelines produced the same vector")
	}
}

func testRuntime(t *testing.T) (*onnxRuntime, *ONNXPaths) {
	t.Helper()
	dir := t.TempDir()
	paths := &ONNXPaths{TokenizerFile: writeTestTokenizerJSON(t, dir), VocabFile: filepath.Join(dir, "vocab.txt"), ModelFile: filepath.Join(dir, "model.onnx")}
	return newONNXRuntime(paths, false, slog.New(slog.NewTextHandler(io.Discard, nil))), paths
}

// A timer that fires while an embed holds the lock finds a fresh use and waits
// again instead of unloading.
func TestONNXRuntime_IdleTimerKeepsARecentlyUsedModel(t *testing.T) {
	rt, paths := testRuntime(t)
	rt.idleAfter = time.Hour
	rt.mu.Lock()
	if _, err := rt.tokenizerLocked(false, expectedONNXPipeline(paths, false)); err != nil {
		rt.mu.Unlock()
		t.Fatal(err)
	}
	rt.touchLocked()
	rt.mu.Unlock()
	defer rt.idle.Stop()

	rt.unloadIfIdle()
	if rt.tokenizers[false] == nil {
		t.Fatal("a model used a moment ago was unloaded")
	}
	rt.lastUse = time.Now().Add(-2 * time.Hour)
	rt.unloadIfIdle()
	if rt.tokenizers[false] != nil {
		t.Fatal("a model idle past the delay stayed loaded")
	}
}

// With the idle unload off, a use arms no timer.
func TestONNXRuntime_NoIdleTimerWhenUnloadIsOff(t *testing.T) {
	rt, _ := testRuntime(t)
	rt.idleAfter = 0
	rt.mu.Lock()
	rt.touchLocked()
	rt.mu.Unlock()
	if rt.idle != nil {
		t.Fatal("a timer was armed with the idle unload off")
	}
}

// A tokenizer that loads as another pipeline than the one the embedder's
// identity names is refused, so no vector lands in a space it is not
// labelled with.
func TestONNXRuntime_RefusesATokenizerOfAnotherPipeline(t *testing.T) {
	rt, paths := testRuntime(t)
	if err := os.WriteFile(paths.TokenizerFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.VocabFile, []byte("[PAD]\n[UNK]\n[CLS]\n[SEP]\nhello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if _, err := rt.tokenizerLocked(false, expectedONNXPipeline(paths, false)); err == nil {
		t.Fatal("a WordPiece fallback was accepted for an embedder built for tokenizer.json")
	}
}

// A closed embedder refuses to embed instead of loading the model again.
func TestONNXEmbedder_ClosedRefusesToEmbed(t *testing.T) {
	e, err := NewONNXEmbedderV2(installedONNXDir(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Embed(context.Background(), []string{"antes de fechar"}); err != nil {
		t.Fatal(err)
	}
	_ = e.Close()
	if e.Loaded() {
		t.Fatal("the model is still loaded after the last Close")
	}
	if _, err := e.Embed(context.Background(), []string{"depois de fechar"}); err == nil {
		t.Fatal("a closed embedder embedded")
	}
	if e.Loaded() {
		t.Fatal("embedding after Close loaded the model again")
	}
}
