package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type AppliedMigrationRecord struct {
	DatabaseID string    `json:"database_id"`
	Version    string    `json:"version"`
	Name       string    `json:"name"`
	Checksum   string    `json:"checksum"`
	AppliedAt  time.Time `json:"applied_at"`
	AppliedBy  string    `json:"applied_by"`
}

type SnapshotRecord struct {
	ID         string    `json:"id"`
	DatabaseID string    `json:"database_id"`
	Path       string    `json:"-"`
	SizeBytes  int64     `json:"size_bytes"`
	CreatedAt  time.Time `json:"created_at"`
	Label      string    `json:"label"`
}

type CreateSnapshotParams struct {
	DatabaseID   string
	SnapshotPath string
	SizeBytes    int64
	Label        string
}

type BranchRecord struct {
	ID               string    `json:"id"`
	DatabaseID       string    `json:"database_id"`
	ParentID         string    `json:"parent_id"`
	ParentSnapshotID string    `json:"parent_snapshot_id,omitempty"`
	Name             string    `json:"name"`
	CreatedAt        time.Time `json:"created_at"`
}

type CreateBranchParams struct {
	BranchDatabaseID string
	ParentDatabaseID string
	ParentSnapshotID string
	Name             string
}

type AuditParams struct {
	Actor   string
	Action  string
	Target  string
	Details any
}

func (s *Store) RecordAppliedMigration(ctx context.Context, rec AppliedMigrationRecord) error {
	appliedAt := rec.AppliedAt
	if appliedAt.IsZero() {
		appliedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO migrations_applied (database_id, version, name, checksum, applied_at, applied_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rec.DatabaseID,
		rec.Version,
		rec.Name,
		rec.Checksum,
		appliedAt.Format(time.RFC3339Nano),
		rec.AppliedBy,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return fmt.Errorf("%w: migration version %q", ErrAlreadyExists, rec.Version)
		}
		return fmt.Errorf("record migration %q: %w", rec.Version, err)
	}
	return nil
}

func (s *Store) ListAppliedMigrations(ctx context.Context, databaseID string) ([]AppliedMigrationRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT database_id, version, name, checksum, applied_at, applied_by
		 FROM migrations_applied
		 WHERE database_id = ?
		 ORDER BY version ASC`,
		databaseID,
	)
	if err != nil {
		return nil, fmt.Errorf("list applied migrations: %w", err)
	}
	defer rows.Close()

	records := make([]AppliedMigrationRecord, 0)
	for rows.Next() {
		var rec AppliedMigrationRecord
		var appliedStr string
		if err := rows.Scan(&rec.DatabaseID, &rec.Version, &rec.Name, &rec.Checksum, &appliedStr, &rec.AppliedBy); err != nil {
			return nil, err
		}
		rec.AppliedAt, _ = time.Parse(time.RFC3339Nano, appliedStr)
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) CreateSnapshotRecord(ctx context.Context, params CreateSnapshotParams) (SnapshotRecord, error) {
	snapID, err := NewSortableID("snap")
	if err != nil {
		return SnapshotRecord{}, err
	}
	createdAt := time.Now().UTC()
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO snapshots (id, database_id, path, size_bytes, created_at, label)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		snapID,
		params.DatabaseID,
		params.SnapshotPath,
		params.SizeBytes,
		createdAt.Format(time.RFC3339Nano),
		params.Label,
	)
	if err != nil {
		return SnapshotRecord{}, fmt.Errorf("insert snapshot record: %w", err)
	}
	return SnapshotRecord{
		ID:         snapID,
		DatabaseID: params.DatabaseID,
		Path:       params.SnapshotPath,
		SizeBytes:  params.SizeBytes,
		CreatedAt:  createdAt,
		Label:      params.Label,
	}, nil
}

func (s *Store) GetSnapshotRecord(ctx context.Context, databaseID string, snapshotID string) (SnapshotRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, database_id, path, size_bytes, created_at, label
		 FROM snapshots
		 WHERE database_id = ? AND id = ?`,
		databaseID,
		snapshotID,
	)
	return scanSnapshotRow(row)
}

