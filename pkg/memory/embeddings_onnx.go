package memory

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	onnxModelName       = "paraphrase-multilingual-MiniLM-L12-v2"
	legacyModelName     = "all-MiniLM-L6-v2"
	onnxModelDims       = 384
	onnxMaxSeqLen       = 128
	onnxRuntimeVersion  = "1.25.1"
	onnxModelRevision   = "e8f8c211226b894fcb81acc59f3b34ba3efd5f42"
	legacyModelRevision = "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"
	// Bump whenever tokenization, truncation, pooling, or normalization changes
	// without changing the downloaded model artifacts.
	//
	// onnxPipelineRevision names the legacy pipeline (NewLegacyFastTokenizer,
	// or vocab.txt WordPiece when tokenizer.json is missing): every vector up
	// to v0.19 was built by it, so it never changes. The v2 pipeline names the
	// tokenizer that actually loaded, since the two produce different vectors.
	onnxPipelineRevision        = "wordpiece-v1:maxseq128:mean-pool:l2-v1"
	onnxPipelineRevisionHF      = "hf-tokenizer-v2:maxseq128:mean-pool:l2-v1"
	onnxPipelineRevisionVocabV2 = "vocab-wordpiece-v2:maxseq128:mean-pool:l2-v1"

	onnxRuntimeURLTemplate = "https://github.com/microsoft/onnxruntime/releases/download/v%s/onnxruntime-%s-%s-%s.tgz"
	onnxModelBaseURL       = "https://huggingface.co/sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2/resolve/" + onnxModelRevision
	onnxLegacyModelBaseURL = "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/" + legacyModelRevision
)

type ONNXEmbedder struct {
	session       *ort.AdvancedSession
	tokenizer     Tokenizer
	dims          int
	logger        *slog.Logger
	modelName     string
	modelRevision string

	inputIDs      *ort.Tensor[int64]
	attentionMask *ort.Tensor[int64]
	tokenTypeIDs  *ort.Tensor[int64]
	output        *ort.Tensor[float32]

	// The session and its tensors are shared by the pipelines built from one
	// load (Pipeline): mu serializes inference across all of them and refs
	// destroys the session when the last one closes.
	mu     *sync.Mutex
	refs   *onnxRefs
	paths  *ONNXPaths
	legacy bool
	closed bool
}

type onnxRefs struct {
	mu sync.Mutex
	n  int
}

type ONNXPaths struct {
	RuntimeLib    string
	ModelFile     string
	VocabFile     string
	TokenizerFile string
}

// NewONNXEmbedder loads the model with the legacy pipeline, the one every
// vector up to v0.19 was built with (see NewLegacyFastTokenizer).
func NewONNXEmbedder(modelDir string, logger *slog.Logger) (*ONNXEmbedder, error) {
	return newONNXEmbedder(modelDir, true, logger)
}

// NewONNXEmbedderV2 loads the model with the v2 pipeline (the HuggingFace
// tokenizer); see onnxPipelineRevisionHF.
func NewONNXEmbedderV2(modelDir string, logger *slog.Logger) (*ONNXEmbedder, error) {
	return newONNXEmbedder(modelDir, false, logger)
}

