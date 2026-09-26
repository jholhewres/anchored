package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/jholhewres/anchored/pkg/eval"
	"github.com/jholhewres/anchored/pkg/memory"
)

// runEval dispatches the local evaluation gates. Each sub-eval prints a report
// and exits non-zero when it fails, so `make eval` (and CI) gate on them.
//
//	anchored eval recall|sync-safety|identity [--fixture PATH] [--json]
func runEval(args []string) {
	if len(args) == 0 {
		printEvalUsage()
		os.Exit(2)
	}
	sub := args[0]
	fs := newFlagSet("eval " + sub)
	fixture := fs.String("fixture", "", "path to a YAML fixture (defaults to the embedded fixture)")
	jsonOut := fs.Bool("json", false, "emit the report as JSON")
	real := fs.Bool("real", false, "recall: run through the production search path with the installed ONNX model")
	configPath := fs.String("config", "", "path to config file (real recall, embedding-health)")
	saveBaseline := fs.String("save-baseline", "", "recall --real: write the result as a baseline JSON file")
	compare := fs.String("compare", "", "recall --real: compare against a baseline JSON file")
	minGain := fs.Float64("min-gain", 1.0, "recall --real --compare: required ratio over the baseline for recall@5 and mrr@10")
	pairs := fs.Int("pairs", 1500, "embedding-health: random vector pairs to sample")
	maxMean := fs.Float64("max-mean", 0, "embedding-health: fail when the mean cosine exceeds this (0 = report only)")
	runs := fs.Int("runs", 5, "recall --real: repetitions of the query set, averaged (the v0.19 ranking is not deterministic)")
	label := fs.String("label", "", "recall --real: describe what this run measures (stored in a saved baseline)")
	fs.Parse(args[1:])

	if sub == "recall" && *real {
		os.Exit(runEvalRecallReal(*configPath, *fixture, *saveBaseline, *compare, *label, *minGain, *runs, *jsonOut))
	}
	if sub == "embedding-health" {
		os.Exit(runEvalEmbeddingHealth(*configPath, *pairs, *maxMean, *jsonOut))
	}

	var (
		report eval.Report
		err    error
	)
	switch sub {
	case "recall":
		report, err = runEvalRecall(*fixture)
	case "sync-safety":
		var data []byte
		if data, err = eval.FixtureBytes(*fixture, "privacy.yaml"); err == nil {
			report, err = eval.RunSyncSafety(data)
		}
	case "identity":
		var data []byte
		if data, err = eval.FixtureBytes(*fixture, "identity.yaml"); err == nil {
			report, err = eval.RunIdentity(data)
		}
	default:
		printEvalUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval %s: %v\n", sub, err)
		os.Exit(2)
	}

	emitEvalReport(report, *jsonOut)
	if !report.Passed {
		os.Exit(1)
	}
}

