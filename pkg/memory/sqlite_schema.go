package memory

import "strings"

func initSchema() string {
	return strings.Join([]string{
		`CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			category TEXT NOT NULL,
			content TEXT NOT NULL,
			keywords TEXT,
			embedding BLOB,
			source TEXT,
			source_id TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			access_count INTEGER DEFAULT 0,
			last_accessed_at DATETIME,
			metadata TEXT
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
			content,
			keywords,
			content='memories',
			content_rowid='rowid',
			tokenize='porter unicode61'
		)`,
		`CREATE TABLE IF NOT EXISTS embedding_cache (
			text_hash TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT 'all-MiniLM-L6-v2',
			embedding BLOB NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (text_hash, model)
		)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			path TEXT UNIQUE NOT NULL,
			source_tool TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS essential_stories (
			project_id TEXT PRIMARY KEY,
			story TEXT,
			source_memories TEXT,
			generated_at DATETIME,
			bytes INTEGER,
			schema_version INTEGER DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS kg_entities (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			project_id TEXT,
			embedding BLOB,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS kg_entity_aliases (
			entity_id TEXT NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
			alias TEXT NOT NULL,
			PRIMARY KEY (entity_id, alias)
		)`,
		`CREATE TABLE IF NOT EXISTS kg_predicates (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			is_functional BOOLEAN DEFAULT FALSE,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS kg_triples (
			id TEXT PRIMARY KEY,
			subject_id TEXT NOT NULL REFERENCES kg_entities(id),
			predicate_id TEXT NOT NULL REFERENCES kg_predicates(id),
			object_id TEXT NOT NULL REFERENCES kg_entities(id),
			confidence REAL DEFAULT 1.0,
			project_id TEXT,
			valid_from DATETIME DEFAULT CURRENT_TIMESTAMP,
			valid_to DATETIME,
			txn_time DATETIME DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS imports (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			path TEXT NOT NULL,
			memories_imported INTEGER DEFAULT 0,
			entities_imported INTEGER DEFAULT 0,
			status TEXT DEFAULT 'pending',
			started_at DATETIME,
			finished_at DATETIME,
			error TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			project_id TEXT,
			source TEXT NOT NULL,
			source_session_id TEXT,
			title TEXT,
			directory TEXT,
			created_at DATETIME,
			message_count INTEGER DEFAULT 0,
			imported_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS migrations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			applied_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TRIGGER IF NOT EXISTS memories_fts_insert AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content, keywords) VALUES (new.rowid, new.content, new.keywords);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_fts_update AFTER UPDATE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content, keywords) VALUES('delete', old.rowid, old.content, old.keywords);
			INSERT INTO memories_fts(rowid, content, keywords) VALUES (new.rowid, new.content, new.keywords);
		END`,
		`CREATE TRIGGER IF NOT EXISTS memories_fts_delete AFTER DELETE ON memories BEGIN
			INSERT INTO memories_fts(memories_fts, rowid, content, keywords) VALUES('delete', old.rowid, old.content, old.keywords);
		END`,
	}, ";\n") + ";"
}