func newONNXEmbedder(modelDir string, legacy bool, logger *slog.Logger) (*ONNXEmbedder, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "onnx-embedder")
	paths := resolveONNXPaths(modelDir)
	if err := ensureONNXRuntime(paths, logger); err != nil {
		return nil, fmt.Errorf("onnx: runtime setup: %w", err)
	}
	if err := ensureONNXModel(paths, logger); err != nil {
		return nil, fmt.Errorf("onnx: model setup: %w", err)
	}

	ort.SetSharedLibraryPath(paths.RuntimeLib)
	if !ort.IsInitialized() {
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("onnx: init environment: %w", err)
		}
	}

	if !legacy && !strings.Contains(paths.ModelFile, onnxModelName) {
		// The v2 pipeline is verified (TestTokenizerGolden) for the
		// multilingual model only; an install still on the older English
		// model keeps the pipeline its vectors were built with.
		logger.Info("v2 embedding pipeline is only verified for "+onnxModelName+"; keeping the legacy pipeline", "model", paths.ModelFile)
		legacy = true
	}

	tokenizer, pipeline, err := loadONNXTokenizer(paths, legacy, logger)
	if err != nil {
		return nil, err
	}
	modelRevision, err := onnxArtifactRevisionForPipeline(paths, pipeline)
	if err != nil {
		return nil, fmt.Errorf("onnx: identify model artifacts: %w", err)
	}

	shape := ort.NewShape(1, int64(onnxMaxSeqLen))
	inputIDs, err := ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return nil, fmt.Errorf("onnx: create input_ids tensor: %w", err)
	}
	attentionMask, err := ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return nil, fmt.Errorf("onnx: create attention_mask tensor: %w", err)
	}
	tokenTypeIDs, err := ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return nil, fmt.Errorf("onnx: create token_type_ids tensor: %w", err)
	}

	outputShape := ort.NewShape(1, int64(onnxMaxSeqLen), int64(onnxModelDims))
	output, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, fmt.Errorf("onnx: create output tensor: %w", err)
	}

	// Cap ONNX intra/inter-op parallelism. Embeds are short, single-sequence
	// inferences run by a background worker that may live in several processes at
	// once (the hub daemon plus each per-session MCP). With the default (nil)
	// options the runtime spread every inference across ALL cores, so a
	// corpus-wide re-embed saturated the machine (load ~15 on 12 cores). One
	// thread per op keeps each embed to ~1 core; the throttle paces the rest.
	sessOpts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("onnx: create session options: %w", err)
	}
	defer sessOpts.Destroy()
	if err := sessOpts.SetIntraOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("onnx: set intra-op threads: %w", err)
	}
	if err := sessOpts.SetInterOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("onnx: set inter-op threads: %w", err)
	}

	session, err := ort.NewAdvancedSession(
		paths.ModelFile,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"},
		[]ort.Value{inputIDs, attentionMask, tokenTypeIDs},
		[]ort.Value{output},
		sessOpts,
	)
	if err != nil {
		return nil, fmt.Errorf("onnx: create session: %w", err)
	}

	var activeModel string
	if strings.Contains(paths.ModelFile, legacyModelName) {
		activeModel = legacyModelName
	} else {
		activeModel = onnxModelName
	}

	logger.Info("ONNX embedder initialized", "model", activeModel, "dims", onnxModelDims)

	return &ONNXEmbedder{
		session:       session,
		tokenizer:     tokenizer,
		dims:          onnxModelDims,
		logger:        logger,
		modelName:     activeModel,
		modelRevision: modelRevision,
		inputIDs:      inputIDs,
		attentionMask: attentionMask,
		tokenTypeIDs:  tokenTypeIDs,
		output:        output,
		mu:            &sync.Mutex{},
		refs:          &onnxRefs{n: 1},
		paths:         paths,
		legacy:        legacy,
	}, nil
}

// Legacy reports whether e embeds with the legacy pipeline.
func (e *ONNXEmbedder) Legacy() bool { return e.legacy }

// Pipeline returns an embedder for the legacy (true) or v2 pipeline that
// shares e's model session: the two tokenize differently, so their vectors
// live in different semantic spaces, but the 470 MB model is loaded once. The
// result must be closed like any embedder.
func (e *ONNXEmbedder) Pipeline(legacy bool) (*ONNXEmbedder, error) {
	if legacy == e.legacy {
		e.refs.mu.Lock()
		e.refs.n++
		e.refs.mu.Unlock()
		clone := *e
		clone.closed = false
		return &clone, nil
	}
	tokenizer, pipeline, err := loadONNXTokenizer(e.paths, legacy, e.logger)
	if err != nil {
		return nil, err
	}
	revision, err := onnxArtifactRevisionForPipeline(e.paths, pipeline)
	if err != nil {
		return nil, fmt.Errorf("onnx: identify model artifacts: %w", err)
	}
	e.refs.mu.Lock()
	e.refs.n++
	e.refs.mu.Unlock()
	sibling := *e
	sibling.tokenizer = tokenizer
	sibling.modelRevision = revision
	sibling.legacy = legacy
	sibling.closed = false
	return &sibling, nil
}