func runEvalRecall(fixturePath string) (eval.Report, error) {
	data, err := eval.FixtureBytes(fixturePath, "recall_basic.yaml")
	if err != nil {
		return eval.Report{}, err
	}
	// A throwaway BM25-only store (no embeddings, no network) keeps the eval
	// deterministic and offline.
	dir, err := os.MkdirTemp("", "anchored-eval-*")
	if err != nil {
		return eval.Report{}, err
	}
	defer os.RemoveAll(dir)

	store, err := memory.NewSQLiteStore(filepath.Join(dir, "recall.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return eval.Report{}, err
	}
	defer store.Close()

	return eval.RunRecall(context.Background(), store, data)
}

// runEvalRecallReal measures retrieval with the user's installed model and
// search settings over the synthetic recall_real fixture, in a throwaway
// database: nothing touches the real store, remotes or debug log.
func runEvalRecallReal(configPath, fixturePath, saveBaseline, comparePath, label string, minGain float64, runs int, asJSON bool) int {
	data, err := eval.FixtureBytes(fixturePath, "recall_real.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	dir, err := os.MkdirTemp("", "anchored-eval-real-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	cfg.Memory.StorageDir = dir
	cfg.Memory.DatabasePath = filepath.Join(dir, "recall.db")
	cfg.Remote.Enabled = false
	cfg.Remotes = nil
	cfg.Debug.Enabled = false
	cfg.Curation.Enabled = false

	svc, err := memory.NewService(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	defer svc.Close()
	if !svc.EmbeddingsEnabled() {
		fmt.Fprintf(os.Stderr, "eval recall --real: no embedding model available (provider %q, model_dir %s)\n",
			cfg.Embedding.Provider, cfg.Embedding.ModelDir)
		return 2
	}

	res, err := eval.RunRecallReal(context.Background(), svc, data, runs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	res.Model = cfg.Embedding.Model
	res.Label = label
	if res.Label == "" {
		res.Label = fmt.Sprintf("anchored %s · %s", Version, cfg.Embedding.Model)
	}
	h, err := eval.MeasureEmbeddingHealth(context.Background(), svc.StoreDB(), 500)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	res.Health = h
	if err := eval.CheckCoverage(h, res.Corpus); err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}

	if saveBaseline != "" {
		out, _ := json.MarshalIndent(res, "", "  ")
		if err := os.WriteFile(saveBaseline, append(out, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "eval recall --real: write baseline: %v\n", err)
			return 2
		}
	}
	if asJSON {
		out, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(out))
	} else {
		emitEvalReport(res.Report, false)
		fmt.Printf("  eval corpus vector space: %d vectors, mean cosine of random pairs %.3f\n", res.Health.Vectors, res.Health.MeanCosine)
	}
	if comparePath == "" {
		return 0
	}
	base, err := eval.LoadRealResult(comparePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	if err := eval.CheckComparable(res, base); err != nil {
		fmt.Fprintf(os.Stderr, "eval recall --real: %v\n", err)
		return 2
	}
	ok, detail := eval.CompareToBaseline(res.Metrics, base.Metrics, minGain)
	fmt.Printf("\nagainst %s (%s; its recall@5 ranged %.3f-%.3f over %d runs), min gain %.2fx:\n%s\n",
		comparePath, base.Label, base.RecallSpread[0], base.RecallSpread[1], base.Runs, minGain, detail)
	if !ok {
		return 1
	}
	return 0
}

// runEvalEmbeddingHealth samples the configured database's active vector
// space. It opens the database read-only and runs no migration.
func runEvalEmbeddingHealth(configPath string, pairs int, maxMean float64, asJSON bool) int {
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval embedding-health: %v\n", err)
		return 2
	}
	db, err := sql.Open("sqlite3", "file:"+cfg.Memory.DatabasePath+"?mode=ro")
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval embedding-health: %v\n", err)
		return 2
	}
	defer db.Close()
	h, err := eval.MeasureEmbeddingHealth(context.Background(), db, pairs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "eval embedding-health: %v\n", err)
		return 2
	}
	if asJSON {
		out, _ := json.MarshalIndent(h, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Printf("embedding health of %s\n  %d vectors, %d random pairs\n  mean cosine   %.3f\n  median cosine %.3f\n  pairs >= 0.75 %.1f%%\n",
			cfg.Memory.DatabasePath, h.Vectors, h.Pairs, h.MeanCosine, h.MedianCosine, h.ShareAbove075*100)
		fmt.Println("  (a healthy space sits around 0.1-0.3; 0.7+ means unrelated memories look alike)")
	}
	if maxMean > 0 && h.MeanCosine > maxMean {
		fmt.Fprintf(os.Stderr, "mean cosine %.3f exceeds %.3f\n", h.MeanCosine, maxMean)
		return 1
	}
	return 0
}

func emitEvalReport(r eval.Report, asJSON bool) {
	if asJSON {
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Println(string(b))
		return
	}
	status := "PASS"
	if !r.Passed {
		status = "FAIL"
	}
	fmt.Printf("[%s] %s — %s\n", status, r.Name, r.Summary)
	for _, c := range r.Cases {
		mark := "ok"
		if !c.Passed {
			mark = "XX"
		}
		fmt.Printf("  %s %s — %s\n", mark, c.Name, c.Detail)
	}
}

func printEvalUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  anchored eval recall      [--fixture PATH] [--json]   Recall@K over a seeded corpus")
	fmt.Fprintln(os.Stderr, "  anchored eval recall --real [--save-baseline F] [--compare F --min-gain X]")
	fmt.Fprintln(os.Stderr, "                                                        same, through the production search path and the installed model")
	fmt.Fprintln(os.Stderr, "  anchored eval embedding-health [--pairs N] [--max-mean X]  spread of the configured database's vector space (read-only)")
	fmt.Fprintln(os.Stderr, "  anchored eval sync-safety [--fixture PATH] [--json]   privacy filter blocks/redacts sensitive content")
	fmt.Fprintln(os.Stderr, "  anchored eval identity    [--fixture PATH] [--json]   remote-key derivation invariants")
}
