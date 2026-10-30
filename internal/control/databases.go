package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ibrahimraimi/litebase/internal/engine"
)

type DatabaseRecord struct {
	ID         string                `json:"id"`
	ProjectID  string                `json:"project_id"`
	Name       string                `json:"name"`
	Path       string                `json:"-"`
	Durability engine.DurabilityMode `json:"durability"`
	SizeBytes  int64                 `json:"size_bytes"`
	CreatedAt  time.Time             `json:"created_at"`
	DeletedAt  *time.Time            `json:"deleted_at,omitempty"`
}

type CreateDatabaseParams struct {
	ProjectID  string
	Name       string
	Path       string
	Durability engine.DurabilityMode
}

func (s *Store) CreateDatabase(ctx context.Context, params CreateDatabaseParams) (DatabaseRecord, error) {
	if err := engine.ValidateName(params.Name); err != nil {
		return DatabaseRecord{}, err
	}
	projectID := params.ProjectID
	if projectID == "" {
		projectID = DefaultProjectID
	}
	durability := params.Durability
	if durability == "" {
		durability = engine.DurabilityNormal
	}
	dbID, err := NewSortableID("db")
	if err != nil {
		return DatabaseRecord{}, err
	}
	createdAt := time.Now().UTC()
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO databases (id, project_id, name, path, durability, size_bytes, created_at)
		 VALUES (?, ?, ?, ?, ?, 0, ?)`,
		dbID,
		projectID,
		params.Name,
		params.Path,
		string(durability),
		createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return DatabaseRecord{}, fmt.Errorf("%w: database %q", ErrAlreadyExists, params.Name)
		}
		return DatabaseRecord{}, fmt.Errorf("insert database %q: %w", params.Name, err)
	}
	return DatabaseRecord{
		ID:         dbID,
		ProjectID:  projectID,
		Name:       params.Name,
		Path:       params.Path,
		Durability: durability,
		CreatedAt:  createdAt,
	}, nil
}

func (s *Store) GetDatabaseByName(ctx context.Context, projectID string, name string) (DatabaseRecord, error) {
	if projectID == "" {
		projectID = DefaultProjectID
	}
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, project_id, name, path, durability, size_bytes, created_at
		 FROM databases
		 WHERE project_id = ? AND name = ? AND deleted_at IS NULL`,
		projectID,
		name,
	)
	rec, err := scanDatabaseRow(row)
	if err == nil {
		return rec, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return DatabaseRecord{}, err
	}
	return s.findUniqueBranchDatabase(ctx, projectID, name)
}

func (s *Store) findUniqueBranchDatabase(ctx context.Context, projectID string, branchName string) (DatabaseRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT d.id, d.project_id, d.name, d.path, d.durability, d.size_bytes, d.created_at
		 FROM databases d
		 JOIN branches b ON b.database_id = d.id
		 WHERE d.project_id = ? AND b.name = ? AND d.deleted_at IS NULL`,
		projectID,
		branchName,
	)
	if err != nil {
		return DatabaseRecord{}, err
	}
	defer rows.Close()

	var matches []DatabaseRecord
	for rows.Next() {
		rec, err := scanDatabaseRow(rows)
		if err != nil {
			return DatabaseRecord{}, err
		}
		matches = append(matches, rec)
	}
	if err := rows.Err(); err != nil {
		return DatabaseRecord{}, err
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return DatabaseRecord{}, ErrNotFound
}

func (s *Store) GetDatabaseByID(ctx context.Context, databaseID string) (DatabaseRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, project_id, name, path, durability, size_bytes, created_at
		 FROM databases
		 WHERE id = ? AND deleted_at IS NULL`,
		databaseID,
	)
	return scanDatabaseRow(row)
}

func (s *Store) ListDatabases(ctx context.Context, projectID string) ([]DatabaseRecord, error) {
	if projectID == "" {
		projectID = DefaultProjectID
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, project_id, name, path, durability, size_bytes, created_at
		 FROM databases
		 WHERE project_id = ? AND deleted_at IS NULL
		 ORDER BY name ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()

	records := make([]DatabaseRecord, 0)
	for rows.Next() {
		rec, err := scanDatabaseRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) SoftDeleteDatabase(ctx context.Context, projectID string, name string) error {
	if projectID == "" {
		projectID = DefaultProjectID
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	outcome, err := s.db.ExecContext(
		ctx,
		`UPDATE databases SET deleted_at = ? WHERE project_id = ? AND name = ? AND deleted_at IS NULL`,
		nowStr,
		projectID,
		name,
	)
	if err != nil {
		return fmt.Errorf("soft delete database %q: %w", name, err)
	}
	affected, _ := outcome.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("%w: database %q", ErrNotFound, name)
	}
	return nil
}

func (s *Store) UpdateDatabaseSize(ctx context.Context, databaseID string, sizeBytes int64) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE databases SET size_bytes = ? WHERE id = ?`,
		sizeBytes,
		databaseID,
	)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanDatabaseRow(scanner rowScanner) (DatabaseRecord, error) {
	var rec DatabaseRecord
	var durabilityStr string
	var createdStr string
	err := scanner.Scan(
		&rec.ID,
		&rec.ProjectID,
		&rec.Name,
		&rec.Path,
		&durabilityStr,
		&rec.SizeBytes,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DatabaseRecord{}, ErrNotFound
		}
		return DatabaseRecord{}, err
	}
	rec.Durability = engine.DurabilityMode(durabilityStr)
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return rec, nil
}