func (e *ONNXEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := e.embedSingle(text)
		if err != nil {
			return nil, fmt.Errorf("onnx embed text %d: %w", i, err)
		}
		results[i] = vec
	}
	return results, nil
}

func (e *ONNXEmbedder) embedSingle(text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	ids, mask, typeIDs := e.tokenizer.Tokenize(text)

	copy(e.inputIDs.GetData(), ids)
	copy(e.attentionMask.GetData(), mask)
	copy(e.tokenTypeIDs.GetData(), typeIDs)

	if err := e.session.Run(); err != nil {
		return nil, fmt.Errorf("session run: %w", err)
	}

	raw := e.output.GetData()
	vec := meanPool(raw, mask, onnxMaxSeqLen, e.dims)
	l2Normalize(vec)

	result := make([]float32, len(vec))
	copy(result, vec)
	return result, nil
}

func (e *ONNXEmbedder) Dimensions() int       { return e.dims }
func (e *ONNXEmbedder) Name() string          { return "onnx" }
func (e *ONNXEmbedder) Model() string         { return e.modelName }
func (e *ONNXEmbedder) ModelRevision() string { return e.modelRevision }
func (e *ONNXEmbedder) Normalization() string { return "l2" }

// loadONNXTokenizer picks the tokenizer.json tokenizer, falling back to
// vocab.txt WordPiece, and returns the pipeline revision that names it. The
// legacy pipeline keeps v0.19's selection and its single revision.
func loadONNXTokenizer(paths *ONNXPaths, legacy bool, logger *slog.Logger) (Tokenizer, string, error) {
	if fileExists(paths.TokenizerFile) {
		newTok, pipeline := NewFastTokenizer, onnxPipelineRevisionHF
		if legacy {
			newTok, pipeline = NewLegacyFastTokenizer, onnxPipelineRevision
		}
		tok, err := newTok(paths.TokenizerFile, onnxMaxSeqLen)
		if err == nil {
			logger.Info("using fast tokenizer (tokenizer.json)", "legacy", legacy)
			return tok, pipeline, nil
		}
		logger.Warn("fast tokenizer failed, falling back to wordpiece", "error", err, "legacy", legacy)
	}
	tok, err := NewWordPieceTokenizer(paths.VocabFile, onnxMaxSeqLen)
	if err != nil {
		return nil, "", fmt.Errorf("onnx: load tokenizer: %w", err)
	}
	logger.Info("using wordpiece tokenizer (vocab.txt)", "legacy", legacy)
	if legacy {
		return tok, onnxPipelineRevision, nil
	}
	return tok, onnxPipelineRevisionVocabV2, nil
}

func onnxArtifactRevision(paths *ONNXPaths) (string, error) {
	return onnxArtifactRevisionForPipeline(paths, onnxPipelineRevision)
}

