package kg

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type KG struct {
	db     *sql.DB
	logger *slog.Logger
}

func New(db *sql.DB, logger *slog.Logger) *KG {
	if logger == nil {
		logger = slog.Default()
	}
	return &KG{db: db, logger: logger}
}

type Triple struct {
	ID         string     `json:"id"`
	Subject    string     `json:"subject"`
	Predicate  string     `json:"predicate"`
	Object     string     `json:"object"`
	Confidence float64    `json:"confidence"`
	ProjectID  *string    `json:"project_id,omitempty"`
	ValidFrom  time.Time  `json:"valid_from"`
	ValidTo    *time.Time `json:"valid_to,omitempty"`
}

func (k *KG) AddTriple(ctx context.Context, subject, predicate, object string, projectID *string) (*Triple, error) {
	tx, err := k.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	subjectID, err := k.ensureEntity(ctx, tx, subject, projectID)
	if err != nil {
		return nil, fmt.Errorf("ensure subject: %w", err)
	}

	predicateID, err := k.ensurePredicate(ctx, tx, predicate)
	if err != nil {
		return nil, fmt.Errorf("ensure predicate: %w", err)
	}

	objectID, err := k.ensureEntity(ctx, tx, object, projectID)
	if err != nil {
		return nil, fmt.Errorf("ensure object: %w", err)
	}

	var isFunctional bool
	err = tx.QueryRowContext(ctx,
		"SELECT is_functional FROM kg_predicates WHERE id = ?",
		predicateID,
	).Scan(&isFunctional)
	if err != nil {
		return nil, fmt.Errorf("check functional: %w", err)
	}

	if isFunctional {
		tx.ExecContext(ctx,
			"UPDATE kg_triples SET valid_to = CURRENT_TIMESTAMP WHERE subject_id = ? AND predicate_id = ? AND valid_to IS NULL",
			subjectID, predicateID,
		)
	}

	id := newID()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO kg_triples (id, subject_id, predicate_id, object_id, confidence, project_id)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, subjectID, predicateID, objectID, 1.0, projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("insert triple: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &Triple{
		ID:        id,
		Subject:   subject,
		Predicate: predicate,
		Object:    object,
		Confidence: 1.0,
		ValidFrom: time.Now().UTC(),
	}, nil
}

func (k *KG) Query(ctx context.Context, entityName string, projectID *string) ([]Triple, error) {
	var rows *sql.Rows
	var err error

	if projectID != nil {
		rows, err = k.db.QueryContext(ctx, `
			SELECT t.id, s.name, p.name, o.name, t.confidence, t.project_id, t.valid_from, t.valid_to
			FROM kg_triples t
			JOIN kg_entities s ON t.subject_id = s.id
			JOIN kg_predicates p ON t.predicate_id = p.id
			JOIN kg_entities o ON t.object_id = o.id
			WHERE t.valid_to IS NULL AND t.project_id = ?
			AND (s.name = ? OR o.name = ? OR EXISTS (
				SELECT 1 FROM kg_entity_aliases a WHERE a.entity_id = s.id AND a.alias = ?
			) OR EXISTS (
				SELECT 1 FROM kg_entity_aliases a WHERE a.entity_id = o.id AND a.alias = ?
			))
			ORDER BY t.created_at DESC
		`, projectID, entityName, entityName, strings.ToLower(entityName), strings.ToLower(entityName))
	} else {
		rows, err = k.db.QueryContext(ctx, `
			SELECT t.id, s.name, p.name, o.name, t.confidence, t.project_id, t.valid_from, t.valid_to
			FROM kg_triples t
			JOIN kg_entities s ON t.subject_id = s.id
			JOIN kg_predicates p ON t.predicate_id = p.id
			JOIN kg_entities o ON t.object_id = o.id
			WHERE t.valid_to IS NULL
			AND (s.name = ? OR o.name = ? OR EXISTS (
				SELECT 1 FROM kg_entity_aliases a WHERE a.entity_id = s.id AND a.alias = ?
			) OR EXISTS (
				SELECT 1 FROM kg_entity_aliases a WHERE a.entity_id = o.id AND a.alias = ?
			))
			ORDER BY t.created_at DESC
		`, entityName, entityName, strings.ToLower(entityName), strings.ToLower(entityName))
	}
	if err != nil {
		return nil, fmt.Errorf("query triples: %w", err)
	}
	defer rows.Close()

	var triples []Triple
	for rows.Next() {
		var t Triple
		var projID sql.NullString
		var validTo sql.NullTime
		err := rows.Scan(&t.ID, &t.Subject, &t.Predicate, &t.Object, &t.Confidence, &projID, &t.ValidFrom, &validTo)
		if err != nil {
			continue
		}
		if projID.Valid {
			t.ProjectID = &projID.String
		}
		if validTo.Valid {
			t.ValidTo = &validTo.Time
		}
		triples = append(triples, t)
	}

	return triples, nil
}

func (k *KG) ensureEntity(ctx context.Context, tx *sql.Tx, name string, projectID *string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM kg_entities WHERE name = ?", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	err = tx.QueryRowContext(ctx,
		"SELECT e.id FROM kg_entity_aliases a JOIN kg_entities e ON a.entity_id = e.id WHERE a.alias = ?",
		strings.ToLower(name),
	).Scan(&id)
	if err == nil {
		return id, nil
	}

	id = newID()
	_, err = tx.ExecContext(ctx,
		"INSERT INTO kg_entities (id, name, project_id) VALUES (?, ?, ?)",
		id, name, projectID,
	)
	if err != nil {
		return "", err
	}

	tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO kg_entity_aliases (entity_id, alias) VALUES (?, ?)",
		id, strings.ToLower(name),
	)

	return id, nil
}

func (k *KG) ensurePredicate(ctx context.Context, tx *sql.Tx, name string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM kg_predicates WHERE name = ?", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	id = newID()
	_, err = tx.ExecContext(ctx,
		"INSERT INTO kg_predicates (id, name) VALUES (?, ?)",
		id, name,
	)
	return id, err
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
