package memory

import (
	"database/sql"
	"fmt"

	ctxpkg "github.com/jholhewres/anchored/pkg/context"
)

type migration struct {
	Name string
	Up   string
}

func Migrate(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS migrations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}

	migrations := []migration{
		{Name: "001_initial_schema", Up: initSchema()},
		{Name: "002_indexed_files", Up: `CREATE TABLE IF NOT EXISTS indexed_files (
			path TEXT PRIMARY KEY,
			sha256 TEXT NOT NULL,
			indexed_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`},
		{Name: "003_content_hash_soft_delete", Up: `
			ALTER TABLE memories ADD COLUMN content_hash TEXT;
			ALTER TABLE memories ADD COLUMN deleted_at DATETIME;
			UPDATE memories SET content_hash = '' WHERE content_hash IS NULL;
			CREATE INDEX IF NOT EXISTS idx_memories_content_hash ON memories(content_hash, project_id);
			CREATE INDEX IF NOT EXISTS idx_memories_deleted_at ON memories(deleted_at);
		`},
		{Name: "004_cross_project_search", Up: `
			CREATE INDEX IF NOT EXISTS idx_memories_global_search ON memories(category, deleted_at);
			CREATE INDEX IF NOT EXISTS idx_memories_source ON memories(source);
		`},
		{Name: "005_live_sessions", Up: `
			ALTER TABLE sessions ADD COLUMN last_activity_at DATETIME;
			ALTER TABLE sessions ADD COLUMN ended_at DATETIME;
			ALTER TABLE sessions ADD COLUMN source_tool TEXT;
			ALTER TABLE sessions ADD COLUMN metadata TEXT;
			CREATE INDEX IF NOT EXISTS idx_sessions_source_session_id ON sessions(source_session_id);
			CREATE INDEX IF NOT EXISTS idx_sessions_last_activity ON sessions(last_activity_at);
		`},
		{Name: "006_auto_capture", Up: `
			CREATE INDEX IF NOT EXISTS idx_memories_source_type ON memories(source);
		`},
		{Name: "007_dream_system", Up: `
			CREATE TABLE IF NOT EXISTS dream_runs (
				id TEXT PRIMARY KEY,
				started_at DATETIME,
				finished_at DATETIME,
				config TEXT,
				memories_analyzed INTEGER DEFAULT 0,
				actions_proposed INTEGER DEFAULT 0,
				actions_applied INTEGER DEFAULT 0,
				status TEXT DEFAULT 'pending'
			);
			CREATE TABLE IF NOT EXISTS dream_actions (
				id TEXT PRIMARY KEY,
				run_id TEXT REFERENCES dream_runs(id),
				memory_id TEXT,
				related_memory_id TEXT,
				action_type TEXT,
				confidence REAL,
				reason TEXT,
				proposed_at DATETIME DEFAULT CURRENT_TIMESTAMP,
				applied_at DATETIME,
				status TEXT DEFAULT 'proposed'
			);
		`},
		{Name: "008_content_optimizer", Up: ctxpkg.MigrationSQL},
		{Name: "009_content_project_id", Up: ctxpkg.MigrationSQL009},
		{Name: "010_project_remote_key", Up: `
			ALTER TABLE projects ADD COLUMN remote_key TEXT;
			CREATE INDEX IF NOT EXISTS idx_projects_remote_key ON projects(remote_key);
		`},
		{Name: "011_sync_metadata", Up: `
			ALTER TABLE memories ADD COLUMN sync_dirty BOOLEAN DEFAULT FALSE;
			ALTER TABLE memories ADD COLUMN sync_origin TEXT DEFAULT 'local';
			ALTER TABLE memories ADD COLUMN author TEXT;
			ALTER TABLE memories ADD COLUMN remote_project_key TEXT;
			CREATE TABLE IF NOT EXISTS sync_state (
				project_id TEXT PRIMARY KEY,
				remote_project_key TEXT,
				watermark TEXT,
				last_sync DATETIME,
				client_id TEXT NOT NULL
			);
		`},
		{Name: "012_curation_state", Up: `
			CREATE TABLE IF NOT EXISTS curation_state (
				key TEXT PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
			);
		`},
		// Rebuild the FTS index with a multilingual tokenizer. The old
		// 'porter unicode61' applied English-only stemming to a PT/ES/EN corpus,
		// degrading lexical recall for non-English memories. Drop + recreate +
		// repopulate; the existing INSERT/UPDATE/DELETE triggers reference the
		// table by name and keep working after the rebuild.
		{Name: "013_fts_multilingual_tokenizer", Up: `
			DROP TABLE IF EXISTS memories_fts;
			CREATE VIRTUAL TABLE memories_fts USING fts5(
				content,
				keywords,
				content='memories',
				content_rowid='rowid',
				tokenize='unicode61 remove_diacritics 2'
			);
			INSERT INTO memories_fts(memories_fts) VALUES('rebuild');
		`},
		{Name: "014_artifact_store", Up: ctxpkg.MigrationSQL014},
		{Name: "015_working_sets", Up: ctxpkg.MigrationSQL015},
		{Name: "016_task_threads", Up: ctxpkg.MigrationSQL016},
		// Session Cockpit: richer session metadata (provider/model/intent for
		// distinguishing e.g. Claude Code vs Claude Code + GLM) and the
		// session<->task link. session_id is the PK, so a session maps to at
		// most one task at a time; task_key repeats, so a task accumulates many
		// sessions over its life.
		{Name: "017_session_cockpit", Up: `
			ALTER TABLE sessions ADD COLUMN provider TEXT;
			ALTER TABLE sessions ADD COLUMN model TEXT;
			ALTER TABLE sessions ADD COLUMN intent TEXT;
			CREATE TABLE IF NOT EXISTS session_task_link (
				session_id TEXT PRIMARY KEY,
				task_key   TEXT NOT NULL,
				linked_by  TEXT,
				linked_at  DATETIME DEFAULT CURRENT_TIMESTAMP
			);
			CREATE INDEX IF NOT EXISTS idx_session_task_link_task ON session_task_link(task_key);
		`},
		{Name: "018_memory_revision_ledger", Up: `
			ALTER TABLE memories ADD COLUMN logical_id TEXT;
			ALTER TABLE memories ADD COLUMN current_revision_id TEXT;

			CREATE TABLE IF NOT EXISTS memory_revisions (
				revision_id TEXT PRIMARY KEY,
				memory_id TEXT NOT NULL,
				logical_id TEXT NOT NULL,
				project_id TEXT,
				category TEXT NOT NULL,
				content TEXT NOT NULL,
				content_hash TEXT,
				keywords TEXT,
				source TEXT,
				source_id TEXT,
				metadata TEXT,
				memory_created_at DATETIME NOT NULL,
				memory_updated_at DATETIME NOT NULL,
				author TEXT,
				remote_project_key TEXT,
				temporal_mode TEXT NOT NULL
					CHECK (temporal_mode IN ('supersede', 'correct', 'tombstone')),
				is_tombstone BOOLEAN NOT NULL DEFAULT FALSE,
				valid_from INTEGER NOT NULL,
				valid_to INTEGER,
				system_from INTEGER NOT NULL,
				system_to INTEGER,
				created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				CHECK (valid_to IS NULL OR valid_from < valid_to),
				CHECK (system_to IS NULL OR system_from < system_to)
			);

			CREATE INDEX IF NOT EXISTS idx_memory_revisions_logical_time
				ON memory_revisions(logical_id, valid_from, valid_to, system_from, system_to);
			CREATE INDEX IF NOT EXISTS idx_memory_revisions_memory
				ON memory_revisions(memory_id, system_from);
			CREATE INDEX IF NOT EXISTS idx_memory_revisions_current
				ON memory_revisions(logical_id, valid_from)
				WHERE system_to IS NULL;

			UPDATE memories
			SET logical_id = id
			WHERE logical_id IS NULL OR logical_id = '';

			INSERT OR IGNORE INTO memory_revisions (
				revision_id, memory_id, logical_id, project_id, category,
				content, content_hash, keywords, source, source_id, metadata,
				memory_created_at, memory_updated_at, author, remote_project_key,
				temporal_mode, is_tombstone, valid_from, valid_to,
				system_from, system_to, created_at
			)
			SELECT
				'legacy:' || id || ':base',
				id,
				id,
				project_id,
				category,
				content,
				content_hash,
				keywords,
				source,
				source_id,
				metadata,
				COALESCE(created_at, CURRENT_TIMESTAMP),
				COALESCE(updated_at, created_at, CURRENT_TIMESTAMP),
				author,
				remote_project_key,
				'supersede',
				FALSE,
				CAST(strftime('%s', COALESCE(created_at, CURRENT_TIMESTAMP)) AS INTEGER) * 1000000000,
				CASE WHEN deleted_at IS NULL THEN NULL
					ELSE CAST(strftime('%s', deleted_at) AS INTEGER) * 1000000000 END,
				CAST(strftime('%s', COALESCE(created_at, CURRENT_TIMESTAMP)) AS INTEGER) * 1000000000,
				CASE WHEN deleted_at IS NULL THEN NULL
					ELSE CAST(strftime('%s', deleted_at) AS INTEGER) * 1000000000 END,
				COALESCE(created_at, CURRENT_TIMESTAMP)
			FROM memories
			WHERE deleted_at IS NULL
				OR CAST(strftime('%s', deleted_at) AS INTEGER) >
					CAST(strftime('%s', COALESCE(created_at, CURRENT_TIMESTAMP)) AS INTEGER);

			-- A legacy soft delete tells us both that the fact stopped being
			-- valid and that the deletion was learned at deleted_at. Carry the
			-- closed valid interval into the new system-time view so
			-- known_at-after-delete can still answer valid_at-before-delete.
			INSERT OR IGNORE INTO memory_revisions (
				revision_id, memory_id, logical_id, project_id, category,
				content, content_hash, keywords, source, source_id, metadata,
				memory_created_at, memory_updated_at, author, remote_project_key,
				temporal_mode, is_tombstone, valid_from, valid_to,
				system_from, system_to, created_at
			)
			SELECT
				'legacy:' || id || ':carry',
				id,
				id,
				project_id,
				category,
				content,
				content_hash,
				keywords,
				source,
				source_id,
				metadata,
				COALESCE(created_at, CURRENT_TIMESTAMP),
				COALESCE(updated_at, deleted_at, created_at, CURRENT_TIMESTAMP),
				author,
				remote_project_key,
				'supersede',
				FALSE,
				CAST(strftime('%s', COALESCE(created_at, CURRENT_TIMESTAMP)) AS INTEGER) * 1000000000,
				CAST(strftime('%s', deleted_at) AS INTEGER) * 1000000000,
				CAST(strftime('%s', deleted_at) AS INTEGER) * 1000000000,
				NULL,
				deleted_at
			FROM memories
			WHERE deleted_at IS NOT NULL
				AND CAST(strftime('%s', deleted_at) AS INTEGER) >
					CAST(strftime('%s', COALESCE(created_at, CURRENT_TIMESTAMP)) AS INTEGER);

			INSERT OR IGNORE INTO memory_revisions (
				revision_id, memory_id, logical_id, project_id, category,
				content, content_hash, keywords, source, source_id, metadata,
				memory_created_at, memory_updated_at, author, remote_project_key,
				temporal_mode, is_tombstone, valid_from, valid_to,
				system_from, system_to, created_at
			)
			SELECT
				'legacy:' || id || ':tombstone',
				id,
				id,
				project_id,
				category,
				content,
				content_hash,
				keywords,
				source,
				source_id,
				metadata,
				COALESCE(created_at, CURRENT_TIMESTAMP),
				COALESCE(updated_at, deleted_at, created_at, CURRENT_TIMESTAMP),
				author,
				remote_project_key,
				'tombstone',
				TRUE,
				CAST(strftime('%s', deleted_at) AS INTEGER) * 1000000000,
				NULL,
				CAST(strftime('%s', deleted_at) AS INTEGER) * 1000000000,
				NULL,
				deleted_at
			FROM memories
			WHERE deleted_at IS NOT NULL;

			UPDATE memories
			SET current_revision_id = CASE
				WHEN deleted_at IS NULL THEN 'legacy:' || id || ':base'
				ELSE 'legacy:' || id || ':tombstone'
			END
			WHERE current_revision_id IS NULL OR current_revision_id = '';

			CREATE INDEX IF NOT EXISTS idx_memories_logical_id ON memories(logical_id);
			CREATE INDEX IF NOT EXISTS idx_memories_current_revision_id ON memories(current_revision_id);
		`},
		{Name: "019_processing_jobs_remote_outbox", Up: `
			CREATE TABLE IF NOT EXISTS memory_processing_jobs (
				id TEXT PRIMARY KEY,
				revision_id TEXT NOT NULL,
				memory_id TEXT NOT NULL,
				kind TEXT NOT NULL,
				generation TEXT NOT NULL DEFAULT '',
				state TEXT NOT NULL DEFAULT 'pending'
					CHECK (state IN ('pending', 'processing', 'done', 'failed')),
				attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
				max_attempts INTEGER NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
				owner TEXT,
				lease_until INTEGER,
				next_attempt_at INTEGER,
				last_error TEXT,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				completed_at INTEGER,
				UNIQUE (revision_id, kind, generation)
			);
			CREATE INDEX IF NOT EXISTS idx_memory_processing_jobs_claim
				ON memory_processing_jobs(state, kind, next_attempt_at, lease_until, created_at);

			CREATE TABLE IF NOT EXISTS remote_outbox (
				operation_id TEXT PRIMARY KEY,
				memory_id TEXT NOT NULL,
				revision_id TEXT NOT NULL,
				remote TEXT NOT NULL,
				project TEXT NOT NULL,
				payload_hash TEXT NOT NULL,
				payload_snapshot BLOB NOT NULL,
				state TEXT NOT NULL DEFAULT 'pending'
					CHECK (state IN ('pending', 'processing', 'delivered', 'dead_letter')),
				attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
				max_attempts INTEGER NOT NULL DEFAULT 8 CHECK (max_attempts > 0),
				owner TEXT,
				lease_until INTEGER,
				next_attempt_at INTEGER,
				error_class TEXT,
				last_error TEXT,
				ack BLOB,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				delivered_at INTEGER
			);
			CREATE INDEX IF NOT EXISTS idx_remote_outbox_claim
				ON remote_outbox(state, next_attempt_at, lease_until, created_at);

			CREATE TRIGGER IF NOT EXISTS remote_outbox_envelope_immutable
			BEFORE UPDATE OF operation_id, memory_id, revision_id, remote, project,
				payload_hash, payload_snapshot ON remote_outbox
			BEGIN
				SELECT RAISE(ABORT, 'remote outbox envelope is immutable');
			END;
		`},
		{Name: "020_embedding_generations", Up: `
			CREATE TABLE IF NOT EXISTS embedding_generations (
				generation_id TEXT PRIMARY KEY,
				semantic_space_id TEXT NOT NULL UNIQUE,
				provider TEXT NOT NULL,
				model TEXT NOT NULL,
				model_revision TEXT NOT NULL DEFAULT '',
				dimensions INTEGER NOT NULL CHECK (dimensions > 0),
				normalization TEXT NOT NULL,
				state TEXT NOT NULL DEFAULT 'building'
					CHECK (state IN ('building', 'active', 'retired')),
				snapshot_at INTEGER NOT NULL,
				created_at INTEGER NOT NULL,
				activated_at INTEGER,
				retired_at INTEGER
			);
			CREATE UNIQUE INDEX IF NOT EXISTS idx_embedding_generations_one_active
				ON embedding_generations(state) WHERE state = 'active';

			CREATE TABLE IF NOT EXISTS memory_embedding_vectors (
				revision_id TEXT NOT NULL
					REFERENCES memory_revisions(revision_id) ON DELETE CASCADE,
				memory_id TEXT NOT NULL,
				generation_id TEXT NOT NULL
					REFERENCES embedding_generations(generation_id) ON DELETE CASCADE,
				semantic_space_id TEXT NOT NULL,
				purpose TEXT NOT NULL CHECK (purpose IN ('document', 'query')),
				provider TEXT NOT NULL,
				model TEXT NOT NULL,
				model_revision TEXT NOT NULL DEFAULT '',
				dimensions INTEGER NOT NULL CHECK (dimensions > 0),
				normalization TEXT NOT NULL,
				content_hash TEXT NOT NULL DEFAULT '',
				embedding BLOB NOT NULL,
				embedded_at INTEGER NOT NULL,
				PRIMARY KEY (revision_id, generation_id, purpose)
			);
			CREATE INDEX IF NOT EXISTS idx_memory_embedding_vectors_generation
				ON memory_embedding_vectors(generation_id, purpose, memory_id);

			CREATE TRIGGER IF NOT EXISTS embedding_generation_transition_guard
			BEFORE UPDATE OF state ON embedding_generations
			WHEN NOT (
				OLD.state = NEW.state OR
				(OLD.state = 'building' AND NEW.state = 'active') OR
				(OLD.state = 'active' AND NEW.state = 'retired') OR
				(OLD.state = 'retired' AND NEW.state = 'building')
			)
			BEGIN
				SELECT RAISE(ABORT, 'invalid embedding generation transition');
			END;
		`},
	}

	for _, m := range migrations {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM migrations WHERE name = ?", m.Name).Scan(&count)
		if err != nil {
			return fmt.Errorf("check migration %s: %w", m.Name, err)
		}
		if count > 0 {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %s: %w", m.Name, err)
		}

		if _, err := tx.Exec(m.Up); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}

		if _, err := tx.Exec("INSERT INTO migrations (name) VALUES (?)", m.Name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.Name, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
	}

	return nil
}