func (s *Store) ListSnapshotRecords(ctx context.Context, databaseID string) ([]SnapshotRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, database_id, path, size_bytes, created_at, label
		 FROM snapshots
		 WHERE database_id = ?
		 ORDER BY created_at DESC, id DESC`,
		databaseID,
	)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()

	records := make([]SnapshotRecord, 0)
	for rows.Next() {
		rec, err := scanSnapshotRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func scanSnapshotRow(scanner rowScanner) (SnapshotRecord, error) {
	var rec SnapshotRecord
	var createdStr string
	err := scanner.Scan(&rec.ID, &rec.DatabaseID, &rec.Path, &rec.SizeBytes, &createdStr, &rec.Label)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SnapshotRecord{}, ErrNotFound
		}
		return SnapshotRecord{}, err
	}
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return rec, nil
}

func (s *Store) CreateBranchRecord(ctx context.Context, params CreateBranchParams) (BranchRecord, error) {
	branchID, err := NewSortableID("br")
	if err != nil {
		return BranchRecord{}, err
	}
	createdAt := time.Now().UTC()
	var nullableSnap any
	if params.ParentSnapshotID != "" {
		nullableSnap = params.ParentSnapshotID
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO branches (id, database_id, parent_id, parent_snapshot_id, name, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		branchID,
		params.BranchDatabaseID,
		params.ParentDatabaseID,
		nullableSnap,
		params.Name,
		createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return BranchRecord{}, fmt.Errorf("%w: branch %q", ErrAlreadyExists, params.Name)
		}
		return BranchRecord{}, fmt.Errorf("insert branch record %q: %w", params.Name, err)
	}
	return BranchRecord{
		ID:               branchID,
		DatabaseID:       params.BranchDatabaseID,
		ParentID:         params.ParentDatabaseID,
		ParentSnapshotID: params.ParentSnapshotID,
		Name:             params.Name,
		CreatedAt:        createdAt,
	}, nil
}

func (s *Store) ListBranchRecords(ctx context.Context, parentDatabaseID string) ([]BranchRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, database_id, parent_id, COALESCE(parent_snapshot_id, ''), name, created_at
		 FROM branches
		 WHERE parent_id = ?
		 ORDER BY name ASC`,
		parentDatabaseID,
	)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	records := make([]BranchRecord, 0)
	for rows.Next() {
		var rec BranchRecord
		var createdStr string
		if err := rows.Scan(&rec.ID, &rec.DatabaseID, &rec.ParentID, &rec.ParentSnapshotID, &rec.Name, &createdStr); err != nil {
			return nil, err
		}
		rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) DeleteBranchRecord(ctx context.Context, parentDatabaseID string, branchName string) error {
	outcome, err := s.db.ExecContext(
		ctx,
		`DELETE FROM branches WHERE parent_id = ? AND name = ?`,
		parentDatabaseID,
		branchName,
	)
	if err != nil {
		return fmt.Errorf("delete branch %q: %w", branchName, err)
	}
	affected, _ := outcome.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("%w: branch %q", ErrNotFound, branchName)
	}
	return nil
}

func (s *Store) RecordAudit(ctx context.Context, params AuditParams) error {
	auditID, err := NewSortableID("aud")
	if err != nil {
		return err
	}
	detailBytes := []byte("{}")
	if params.Details != nil {
		if marshaled, mErr := json.Marshal(params.Details); mErr == nil {
			detailBytes = marshaled
		}
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO audit_log (id, ts, actor, action, target, detail_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		auditID,
		time.Now().UTC().Format(time.RFC3339Nano),
		params.Actor,
		params.Action,
		params.Target,
		string(detailBytes),
	)
	return err
}
