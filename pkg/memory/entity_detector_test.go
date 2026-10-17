package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
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
			metadata TEXT,
			logical_id TEXT,
			current_revision_id TEXT
		);
	`)
	if err != nil {
		t.Fatalf("create tables: %v", err)
	}
	return db
}

func TestEntityDetector_Detect_EmptyLexicon(t *testing.T) {
	db := setupEntityTestDB(t)
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)

	result := d.Detect(context.Background(), "some text with anchore entity")
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

	result := d.Detect(context.Background(), "I'm working on the Anchored project")
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
	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES ('m1', 'fact', 'deploy stuff', ?, ?1, ?1)`, string(kws))
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect(context.Background(), "how to set up kubernetes with docker")
	if len(result) < 2 {
		t.Errorf("expected at least 2 entities, got %v", result)
	}
}

func TestEntityDetector_Detect_ContentTokens(t *testing.T) {
	db := setupEntityTestDB(t)

	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES ('m2', 'fact', 'The Prometheus plan uses Grafana for monitoring', NULL, 'm2', 'm2')`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect(context.Background(), "monitoring with Grafana dashboards")
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

	result := d.Detect(context.Background(), "DevClaw DevClaw DevClaw")
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

	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES ('m3', 'fact', 'the quick brown fox', '["that","with","have"]', 'm3', 'm3')`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect(context.Background(), "that with have")
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

	if d.sinceLastLoadAttempt() > 50*time.Millisecond {
		t.Error("lexicon should be fresh")
	}

	time.Sleep(60 * time.Millisecond)

	if d.sinceLastLoadAttempt() < 50*time.Millisecond {
		t.Error("lexicon should be stale after TTL")
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

	result := d.Detect(context.Background(), "abc def ghi jkl mno pqr stu vwx yz0")
	if len(result) > 5 {
		t.Errorf("expected at most 5 entities (MaxTokens), got %d", len(result))
	}
}

func TestEntityDetector_Detect_LoadsOnFirstCall(t *testing.T) {
	db := setupEntityTestDB(t)

	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES ('m1', 'fact', 'deploy stuff', '["kubernetes"]', 'm1', 'm1')`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)

	result := d.Detect(context.Background(), "how to run kubernetes")
	if len(result) != 1 || result[0] != "kubernetes" {
		t.Errorf("expected the first call to load and find kubernetes, got %v", result)
	}
}

// TestEntityDetector_Detect_WaitsForFirstLoadInFlight locks the single-flight
// first load: a search that arrives while the first load is running waits for
// it instead of returning with an empty lexicon.
func TestEntityDetector_Detect_WaitsForFirstLoadInFlight(t *testing.T) {
	db := setupEntityTestDB(t)
	_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES ('m1', 'fact', 'deploy stuff', '["kubernetes"]', 'm1', 'm1')`)
	if err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)

	release := holdFirstLoad(d)
	done := make(chan []string, 1)
	go func() { done <- d.Detect(context.Background(), "how to run kubernetes") }()

	select {
	case got := <-done:
		release()
		t.Fatalf("Detect returned %v before the first load finished", got)
	case <-time.After(50 * time.Millisecond):
	}
	release()

	if got := <-done; len(got) != 1 || got[0] != "kubernetes" {
		t.Fatalf("Detect after the first load = %v, want [kubernetes]", got)
	}
}

// TestEntityDetector_Detect_FirstLoadWaitHonorsContext locks the bound on that
// wait: a search whose context ends while the first load is still running
// returns without entities instead of waiting for the load.
func TestEntityDetector_Detect_FirstLoadWaitHonorsContext(t *testing.T) {
	db := setupEntityTestDB(t)
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)

	release := holdFirstLoad(d)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan []string, 1)
	go func() { done <- d.Detect(ctx, "how to run kubernetes") }()

	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("Detect = %v, want no entities while the first load runs", got)
		}
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("Detect kept waiting for the first load after its context ended")
	}
}

