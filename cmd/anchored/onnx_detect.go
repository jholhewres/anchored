package main

import (
	"os"
	"path/filepath"
)

// findONNXModel returns the subdirectory name containing model.onnx, if any.
// Layout: <modelDir>/<model-name>/model.onnx (see docs/embedding-model.md).
func findONNXModel(modelDir string) (modelName string, found bool) {
	entries, err := os.ReadDir(modelDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(modelDir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, "model.onnx")); err == nil {
			return e.Name(), true
		}
	}
	return "", false
}
