package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ibrahimraimi/litebase/internal/engine"
)

type AppliedMigrationRecord struct {
	DatabaseID string    `json:"database_id"`
	Version    string    `json:"version"`
	Name       string    `json:"name"`
	Checksum   string    `json:"checksum"`
	AppliedAt  time.Time `json:"applied_at"`
	AppliedBy  string    `json:"applied_by"`
}

const (
	SnapshotKindManual     = "manual"
	SnapshotKindScheduled  = "scheduled"
	SnapshotKindPreRestore = "pre-restore"
	SnapshotKindPrePush    = "pre-push"
	SnapshotKindBranch     = "branch"

	RemoteStatusNone     = "none"
	RemoteStatusPending  = "pending"
	RemoteStatusUploaded = "uploaded"
	RemoteStatusFailed   = "failed"
)

type SnapshotRecord struct {
	ID               string     `json:"id"`
	DatabaseID       string     `json:"database_id"`
	Path             string     `json:"-"`
	RelPath          string     `json:"rel_path,omitempty"`
	SHA256           string     `json:"sha256,omitempty"`
	Kind             string     `json:"kind"`
	Pinned           bool       `json:"pinned"`
	RemoteKey        string     `json:"remote_key,omitempty"`
	RemoteStatus     string     `json:"remote_status"`
	RemoteUploadedAt *time.Time `json:"remote_uploaded_at,omitempty"`
	SizeBytes        int64      `json:"size_bytes"`
	CreatedAt        time.Time  `json:"created_at"`
	Label            string     `json:"label"`
}

type CreateSnapshotParams struct {
	DatabaseID   string
	SnapshotPath string
	RelPath      string
	SHA256       string
	Kind         string
	Pinned       bool
	RemoteStatus string
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
	relPath := params.RelPath
	if relPath == "" {
		relPath = extractRelativePathInDataDir(s.dataDir, params.SnapshotPath, "snapshots")
	}
	kind := params.Kind
	if kind == "" {
		kind = InferSnapshotKind(params.Label)
	}
	remoteStatus := params.RemoteStatus
	if remoteStatus == "" {
		remoteStatus = RemoteStatusNone
	}
	pinnedInt := 0
	if params.Pinned {
		pinnedInt = 1
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO snapshots (
			id, database_id, path, rel_path, sha256, kind, pinned, remote_key, remote_status, remote_uploaded_at, size_bytes, created_at, label
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?, NULL, ?, ?, ?)`,
		snapID,
		params.DatabaseID,
		params.SnapshotPath,
		relPath,
		params.SHA256,
		kind,
		pinnedInt,
		remoteStatus,
		params.SizeBytes,
		createdAt.Format(time.RFC3339Nano),
		params.Label,
	)
	if err != nil {
		return SnapshotRecord{}, fmt.Errorf("insert snapshot record: %w", err)
	}
	resolvedPath := s.resolveSnapshotPath(params.SnapshotPath, relPath)
	return SnapshotRecord{
		ID:           snapID,
		DatabaseID:   params.DatabaseID,
		Path:         resolvedPath,
		RelPath:      relPath,
		SHA256:       params.SHA256,
		Kind:         kind,
		Pinned:       params.Pinned,
		RemoteStatus: remoteStatus,
		SizeBytes:    params.SizeBytes,
		CreatedAt:    createdAt,
		Label:        params.Label,
	}, nil
}

const snapshotSelectColumns = `id, database_id, path, COALESCE(rel_path, ''), COALESCE(sha256, ''), COALESCE(kind, 'manual'), COALESCE(pinned, 0), COALESCE(remote_key, ''), COALESCE(remote_status, 'none'), COALESCE(remote_uploaded_at, ''), size_bytes, created_at, label`

func (s *Store) GetSnapshotRecord(ctx context.Context, databaseID string, snapshotID string) (SnapshotRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+snapshotSelectColumns+`
		 FROM snapshots
		 WHERE database_id = ? AND id = ?`,
		databaseID,
		snapshotID,
	)
	return s.scanSnapshotRow(row)
}

func (s *Store) ListSnapshotRecords(ctx context.Context, databaseID string) ([]SnapshotRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+snapshotSelectColumns+`
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
		rec, err := s.scanSnapshotRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

func (s *Store) scanSnapshotRow(scanner rowScanner) (SnapshotRecord, error) {
	var rec SnapshotRecord
	var pinnedInt int
	var uploadedStr string
	var createdStr string
	err := scanner.Scan(
		&rec.ID,
		&rec.DatabaseID,
		&rec.Path,
		&rec.RelPath,
		&rec.SHA256,
		&rec.Kind,
		&pinnedInt,
		&rec.RemoteKey,
		&rec.RemoteStatus,
		&uploadedStr,
		&rec.SizeBytes,
		&createdStr,
		&rec.Label,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SnapshotRecord{}, ErrNotFound
		}
		return SnapshotRecord{}, err
	}
	rec.Pinned = pinnedInt != 0
	if uploadedStr != "" {
		if parsed, pErr := time.Parse(time.RFC3339Nano, uploadedStr); pErr == nil {
			rec.RemoteUploadedAt = &parsed
		}
	}
	rec.Path = s.resolveSnapshotPath(rec.Path, rec.RelPath)
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return rec, nil
}

func (s *Store) resolveSnapshotPath(storedPath string, relPath string) string {
	if s.dataDir != "" && relPath != "" {
		segments := strings.Split(filepath.ToSlash(relPath), "/")
		if resolved, err := engine.SafeJoinPath(s.dataDir, segments...); err == nil {
			return resolved
		}
	}
	return storedPath
}

func (s *Store) UpdateSnapshotChecksum(ctx context.Context, snapshotID string, sha256Hex string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE snapshots SET sha256 = ? WHERE id = ?`,
		sha256Hex,
		snapshotID,
	)
	return err
}

func (s *Store) UpdateSnapshotRemoteState(
	ctx context.Context,
	snapshotID string,
	remoteKey string,
	remoteStatus string,
	uploadedAt *time.Time,
) error {
	var uploadedVal any
	if uploadedAt != nil {
		uploadedVal = uploadedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE snapshots
		 SET remote_key = ?, remote_status = ?, remote_uploaded_at = ?
		 WHERE id = ?`,
		remoteKey,
		remoteStatus,
		uploadedVal,
		snapshotID,
	)
	return err
}