func TestEntityDetector_Refresh_ReadsOnlyNewMemories(t *testing.T) {
	db := setupEntityTestDB(t)
	// Keep m1 out of the newest rows every load reads again.
	prev := entityRecentLimit
	entityRecentLimit = 1
	t.Cleanup(func() { entityRecentLimit = prev })

	insert := func(id, keywords string) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES (?1, 'fact', 'note', ?2, ?1, ?1)`, id, keywords)
		if err != nil {
			t.Fatalf("insert memory %s: %v", id, err)
		}
	}
	insert("m1", `["kubernetes"]`)

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Rewriting an already-read memory is invisible to the next load: only
	// rows past the watermark are scanned.
	if _, err := db.Exec(`UPDATE memories SET keywords = '["terraform"]' WHERE id = 'm1'`); err != nil {
		t.Fatalf("update memory: %v", err)
	}
	insert("m2", `["grafana"]`)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	result := d.Detect(context.Background(), "kubernetes grafana terraform")
	want := []string{"kubernetes", "grafana"}
	if len(result) != len(want) || result[0] != want[0] || result[1] != want[1] {
		t.Errorf("Detect() = %v, want %v", result, want)
	}
}

// TestEntityDetector_Refresh_ReadsMemoryThatReusedARowid covers a memory saved
// after the newest one was deleted: SQLite hands it the freed rowid, which sits
// at the watermark, so the incremental read alone would never see it.
func TestEntityDetector_Refresh_ReadsMemoryThatReusedARowid(t *testing.T) {
	db := setupEntityTestDB(t)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	insert := `INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES (?1, 'fact', 'note', ?2, ?1, ?1)`
	exec(insert, "m1", `["kubernetes"]`)

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	exec(`DELETE FROM memories WHERE id = 'm1'`)
	exec(insert, "m2", `["terraform"]`)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := d.Detect(context.Background(), "plan with terraform"); len(got) != 1 || got[0] != "terraform" {
		t.Fatalf("Detect = %v, want [terraform]", got)
	}
}

// TestEntityDetector_Detect_RefreshesInTheBackground locks the steady-state
// cost of a search: once the lexicon is loaded, a search past the TTL answers
// from the lexicon it has and the refresh lands right after, off its path.
func TestEntityDetector_Detect_RefreshesInTheBackground(t *testing.T) {
	db := setupEntityTestDB(t)
	insert := `INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES (?1, 'fact', 'note', ?2, ?1, ?1)`
	if _, err := db.Exec(insert, "m1", `["kubernetes"]`); err != nil {
		t.Fatalf("insert m1: %v", err)
	}
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: 20 * time.Millisecond}, nil)
	t.Cleanup(d.Close)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := db.Exec(insert, "m2", `["grafana"]`); err != nil {
		t.Fatalf("insert m2: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	if got := d.Detect(context.Background(), "grafana"); len(got) != 0 {
		t.Fatalf("Detect past the TTL = %v: the search waited for the refresh", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(d.Detect(context.Background(), "grafana")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never added grafana")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEntityDetector_Close_StopsBackgroundRefresh locks the refresh's owner:
// once the detector is closed, a search past the TTL starts no refresh, so
// nothing reads the store after its service closed it.
func TestEntityDetector_Close_StopsBackgroundRefresh(t *testing.T) {
	db := setupEntityTestDB(t)
	insert := `INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES (?1, 'fact', 'note', ?2, ?1, ?1)`
	if _, err := db.Exec(insert, "m1", `["kubernetes"]`); err != nil {
		t.Fatalf("insert m1: %v", err)
	}
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: 20 * time.Millisecond}, nil)
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := db.Exec(insert, "m2", `["grafana"]`); err != nil {
		t.Fatalf("insert m2: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	d.Close()
	d.mu.RLock()
	attemptedAt := d.lastAttemptAt
	d.mu.RUnlock()
	d.Detect(context.Background(), "grafana")
	time.Sleep(100 * time.Millisecond)
	if got := d.Detect(context.Background(), "grafana"); len(got) != 0 {
		t.Fatalf("Detect after Close = %v: a refresh ran after the detector closed", got)
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.lastAttemptAt.Equal(attemptedAt) {
		t.Fatal("a refresh was attempted after the detector closed")
	}
}

// TestEntityDetector_CancelledFirstLoadIsRetried locks the first load against
// a search that gives up mid-load: the next search loads at once instead of
// running with an empty lexicon until the TTL ends.
func TestEntityDetector_CancelledFirstLoadIsRetried(t *testing.T) {
	db := setupEntityTestDB(t)
	if _, err := db.Exec(`INSERT INTO memories (id, category, content, keywords, logical_id, current_revision_id) VALUES ('m1', 'fact', 'note', '["kubernetes"]', 'm1', 'm1')`); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled caller may win the load slot or give up before it; try
	// enough times that one of them runs the load under the dead context.
	for i := 0; i < 20; i++ {
		d.Detect(cancelled, "kubernetes")
	}

	if got := d.Detect(context.Background(), "kubernetes"); len(got) != 1 || got[0] != "kubernetes" {
		t.Fatalf("Detect after a cancelled first load = %v, want [kubernetes]", got)
	}
}

func TestEntityDetector_FailedLoadWaitsForTTL(t *testing.T) {
	db := setupEntityTestDB(t)
	if _, err := db.Exec(`DROP TABLE memories`); err != nil {
		t.Fatalf("drop memories: %v", err)
	}

	d := NewEntityDetector(db, EntityDetectorConfig{CacheTTL: time.Hour}, nil)
	if result := d.Detect(context.Background(), "anything here"); len(result) != 0 {
		t.Fatalf("expected no entities when the load fails, got %v", result)
	}
	if age := d.sinceLastLoadAttempt(); age >= time.Hour {
		t.Fatalf("a failed load should stamp lastAttemptAt, age = %v", age)
	}

	// The next call inside the TTL must not retry the load: a table that
	// now exists is not read until the TTL passes.
	_, err := db.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT NOT NULL, keywords TEXT);
		INSERT INTO memories (id, content, keywords) VALUES ('m1', 'note', '["kubernetes"]')`)
	if err != nil {
		t.Fatalf("recreate memories: %v", err)
	}
	if result := d.Detect(context.Background(), "kubernetes"); len(result) != 0 {
		t.Errorf("expected no reload inside the TTL, got %v", result)
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
		{"cafe\u0301", "cafe"},
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

// holdFirstLoad stands in for a first load that is still running and returns
// the func that ends it; calling it again is a no-op.
func holdFirstLoad(d *EntityDetector) func() {
	d.loadSem <- struct{}{}
	var once sync.Once
	return func() { once.Do(func() { <-d.loadSem }) }
}
