package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	DefaultKeepLastSnapshots = 24
	DefaultKeepDaysSnapshots = 14
)

type BackupPolicyRecord struct {
	DatabaseID      string     `json:"database_id"`
	IntervalSeconds int64      `json:"interval_seconds"`
	KeepLast        int        `json:"keep_last"`
	KeepDays        int        `json:"keep_days"`
	Remote          bool       `json:"remote"`
	Enabled         bool       `json:"enabled"`
	LastRunAt       *time.Time `json:"last_run_at,omitempty"`
	LastSuccessAt   *time.Time `json:"last_success_at,omitempty"`
	LastError       string     `json:"last_error,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

type UpsertBackupPolicyParams struct {
	DatabaseID      string
	IntervalSeconds int64
	KeepLast        int
	KeepDays        int
	Remote          bool
	Enabled         bool
}

func (s *Store) UpsertBackupPolicy(ctx context.Context, params UpsertBackupPolicyParams) (BackupPolicyRecord, error) {
	if params.IntervalSeconds <= 0 {
		return BackupPolicyRecord{}, errors.New("backup policy interval_seconds must be positive")
	}
	keepLast := params.KeepLast
	if keepLast <= 0 {
		keepLast = DefaultKeepLastSnapshots
	}
	keepDays := params.KeepDays
	if keepDays <= 0 {
		keepDays = DefaultKeepDaysSnapshots
	}
	remoteInt := 0
	if params.Remote {
		remoteInt = 1
	}
	enabledInt := 0
	if params.Enabled {
		enabledInt = 1
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO backup_policies (
			database_id, interval_seconds, keep_last, keep_days, remote, enabled, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(database_id) DO UPDATE SET
			interval_seconds = excluded.interval_seconds,
			keep_last = excluded.keep_last,
			keep_days = excluded.keep_days,
			remote = excluded.remote,
			enabled = excluded.enabled`,
		params.DatabaseID,
		params.IntervalSeconds,
		keepLast,
		keepDays,
		remoteInt,
		enabledInt,
		nowStr,
	)
	if err != nil {
		return BackupPolicyRecord{}, fmt.Errorf("upsert backup policy: %w", err)
	}
	return s.GetBackupPolicy(ctx, params.DatabaseID)
}

const backupPolicySelectCols = `database_id, interval_seconds, keep_last, keep_days, remote, enabled, COALESCE(last_run_at, ''), COALESCE(last_success_at, ''), COALESCE(last_error, ''), created_at`

func (s *Store) GetBackupPolicy(ctx context.Context, databaseID string) (BackupPolicyRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+backupPolicySelectCols+`
		 FROM backup_policies
		 WHERE database_id = ?`,
		databaseID,
	)
	return scanBackupPolicyRow(row)
}

func (s *Store) ListBackupPolicies(ctx context.Context) ([]BackupPolicyRecord, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT `+backupPolicySelectCols+`
		 FROM backup_policies
		 ORDER BY database_id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list backup policies: %w", err)
	}
	defer rows.Close()

	var policies []BackupPolicyRecord
	for rows.Next() {
		rec, err := scanBackupPolicyRow(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, rec)
	}
	return policies, rows.Err()
}

func (s *Store) DisableBackupPolicy(ctx context.Context, databaseID string) error {
	outcome, err := s.db.ExecContext(
		ctx,
		`UPDATE backup_policies SET enabled = 0 WHERE database_id = ?`,
		databaseID,
	)
	if err != nil {
		return fmt.Errorf("disable backup policy: %w", err)
	}
	affected, _ := outcome.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("%w: backup policy for database %q", ErrNotFound, databaseID)
	}
	return nil
}

func (s *Store) DeleteBackupPolicy(ctx context.Context, databaseID string) error {
	outcome, err := s.db.ExecContext(
		ctx,
		`DELETE FROM backup_policies WHERE database_id = ?`,
		databaseID,
	)
	if err != nil {
		return fmt.Errorf("delete backup policy: %w", err)
	}
	affected, _ := outcome.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("%w: backup policy for database %q", ErrNotFound, databaseID)
	}
	return nil
}

func (s *Store) UpdateBackupPolicyRunState(
	ctx context.Context,
	databaseID string,
	lastRunAt time.Time,
	lastSuccessAt *time.Time,
	lastError string,
) error {
	var successStr any
	if lastSuccessAt != nil {
		successStr = lastSuccessAt.UTC().Format(time.RFC3339Nano)
	} else {
		existing, err := s.GetBackupPolicy(ctx, databaseID)
		if err == nil && existing.LastSuccessAt != nil {
			successStr = existing.LastSuccessAt.UTC().Format(time.RFC3339Nano)
		}
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE backup_policies
		 SET last_run_at = ?, last_success_at = ?, last_error = ?
		 WHERE database_id = ?`,
		lastRunAt.UTC().Format(time.RFC3339Nano),
		successStr,
		lastError,
		databaseID,
	)
	return err
}

func scanBackupPolicyRow(scanner rowScanner) (BackupPolicyRecord, error) {
	var rec BackupPolicyRecord
	var remoteInt, enabledInt int
	var lastRunStr, lastSuccessStr, createdStr string
	err := scanner.Scan(
		&rec.DatabaseID,
		&rec.IntervalSeconds,
		&rec.KeepLast,
		&rec.KeepDays,
		&remoteInt,
		&enabledInt,
		&lastRunStr,
		&lastSuccessStr,
		&rec.LastError,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BackupPolicyRecord{}, ErrNotFound
		}
		return BackupPolicyRecord{}, err
	}
	rec.Remote = remoteInt != 0
	rec.Enabled = enabledInt != 0
	if lastRunStr != "" {
		if parsed, pErr := time.Parse(time.RFC3339Nano, lastRunStr); pErr == nil {
			rec.LastRunAt = &parsed
		}
	}
	if lastSuccessStr != "" {
		if parsed, pErr := time.Parse(time.RFC3339Nano, lastSuccessStr); pErr == nil {
			rec.LastSuccessAt = &parsed
		}
	}
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	return rec, nil
}
