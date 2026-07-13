package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindONNXModel(t *testing.T) {
	dir := t.TempDir()

	if _, found := findONNXModel(dir); found {
		t.Fatal("expected no model in empty dir")
	}

	modelDir := filepath.Join(dir, "paraphrase-multilingual-MiniLM-L12-v2")
	if err := os.MkdirAll(modelDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.onnx"), []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}

	name, found := findONNXModel(dir)
	if !found {
		t.Fatal("expected model to be found")
	}
	if name != "paraphrase-multilingual-MiniLM-L12-v2" {
		t.Fatalf("model name = %q, want paraphrase-multilingual-MiniLM-L12-v2", name)
	}
}
