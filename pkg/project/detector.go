package project

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
)

type Project struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	SourceTool string `json:"source_tool,omitempty"`
}

type Detector struct {
	db *sql.DB
}

func NewDetector(db *sql.DB) *Detector {
	return &Detector{db: db}
}

func (d *Detector) Detect(cwd string) (*Project, error) {
	gitRoot, err := gitRoot(cwd)
	if err != nil || gitRoot == "" {
		return nil, nil
	}

	gitRoot, err = filepath.Abs(gitRoot)
	if err != nil {
		return nil, err
	}

	var existing Project
	err = d.db.QueryRow(
		"SELECT id, name, path, source_tool FROM projects WHERE path = ?",
		gitRoot,
	).Scan(&existing.ID, &existing.Name, &existing.Path, &existing.SourceTool)

	if err == nil {
		return &existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	name := filepath.Base(gitRoot)
	id := newID()

	_, err = d.db.Exec(
		"INSERT INTO projects (id, name, path) VALUES (?, ?, ?)",
		id, name, gitRoot,
	)
	if err != nil {
		return nil, err
	}

	return &Project{ID: id, Name: name, Path: gitRoot}, nil
}

func (d *Detector) Resolve(id string) (*Project, error) {
	var p Project
	err := d.db.QueryRow(
		"SELECT id, name, path, source_tool FROM projects WHERE id = ?",
		id,
	).Scan(&p.ID, &p.Name, &p.Path, &p.SourceTool)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func gitRoot(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