func (s *Store) DeleteSnapshotRecord(ctx context.Context, databaseID string, snapshotID string) error {
	outcome, err := s.db.ExecContext(
		ctx,
		`DELETE FROM snapshots WHERE database_id = ? AND id = ?`,
		databaseID,
		snapshotID,
	)
	if err != nil {
		return fmt.Errorf("delete snapshot record %q: %w", snapshotID, err)
	}
	affected, _ := outcome.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("%w: snapshot %q", ErrNotFound, snapshotID)
	}
	return nil
}

func (s *Store) IsSnapshotReferencedByBranch(ctx context.Context, snapshotID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM branches WHERE parent_snapshot_id = ?`,
		snapshotID,
	).Scan(&count)
	return count > 0, err
}

func (s *Store) ListBranchReferencedSnapshotIDs(ctx context.Context, parentDatabaseID string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT COALESCE(parent_snapshot_id, '') FROM branches WHERE parent_id = ?`,
		parentDatabaseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	protected := make(map[string]struct{})
	for rows.Next() {
		var snapID string
		if err := rows.Scan(&snapID); err != nil {
			return nil, err
		}
		if snapID != "" {
			protected[snapID] = struct{}{}
		}
	}
	return protected, rows.Err()
}

func (s *Store) ListPendingRemoteSnapshots(ctx context.Context, databaseID string) ([]SnapshotRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+snapshotSelectColumns+`
		 FROM snapshots
		 WHERE database_id = ? AND remote_status IN ('pending', 'failed')
		 ORDER BY created_at ASC, id ASC`,
		databaseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pending []SnapshotRecord
	for rows.Next() {
		rec, err := s.scanSnapshotRow(rows)
		if err != nil {
			return nil, err
		}
		pending = append(pending, rec)
	}
	return pending, rows.Err()
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

func (s *Store) GetBranchRecord(ctx context.Context, parentDatabaseID string, branchName string) (BranchRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, database_id, parent_id, COALESCE(parent_snapshot_id, ''), name, created_at
		 FROM branches
		 WHERE parent_id = ? AND name = ?`,
		parentDatabaseID,
		branchName,
	)
	var rec BranchRecord
	var createdStr string
	if err := row.Scan(&rec.ID, &rec.DatabaseID, &rec.ParentID, &rec.ParentSnapshotID, &rec.Name, &createdStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BranchRecord{}, ErrNotFound
		}
		return BranchRecord{}, err
	}
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return rec, nil
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

func (s *Store) CountAuditEvents(ctx context.Context, action string, target string) (int, error) {
	var count int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action = ? AND target = ?`,
		action,
		target,
	).Scan(&count)
	return count, err
}
