package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jholhewres/anchored/pkg/memory"

	_ "github.com/mattn/go-sqlite3"
)

type OpenCodeImporter struct {
	baseDir string
	log     func(string, ...any)
}

func NewOpenCodeImporter(baseDir string, log func(string, ...any)) *OpenCodeImporter {
	return &OpenCodeImporter{baseDir: baseDir, log: log}
}

func (i *OpenCodeImporter) Name() string { return "opencode" }
func (i *OpenCodeImporter) Path() string { return i.dbPath() }

func (i *OpenCodeImporter) dbPath() string {
	return filepath.Join(i.baseDir, ".local", "share", "opencode", "opencode.db")
}

func (i *OpenCodeImporter) Detect() bool {
	_, err := os.Stat(i.dbPath())
	if err != nil {
		return false
	}
	db, err := sql.Open("sqlite3", i.dbPath()+"?_mode=ro")
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('session','message','part')").Scan(&n)
	return err == nil && n == 3
}

func (i *OpenCodeImporter) Import(ctx context.Context, store ImportStore) ImportResult {
	result := ImportResult{Source: i.Name()}

	db, err := sql.Open("sqlite3", i.dbPath()+"?_mode=ro")
	if err != nil {
		result.Errors++
		return result
	}
	defer db.Close()

	projects, err := i.loadProjects(ctx, db)
	if err != nil {
		if i.log != nil {
			i.log("failed to load projects", "error", err)
		}
		result.Errors++
		return result
	}

	i.importProjects(ctx, store, projects, &result)
	i.importSessions(ctx, store, db, projects, &result)
	i.importTodos(ctx, store, db, projects, &result)

	return result
}

type opencodeProject struct {
	id       string
	worktree string
	name     string
}

func (i *OpenCodeImporter) loadProjects(ctx context.Context, db *sql.DB) (map[string]opencodeProject, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, worktree FROM project")
	if err != nil {
		return nil, fmt.Errorf("query projects: %w", err)
	}
	defer rows.Close()

	m := make(map[string]opencodeProject)
	for rows.Next() {
		var p opencodeProject
		if err := rows.Scan(&p.id, &p.name, &p.worktree); err != nil {
			continue
		}
		m[p.id] = p
	}
	return m, nil
}

func (i *OpenCodeImporter) importProjects(ctx context.Context, store ImportStore, projects map[string]opencodeProject, result *ImportResult) {
	for _, p := range projects {
		if strings.TrimSpace(p.name) == "" && strings.TrimSpace(p.worktree) == "" {
			continue
		}
		content := fmt.Sprintf("Project: %s (worktree: %s)", p.name, p.worktree)
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		result.Found++
		cwd := p.worktree
		if err := store.SaveRaw(ctx, content, "fact", i.Name(), cwd); err != nil {
			if i.log != nil {
				i.log("skip project", "id", p.id, "error", err)
			}
			result.Skipped++
			continue
		}
		result.Imported++
	}
}

type opencodeSession struct {
	id        string
	projectID string
	directory string
	title     string
}

func (i *OpenCodeImporter) importSessions(ctx context.Context, store ImportStore, db *sql.DB, projects map[string]opencodeProject, result *ImportResult) {
	rows, err := db.QueryContext(ctx, "SELECT id, project_id, directory, title FROM session")
	if err != nil {
		if i.log != nil {
			i.log("failed to query sessions", "error", err)
		}
		result.Errors++
		return
	}
	defer rows.Close()

	for rows.Next() {
		var s opencodeSession
		if err := rows.Scan(&s.id, &s.projectID, &s.directory, &s.title); err != nil {
			result.Errors++
			continue
		}

		cwd := s.directory
		if cwd == "" {
			if p, ok := projects[s.projectID]; ok {
				cwd = p.worktree
			}
		}
		if cwd == "" {
			continue
		}

		i.importSessionMessages(ctx, store, db, s, cwd, result)
	}
}

func (i *OpenCodeImporter) importSessionMessages(ctx context.Context, store ImportStore, db *sql.DB, s opencodeSession, cwd string, result *ImportResult) {
	msgRows, err := db.QueryContext(ctx, "SELECT id, data FROM message WHERE session_id = ?", s.id)
	if err != nil {
		if i.log != nil {
			i.log("failed to query messages", "session", s.id, "error", err)
		}
		return
	}
	defer msgRows.Close()

	for msgRows.Next() {
		var msgID, dataStr string
		if err := msgRows.Scan(&msgID, &dataStr); err != nil {
			continue
		}

		var msgData struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal([]byte(dataStr), &msgData); err != nil {
			msgData.Role = strings.TrimSpace(dataStr)
		}

		content := i.extractMessageText(ctx, db, msgID)
		if strings.TrimSpace(content) == "" {
			continue
		}

		result.Found++
		category := memory.Categorize(content)
		if err := store.SaveRaw(ctx, content, category, i.Name(), cwd); err != nil {
			if i.log != nil {
				i.log("skip message", "id", msgID, "error", err)
			}
			result.Skipped++
			continue
		}
		result.Imported++
	}
}

func (i *OpenCodeImporter) extractMessageText(ctx context.Context, db *sql.DB, messageID string) string {
	partRows, err := db.QueryContext(ctx, "SELECT data FROM part WHERE message_id = ?", messageID)
	if err != nil {
		return ""
	}
	defer partRows.Close()

	var texts []string
	for partRows.Next() {
		var dataStr string
		if err := partRows.Scan(&dataStr); err != nil {
			continue
		}

		var partData struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(dataStr), &partData); err != nil {
			continue
		}
		if partData.Type == "text" && strings.TrimSpace(partData.Text) != "" {
			texts = append(texts, strings.TrimSpace(partData.Text))
		}
	}

	return strings.Join(texts, "\n")
}

func (i *OpenCodeImporter) importTodos(ctx context.Context, store ImportStore, db *sql.DB, projects map[string]opencodeProject, result *ImportResult) {
	sessionCWDs := i.buildSessionCWDMap(ctx, db, projects)

	rows, err := db.QueryContext(ctx, "SELECT session_id, content, status FROM todo")
	if err != nil {
		if i.log != nil {
			i.log("failed to query todos", "error", err)
		}
		return
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID, content, status string
		if err := rows.Scan(&sessionID, &content, &status); err != nil {
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}

		result.Found++
		cwd := sessionCWDs[sessionID]
		if err := store.SaveRaw(ctx, content, "plan", i.Name(), cwd); err != nil {
			if i.log != nil {
				i.log("skip todo", "session", sessionID, "error", err)
			}
			result.Skipped++
			continue
		}
		result.Imported++
	}
}

func (i *OpenCodeImporter) buildSessionCWDMap(ctx context.Context, db *sql.DB, projects map[string]opencodeProject) map[string]string {
	rows, err := db.QueryContext(ctx, "SELECT id, project_id, directory FROM session")
	if err != nil {
		return nil
	}
	defer rows.Close()

	m := make(map[string]string)
	for rows.Next() {
		var id, projectID, directory string
		if err := rows.Scan(&id, &projectID, &directory); err != nil {
			continue
		}
		cwd := directory
		if cwd == "" {
			if p, ok := projects[projectID]; ok {
				cwd = p.worktree
			}
		}
		if cwd != "" {
			m[id] = cwd
		}
	}
	return m
}