func onnxArtifactRevisionForPipeline(paths *ONNXPaths, pipelineRevision string) (string, error) {
	digest := sha256.New()
	if _, err := io.WriteString(digest, "pipeline\x00"+pipelineRevision+"\x00"); err != nil {
		return "", err
	}
	for _, artifact := range []string{paths.ModelFile, paths.TokenizerFile, paths.VocabFile} {
		if !fileExists(artifact) {
			continue
		}
		if err := hashArtifact(digest, artifact); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}

func hashArtifact(digest hash.Hash, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.WriteString(digest, filepath.Base(path)+"\x00"); err != nil {
		return err
	}
	if _, err := io.Copy(digest, file); err != nil {
		return err
	}
	_, err = io.WriteString(digest, "\x00")
	return err
}

func (e *ONNXEmbedder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	if e.refs != nil {
		e.refs.mu.Lock()
		e.refs.n--
		last := e.refs.n <= 0
		e.refs.mu.Unlock()
		if !last {
			return nil
		}
	}
	if e.session != nil {
		e.session.Destroy()
	}
	if e.inputIDs != nil {
		e.inputIDs.Destroy()
	}
	if e.attentionMask != nil {
		e.attentionMask.Destroy()
	}
	if e.tokenTypeIDs != nil {
		e.tokenTypeIDs.Destroy()
	}
	if e.output != nil {
		e.output.Destroy()
	}
	return nil
}

func meanPool(raw []float32, mask []int64, seqLen, dims int) []float32 {
	result := make([]float32, dims)
	var count float32
	for i := 0; i < seqLen; i++ {
		if mask[i] == 0 {
			continue
		}
		count++
		offset := i * dims
		for j := 0; j < dims; j++ {
			result[j] += raw[offset+j]
		}
	}
	if count > 0 {
		for j := range result {
			result[j] /= count
		}
	}
	return result
}

func l2Normalize(vec []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	norm := float32(math.Sqrt(sum))
	if norm > 0 {
		for i := range vec {
			vec[i] /= norm
		}
	}
}

func resolveONNXPaths(modelDir string) *ONNXPaths {
	libDir := filepath.Join(filepath.Dir(modelDir), "lib")

	// Prefer new model directory; fall back to legacy if it already exists.
	modelSubDir := filepath.Join(modelDir, onnxModelName)
	if !fileExists(filepath.Join(modelSubDir, "model.onnx")) {
		legacyDir := filepath.Join(modelDir, legacyModelName)
		if fileExists(filepath.Join(legacyDir, "model.onnx")) {
			modelSubDir = legacyDir
		}
	}

	libName := "libonnxruntime.so"
	if runtime.GOOS == "darwin" {
		libName = "libonnxruntime.dylib"
	}

	return &ONNXPaths{
		RuntimeLib:    filepath.Join(libDir, libName),
		ModelFile:     filepath.Join(modelSubDir, "model.onnx"),
		VocabFile:     filepath.Join(modelSubDir, "vocab.txt"),
		TokenizerFile: filepath.Join(modelSubDir, "tokenizer.json"),
	}
}

func ensureONNXRuntime(paths *ONNXPaths, logger *slog.Logger) error {
	if _, err := os.Stat(paths.RuntimeLib); err == nil {
		return nil
	}

	logger.Info("downloading ONNX Runtime (first run)...", "version", onnxRuntimeVersion)
	if err := os.MkdirAll(filepath.Dir(paths.RuntimeLib), 0o755); err != nil {
		return err
	}

	goos := runtime.GOOS
	goarch := runtime.GOARCH
	if goos == "darwin" {
		goos = "osx" // ONNX Runtime uses "osx" not "darwin" for macOS archives
		goarch = "x64"
		if runtime.GOARCH == "arm64" {
			goarch = "arm64"
		}
	} else {
		goos = "linux"
		goarch = "x64"
	}

	url := fmt.Sprintf(onnxRuntimeURLTemplate, onnxRuntimeVersion, goos, goarch, onnxRuntimeVersion)
	return downloadAndExtractLib(url, paths.RuntimeLib, logger)
}

func ensureONNXModel(paths *ONNXPaths, logger *slog.Logger) error {
	isLegacy := strings.Contains(paths.ModelFile, legacyModelName)
	if isLegacy {
		if fileExists(paths.ModelFile) && (fileExists(paths.VocabFile) || fileExists(paths.TokenizerFile)) {
			return nil
		}
	} else {
		if fileExists(paths.ModelFile) && fileExists(paths.TokenizerFile) {
			return nil
		}
	}

	activeModel := onnxModelName
	if isLegacy {
		activeModel = legacyModelName
	}
	logger.Info("downloading ONNX model (first run)...", "model", activeModel)
	if err := os.MkdirAll(filepath.Dir(paths.ModelFile), 0o755); err != nil {
		return err
	}

	modelURL, tokenizerURL, vocabURL := onnxAssetURLs(isLegacy)

	if !fileExists(paths.ModelFile) {
		if err := downloadFileWithProgress(modelURL, paths.ModelFile, logger); err != nil {
			return fmt.Errorf("download model: %w", err)
		}
	}

	if !fileExists(paths.TokenizerFile) {
		if err := downloadFileWithProgress(tokenizerURL, paths.TokenizerFile, logger); err != nil {
			logger.Warn("tokenizer.json download failed, will use vocab.txt fallback", "error", err)
		}
	}

	if !fileExists(paths.VocabFile) {
		if err := downloadFileWithProgress(vocabURL, paths.VocabFile, logger); err != nil {
			if !fileExists(paths.TokenizerFile) {
				return fmt.Errorf("download vocab: %w", err)
			}
			logger.Warn("vocab.txt download failed, using tokenizer.json only", "error", err)
		}
	}

	return nil
}

func onnxAssetURLs(legacy bool) (model, tokenizer, vocab string) {
	base := onnxModelBaseURL
	if legacy {
		base = onnxLegacyModelBaseURL
		return base + "/onnx/model.onnx", base + "/tokenizer.json", base + "/vocab.txt"
	}
	return base + "/onnx/model.onnx", base + "/tokenizer.json", base + "/onnx/vocab.txt"
}

func downloadFile(url, destPath string, logger *slog.Logger) error {
	return downloadFileWithProgress(url, destPath, logger)
}

func downloadFileWithProgress(url, destPath string, logger *slog.Logger) error {
	const maxRetries = 3
	const progressInterval = 10 * 1024 * 1024

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		logger.Info("downloading", "url", url, "dest", filepath.Base(destPath), "attempt", attempt)

		var existingSize int64
		tmpPath := destPath + ".download"
		if info, err := os.Stat(tmpPath); err == nil {
			existingSize = info.Size()
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}
		if existingSize > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
		}

		client := &http.Client{Timeout: 10 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			continue
		}

		f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			resp.Body.Close()
			return err
		}
		if existingSize > 0 && resp.StatusCode == http.StatusPartialContent {
			f.Seek(existingSize, io.SeekStart)
		} else {
			f.Truncate(0)
			f.Seek(0, io.SeekStart)
			existingSize = 0
		}

		var totalWritten int64
		nextProgress := progressInterval
		buf := make([]byte, 32*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				written, writeErr := f.Write(buf[:n])
				if writeErr != nil {
					f.Close()
					resp.Body.Close()
					os.Remove(tmpPath)
					lastErr = writeErr
					break
				}
				totalWritten += int64(written)
				if totalWritten+existingSize >= int64(nextProgress) {
					logger.Info("download progress",
						"file", filepath.Base(destPath),
						"bytes", fmt.Sprintf("%d MB", (totalWritten+existingSize)/1024/1024),
					)
					nextProgress += progressInterval
				}
			}
			if readErr == io.EOF {
				f.Close()
				resp.Body.Close()
				return os.Rename(tmpPath, destPath)
			}
			if readErr != nil {
				f.Close()
				resp.Body.Close()
				lastErr = readErr
				break
			}
		}

		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}

	return fmt.Errorf("download failed after %d attempts: %w", maxRetries, lastErr)
}

func downloadAndExtractLib(tgzURL, destPath string, logger *slog.Logger) error {
	tmpTgz := destPath + ".tgz"
	if err := downloadFile(tgzURL, tmpTgz, logger); err != nil {
		return err
	}
	defer os.Remove(tmpTgz)

	return extractLibFromTgz(tmpTgz, destPath)
}

func extractLibFromTgz(tgzPath, destPath string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}

		name := hdr.Name
		if !strings.Contains(name, "/lib/") {
			continue
		}
		base := filepath.Base(name)
		if !strings.HasPrefix(base, "libonnxruntime.so") && !strings.HasPrefix(base, "libonnxruntime.dylib") {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		tmpPath := destPath + ".extracting"
		out, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(tr, 200*1024*1024))
		out.Close()
		if err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("extract lib: %w", err)
		}
		if err := os.Chmod(tmpPath, 0o755); err != nil {
			os.Remove(tmpPath)
			return err
		}
		return os.Rename(tmpPath, destPath)
	}

	return fmt.Errorf("libonnxruntime not found in archive")
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
