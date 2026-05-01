package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func setupEntityTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			path TEXT UNIQUE NOT NULL,
			source_tool TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS memories (
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
		);
	`)
	if err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func TestEntityDetector_Detect_EmptySnapshot(t *testing.T) {
	db := setupEntityTestDB(t)
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)

	result := d.Detect("some text with anchore entity")
	if len(result) != 0 {
		t.Errorf("expected no entities from empty DB, got %v", result)
	}
}

func TestEntityDetector_Detect_ProjectNames(t *testing.T) {
	db := setupEntityTestDB(t)

	_, err := db.Exec(`INSERT INTO projects (id, name, path) VALUES ('p1', 'Anchored', '/tmp/anchored')`)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	ctx := context.Background()
	if err := d.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect("I'm working on the Anchored project")
	found := false
	for _, e := range result {
		if e == "Anchored" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected to find 'Anchored' in %v", result)
	}
}

func TestEntityDetector_Detect_Keywords(t *testing.T) {
	db := setupEntityTestDB(t)

	kws, _ := json.Marshal([]string{"kubernetes", "docker", "deployment"})
	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords) VALUES ('m1', 'fact', 'deploy stuff', ?)`, string(kws))
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect("how to set up kubernetes with docker")
	if len(result) < 2 {
		t.Errorf("expected at least 2 entities, got %v", result)
	}
}

func TestEntityDetector_Detect_ContentTokens(t *testing.T) {
	db := setupEntityTestDB(t)

	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords) VALUES ('m2', 'fact', 'The Prometheus plan uses Grafana for monitoring', NULL)`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect("monitoring with Grafana dashboards")
	found := false
	for _, e := range result {
		if e == "Grafana" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected to find 'Grafana' in %v", result)
	}
}

func TestEntityDetector_Detect_Deduplicates(t *testing.T) {
	db := setupEntityTestDB(t)

	_, err := db.Exec(`INSERT INTO projects (id, name, path) VALUES ('p2', 'DevClaw', '/tmp/devclaw')`)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect("DevClaw DevClaw DevClaw")
	count := 0
	for _, e := range result {
		if e == "DevClaw" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 occurrence of DevClaw, got %d", count)
	}
}

func TestEntityDetector_Detect_SkipsStopWords(t *testing.T) {
	db := setupEntityTestDB(t)

	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords) VALUES ('m3', 'fact', 'the quick brown fox', '["that","with","have"]')`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect("that with have")
	if len(result) != 0 {
		t.Errorf("expected no entities from stop words, got %v", result)
	}
}

func TestEntityDetector_CacheTTL(t *testing.T) {
	db := setupEntityTestDB(t)

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: 50 * time.Millisecond}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if d.snapshotAge() > 50*time.Millisecond {
		t.Error("snapshot should be fresh")
	}

	time.Sleep(60 * time.Millisecond)

	if d.snapshotAge() < 50*time.Millisecond {
		t.Error("snapshot should be stale after TTL")
	}
}

func TestEntityDetector_MaxTokens(t *testing.T) {
	db := setupEntityTestDB(t)

	for i := 0; i < 50; i++ {
		name := string(rune('a'+i%26)) + string(rune('a'+(i+1)%26)) + string(rune('a'+(i+2)%26)) + string(rune('0'+i%10))
		_, err := db.Exec(`INSERT INTO projects (id, name, path) VALUES (?, ?, ?)`,
			"p"+string(rune('0'+i%10))+string(rune('0'+i/10)), name, "/tmp/"+name+"-"+string(rune('0'+i)))
		if err != nil {
			t.Fatalf("insert project %d: %v", i, err)
		}
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour, MaxTokens: 5}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect("abc def ghi jkl mno pqr stu vwx yz0")
	if len(result) > 5 {
		t.Errorf("expected at most 5 entities (MaxTokens), got %d", len(result))
	}
}

func TestNormalizeEntity(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"Anchored", "anchored"},
		{"São Paulo", "são paulo"},
		{"Kubernetes", "kubernetes"},
		{"my-project", "my-project"},
	}
	for _, tt := range tests {
		got := normalizeEntity(tt.input)
		if got != tt.want {
			t.Errorf("normalizeEntity(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestContainsEntity(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		keywords []string
		entities []string
		want     bool
	}{
		{
			name:     "content match",
			content:  "Use Kubernetes for deployment",
			keywords: nil,
			entities: []string{"Kubernetes"},
			want:     true,
		},
		{
			name:     "keyword match",
			content:  "some content",
			keywords: []string{"docker", "devops"},
			entities: []string{"docker"},
			want:     true,
		},
		{
			name:     "no match",
			content:  "unrelated text",
			keywords: []string{"foo"},
			entities: []string{"bar"},
			want:     false,
		},
		{
			name:     "empty entities",
			content:  "anything",
			keywords: nil,
			entities: nil,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsEntity(tt.content, tt.keywords, tt.entities)
			if got != tt.want {
				t.Errorf("containsEntity() = %v, want %v", got, tt.want)
			}
		})
	}
}
