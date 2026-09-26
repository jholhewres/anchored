package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jholhewres/anchored/pkg/config"
	"github.com/jholhewres/anchored/pkg/memory"
)

// An installation still on the legacy (collapsed) space, with the v0.20
// replacement being built: doctor warns about the active one and shows the
// progress of the other.
func TestCheckEmbeddingGenerations_ReportsCollapseAndProgress(t *testing.T) {
	withDoctorChecks(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gen.db")
	store, err := memory.NewSQLiteStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"a", "b", "c"} {
		if err := store.Save(ctx, memory.Memory{ID: id, Category: "fact", Source: "test", Content: "memory " + id}); err != nil {
			t.Fatal(err)
		}
	}
	legacy := memory.EmbeddingIdentity{Provider: "onnx", Model: "m", ModelRevision: "legacy", Dimensions: 2, Normalization: "l2"}
	gen, err := store.EnsureEmbeddingGeneration(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	revs, err := store.ListMissingEmbeddingRevisions(ctx, gen.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range revs {
		// Every vector the same: a collapsed space.
		if err := store.PutEmbeddingVector(ctx, memory.EmbeddingVectorRecord{
			RevisionID: r.RevisionID, MemoryID: r.MemoryID, GenerationID: gen.ID,
			Purpose: memory.EmbeddingPurposeDocument, Identity: legacy,
			ContentHash: r.Memory.ContentHash, Vector: []float32{1, 0},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.ActivateEmbeddingGeneration(ctx, gen.ID); err != nil {
		t.Fatal(err)
	}
	v2 := legacy
	v2.ModelRevision = "v2"
	if _, err := store.EnsureEmbeddingGeneration(ctx, v2); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkEmbeddingGenerations(ctx, db, config.EmbeddingConfig{})

	var active, building *checkResult
	for i := range doctorChecks {
		c := &doctorChecks[i]
		switch {
		case strings.Contains(c.Name, " active:"):
			active = c
		case strings.Contains(c.Name, " building:"):
			building = c
		}
	}
	if active == nil || active.Status != "warn" || !strings.Contains(active.Detail, "collapsed") || !strings.Contains(active.FixCommand, "being built") {
		t.Fatalf("active generation check: %+v", active)
	}
	if building == nil || !strings.Contains(building.Name, "0/3") {
		t.Fatalf("building generation check: %+v", building)
	}
}
