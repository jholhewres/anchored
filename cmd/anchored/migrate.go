package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jholhewres/anchored/pkg/config"
)

// `anchored migrate --remote <url>` moves this local database to a hosted
// Anchored OSS server.
//
// It PUSHES rather than uploading the SQLite file: the server never opens a
// database it did not create, the transfer is resumable, and it reuses the
// authentication the remote already has. See ADR-05 in the server's plan.
//
// Two things this command will not do:
//
//   - write anything by default. The first run is a dry run and prints a
//     report; applying takes an explicit --apply.
//   - touch the local database. It only reads. Turning the local client off is
//     a separate, manual decision the user makes after checking the report.
const (
	migrateBatchSize   = 200
	migrateHTTPTimeout = 60 * time.Second
)

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	remote := fs.String("remote", "", "base URL of the Anchored OSS server (required)")
	token := fs.String("token", os.Getenv("ANCHORED_TOKEN"), "API token (or ANCHORED_TOKEN)")
	apply := fs.Bool("apply", false, "actually write; without it the run is a dry run")
	dbPath := fs.String("db", "", "path to the local database (default: from config)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: anchored migrate --remote <url> [--apply]

Moves this local database to a hosted Anchored OSS server.

The first run is a DRY RUN: it reports how many memories, projects, triples and
task threads would be transferred, how each local project path maps to a remote
project, and how many embedding calls the import will cost. Nothing is written
until you pass --apply.

This command only READS the local database. Switching off the local client is a
separate decision, and one worth making only after reading the report.

Flags:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if strings.TrimSpace(*remote) == "" {
		fmt.Fprintln(os.Stderr, "anchored migrate: --remote is required")
		fs.Usage()
		os.Exit(1)
	}
	if strings.TrimSpace(*token) == "" {
		fmt.Fprintln(os.Stderr, "anchored migrate: a token is required (--token or ANCHORED_TOKEN)")
		os.Exit(1)
	}

	path := *dbPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "anchored migrate: resolve home: %v\n", err)
			os.Exit(1)
		}
		cfg, err := config.Load(filepath.Join(home, ".anchored", "config.yaml"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "anchored migrate: load config: %v\n", err)
			os.Exit(1)
		}
		path = cfg.Memory.DatabasePath
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anchored migrate: open %s: %v\n", path, err)
		os.Exit(1)
	}
	defer db.Close()

	m := &migrator{
		db:     db,
		client: &http.Client{Timeout: migrateHTTPTimeout},
		base:   strings.TrimRight(*remote, "/"),
		token:  *token,
		apply:  *apply,
	}
	if err := m.run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "anchored migrate: %v\n", err)
		os.Exit(1)
	}
}

type migrator struct {
	db     *sql.DB
	client *http.Client
	base   string
	token  string
	apply  bool
}

func (m *migrator) run(ctx context.Context) error {
	projects, err := m.readProjects(ctx)
	if err != nil {
		return err
	}

	batchID, mapping, err := m.openBatch(ctx, projects)
	if err != nil {
		return err
	}

	mode := "DRY RUN"
	if m.apply {
		mode = "APPLYING"
	}
	fmt.Printf("anchored migrate — %s\n  remote: %s\n  batch:  %s\n\n", mode, m.base, batchID)

	fmt.Println("Project mapping:")
	for _, entry := range mapping {
		local, _ := entry["local_path"].(string)
		resolution, _ := entry["resolution"].(string)
		remoteKey, _ := entry["remote_key"].(string)
		switch {
		case remoteKey == "":
			// Said plainly: these memories will not be attached to any project.
			fmt.Printf("  %-45s → workspace scope (no git remote)\n", local)
		default:
			fmt.Printf("  %-45s → %s (%s)\n", local, remoteKey, resolution)
		}
	}
	fmt.Println()

	total, err := m.pushMemories(ctx, batchID)
	if err != nil {
		return err
	}
	triples, err := m.pushTriples(ctx, batchID)
	if err != nil {
		return err
	}
	threads, err := m.pushThreads(ctx, batchID)
	if err != nil {
		return err
	}

	report, err := m.commit(ctx, batchID)
	if err != nil {
		return err
	}

	fmt.Printf("Memories: %d sent, %d created, %d already present, %d rejected\n",
		total.sent, total.created, total.skipped, total.rejected)
	fmt.Printf("Triples: %d · Task threads: %d\n", triples, threads)
	// The cost is stated because it is real and paid by the user: local vectors
	// are 384-wide and the server's are 3072, so nothing can be reused.
	//
	// The number comes from the SERVER, not from the count of created memories:
	// a deployment configured without an embedding provider queues nothing, and
	// quoting a cost it will never charge would be a plain lie.
	fmt.Printf("Embedding calls this import will cost: %d\n", total.embedding)
	for _, e := range report.errs {
		fmt.Printf("  ! %s\n", e)
	}

	if !m.apply {
		fmt.Println("\nNothing was written. Re-run with --apply to perform the import.")
		return nil
	}
	fmt.Println("\nImport complete. Your local database was NOT modified —")
	fmt.Println("verify the hosted copy before switching the local client off.")
	return nil
}

// readProjects reads the local project table and resolves each path's git
// origin HERE, on the machine that has the filesystem. The server cannot: it
// has no checkout to inspect.
func (m *migrator) readProjects(ctx context.Context) ([]map[string]any, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, name, path FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	defer rows.Close()

	var out []map[string]any
	for rows.Next() {
		var id, name, path string
		if err := rows.Scan(&id, &name, &path); err != nil {
			return nil, err
		}
		entry := map[string]any{"local_path": path, "name": name}
		if origin := gitOrigin(path); origin != "" {
			entry["remote_key"] = origin
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// gitOrigin asks git for the remote of a checkout. An empty result is not an
// error: a directory that is not a repository is a legitimate case, and those
// memories land at workspace scope.
func gitOrigin(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return ""
	}
	cmd := exec.Command("git", "-C", path, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type pushTotals struct{ sent, created, skipped, rejected, embedding int }

func (m *migrator) pushMemories(ctx context.Context, batchID string) (pushTotals, error) {
	var totals pushTotals

	rows, err := m.db.QueryContext(ctx, `
		SELECT m.id, m.category, m.content, coalesce(m.source,''), coalesce(m.source_id,''),
		       coalesce(m.keywords,''), coalesce(m.metadata,''),
		       m.created_at, m.updated_at, m.last_accessed_at, m.access_count, m.deleted_at,
		       coalesce(p.path, '')
		FROM memories m
		LEFT JOIN projects p ON p.id = m.project_id
		ORDER BY m.created_at`)
	if err != nil {
		return totals, fmt.Errorf("read memories: %w", err)
	}
	defer rows.Close()

	var batch []map[string]any
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		res, err := m.sendRecords(ctx, batchID, map[string]any{"memories": batch})
		if err != nil {
			return err
		}
		totals.sent += len(batch)
		totals.created += res.created
		totals.skipped += res.skipped
		totals.rejected += res.rejected
		totals.embedding += res.embedding
		fmt.Printf("\r  memories: %d sent…", totals.sent)
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		var (
			id, category, content, source, sourceID, keywords, metadata, projectPath string
			createdAt, updatedAt                                                     sql.NullTime
			lastAccessed, deletedAt                                                  sql.NullTime
			accessCount                                                              int
		)
		if err := rows.Scan(&id, &category, &content, &source, &sourceID, &keywords,
			&metadata, &createdAt, &updatedAt, &lastAccessed, &accessCount, &deletedAt,
			&projectPath); err != nil {
			return totals, err
		}

		rec := map[string]any{
			"local_id": id, "category": category, "content": content,
			"source": source, "source_id": sourceID, "access_count": accessCount,
			"local_path": projectPath,
		}
		if keywords != "" {
			// The local column is a comma-joined string.
			rec["keywords"] = strings.Split(keywords, ",")
		}
		if metadata != "" {
			var meta any
			if err := json.Unmarshal([]byte(metadata), &meta); err == nil {
				rec["metadata"] = meta
			}
		}
		// Timestamps are sent as they are stored. This is the whole point of the
		// command: history has to arrive as history, or the server's temporal
		// decay treats years of memory as if it were written today.
		if createdAt.Valid {
			rec["created_at"] = createdAt.Time.UTC()
		}
		if updatedAt.Valid {
			rec["updated_at"] = updatedAt.Time.UTC()
		}
		if lastAccessed.Valid {
			rec["last_accessed"] = lastAccessed.Time.UTC()
		}
		if deletedAt.Valid {
			rec["deleted_at"] = deletedAt.Time.UTC()
		}

		batch = append(batch, rec)
		if len(batch) >= migrateBatchSize {
			if err := flush(); err != nil {
				return totals, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return totals, err
	}
	if err := flush(); err != nil {
		return totals, err
	}
	if totals.sent > 0 {
		fmt.Println()
	}
	return totals, nil
}

func (m *migrator) pushTriples(ctx context.Context, batchID string) (int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT s.name, pr.name, o.name, t.confidence, t.valid_from, t.valid_to,
		       coalesce(p.path, '')
		FROM kg_triples t
		JOIN kg_entities s   ON s.id = t.subject_id
		JOIN kg_predicates pr ON pr.id = t.predicate_id
		JOIN kg_entities o   ON o.id = t.object_id
		LEFT JOIN projects p ON p.id = t.project_id`)
	if err != nil {
		// A local database from an older version may not have the graph tables.
		// That is not a reason to fail the whole migration.
		return 0, nil
	}
	defer rows.Close()

	var batch []map[string]any
	sent := 0
	for rows.Next() {
		var subject, predicate, object, projectPath string
		var confidence float64
		var validFrom, validTo sql.NullTime
		if err := rows.Scan(&subject, &predicate, &object, &confidence,
			&validFrom, &validTo, &projectPath); err != nil {
			return sent, err
		}
		rec := map[string]any{
			"subject": subject, "predicate": predicate, "object": object,
			"confidence": confidence, "local_path": projectPath,
		}
		// The validity window travels: without it a superseded fact would be
		// re-imported as current and a chain of corrections would collapse.
		if validFrom.Valid {
			rec["valid_from"] = validFrom.Time.UTC()
		}
		if validTo.Valid {
			rec["valid_to"] = validTo.Time.UTC()
		}
		batch = append(batch, rec)

		if len(batch) >= migrateBatchSize {
			if _, err := m.sendRecords(ctx, batchID, map[string]any{"triples": batch}); err != nil {
				return sent, err
			}
			sent += len(batch)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if _, err := m.sendRecords(ctx, batchID, map[string]any{"triples": batch}); err != nil {
			return sent, err
		}
		sent += len(batch)
	}
	return sent, nil
}

func (m *migrator) pushThreads(ctx context.Context, batchID string) (int, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT task_key, coalesce(external_ref,''), coalesce(status,'active'), coalesce(journal,'[]')
		FROM task_threads`)
	if err != nil {
		return 0, nil // older database without task threads
	}
	defer rows.Close()

	var batch []map[string]any
	for rows.Next() {
		var key, ref, status, journal string
		if err := rows.Scan(&key, &ref, &status, &journal); err != nil {
			return 0, err
		}
		rec := map[string]any{"task_key": key, "external_ref": ref, "status": status}
		var notes []string
		if err := json.Unmarshal([]byte(journal), &notes); err == nil && len(notes) > 0 {
			rec["journal"] = notes
		}
		batch = append(batch, rec)
	}
	if len(batch) == 0 {
		return 0, nil
	}
	if _, err := m.sendRecords(ctx, batchID, map[string]any{"threads": batch}); err != nil {
		return 0, err
	}
	return len(batch), nil
}

// --- transport --------------------------------------------------------------

type recordResult struct{ created, skipped, rejected, embedding int }

func (m *migrator) openBatch(ctx context.Context, projects []map[string]any) (string, []map[string]any, error) {
	var out struct {
		BatchID  string           `json:"batch_id"`
		Projects []map[string]any `json:"projects"`
	}
	if err := m.post(ctx, "/v1/import/batches", map[string]any{
		"dry_run": !m.apply, "projects": projects,
	}, &out); err != nil {
		return "", nil, fmt.Errorf("open batch: %w", err)
	}
	return out.BatchID, out.Projects, nil
}

func (m *migrator) sendRecords(ctx context.Context, batchID string, body map[string]any) (recordResult, error) {
	var out struct {
		Created   int      `json:"created"`
		Skipped   int      `json:"skipped"`
		Rejected  int      `json:"rejected"`
		Embedding int      `json:"embeddings_queued"`
		Errors    []string `json:"errors"`
	}
	if err := m.post(ctx, "/v1/import/batches/"+batchID+"/records", body, &out); err != nil {
		return recordResult{}, err
	}
	return recordResult{
		created: out.Created, skipped: out.Skipped, rejected: out.Rejected,
		embedding: out.Embedding,
	}, nil
}

type commitReport struct{ errs []string }

func (m *migrator) commit(ctx context.Context, batchID string) (commitReport, error) {
	var out struct {
		Errors []string `json:"errors"`
	}
	if err := m.post(ctx, "/v1/import/batches/"+batchID+"/commit", map[string]any{}, &out); err != nil {
		return commitReport{}, fmt.Errorf("commit: %w", err)
	}
	return commitReport{errs: out.Errors}, nil
}

func (m *migrator) post(ctx context.Context, path string, body, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Bounded: a misconfigured --remote could point at anything.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s: %s: %s", path, resp.Status, strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}
