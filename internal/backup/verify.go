package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
)

type VerifyOptions struct {
	Database string   `json:"database"`
	Remote   bool     `json:"remote"`
	Samples  []string `json:"samples,omitempty"`
}

type VerifyResult struct {
	Database          string           `json:"database"`
	Source            string           `json:"source"`
	SnapshotID        string           `json:"snapshot_id,omitempty"`
	RemoteKey         string           `json:"remote_key,omitempty"`
	SHA256            string           `json:"sha256"`
	IntegrityOK       bool             `json:"integrity_ok"`
	ForeignKeysOK     bool             `json:"foreign_keys_ok"`
	MigrationVersions []string         `json:"migration_versions"`
	TableRowCounts    map[string]int64 `json:"table_row_counts"`
	VerifiedAt        time.Time        `json:"verified_at"`
}

func (bm *Manager) Verify(ctx context.Context, opts VerifyOptions) (VerifyResult, error) {
	if err := engine.ValidateName(opts.Database); err != nil {
		return VerifyResult{}, err
	}
	tempDir, err := os.MkdirTemp("", "litebase-verify-*")
	if err != nil {
		return VerifyResult{}, err
	}
	defer os.RemoveAll(tempDir)

	drilledFile := filepath.Join(tempDir, "drill.sqlite")
	result := VerifyResult{
		Database:       opts.Database,
		TableRowCounts: make(map[string]int64),
		VerifiedAt:     time.Now().UTC(),
	}

	if opts.Remote {
		result.Source = "remote"
		meta, err := bm.resolveRemoteSnapshotMeta(ctx, opts.Database, "latest")
		if err != nil {
			return VerifyResult{}, err
		}
		if err := downloadAndVerifyObject(ctx, bm.blobStore, meta, drilledFile, bm.maxDownloadBytes); err != nil {
			return VerifyResult{}, err
		}
		result.RemoteKey = meta.DataKey
		result.SnapshotID = meta.SnapshotID
		result.SHA256 = meta.SHA256
	} else {
		result.Source = "local"
		snaps, err := bm.snapshots.List(ctx, opts.Database)
		if err != nil {
			return VerifyResult{}, err
		}
		if len(snaps) == 0 {
			return VerifyResult{}, fmt.Errorf("%w: no local snapshots for %q", control.ErrNotFound, opts.Database)
		}
		latest := snaps[0]
		sha256Hex, err := bm.snapshots.EnsureChecksum(ctx, latest)
		if err != nil {
			return VerifyResult{}, err
		}
		if err := copyVerifiedFileAtomically(latest.Path, drilledFile); err != nil {
			return VerifyResult{}, err
		}
		result.SnapshotID = latest.ID
		result.SHA256 = sha256Hex
	}

	if err := inspectDrilledDatabase(ctx, drilledFile, opts.Samples, &result); err != nil {
		return VerifyResult{}, err
	}
	return result, nil
}

func inspectDrilledDatabase(
	ctx context.Context,
	drilledFile string,
	requestedSamples []string,
	result *VerifyResult,
) error {
	if err := engine.CheckFileIntegrity(ctx, drilledFile); err != nil {
		return err
	}
	result.IntegrityOK = true

	pool, err := engine.NewSQLiteEngine().OpenReaderPool(drilledFile, engine.Options{ReaderPoolMax: 1})
	if err != nil {
		return err
	}
	defer pool.Close()

	fkRows, err := pool.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("run foreign_key_check: %w", err)
	}
	hasFKViolation := fkRows.Next()
	fkIterErr := fkRows.Err()
	_ = fkRows.Close()
	if fkIterErr != nil {
		return fmt.Errorf("iterate foreign_key_check: %w", fkIterErr)
	}
	if hasFKViolation {
		return fmt.Errorf("foreign_key_check failed on drilled backup for %q", result.Database)
	}
	result.ForeignKeysOK = true

	migrations, err := queryDrilledMigrations(ctx, pool)
	if err != nil {
		return err
	}
	result.MigrationVersions = migrations
	existingTables, err := queryUserTables(ctx, pool)
	if err != nil {
		return err
	}
	sampleSet := selectSampleTables(existingTables, requestedSamples)
	for _, tbl := range sampleSet {
		quoted := `"` + strings.ReplaceAll(tbl, `"`, `""`) + `"`
		var count int64
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return fmt.Errorf("count rows in %q: %w", tbl, err)
		}
		result.TableRowCounts[tbl] = count
	}
	return nil
}

func queryDrilledMigrations(ctx context.Context, pool *sql.DB) ([]string, error) {
	rows, err := pool.QueryContext(ctx, "SELECT version FROM _litebase_migrations ORDER BY version ASC")
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return []string{}, nil
		}
		return nil, fmt.Errorf("query _litebase_migrations: %w", err)
	}
	defer rows.Close()

	versions := make([]string, 0)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan _litebase_migrations row: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate _litebase_migrations rows: %w", err)
	}
	return versions, nil
}

func queryUserTables(ctx context.Context, pool *sql.DB) ([]string, error) {
	rows, err := pool.QueryContext(
		ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '_litebase_%'
		 ORDER BY name ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func selectSampleTables(existingTables []string, requestedSamples []string) []string {
	allowed := make(map[string]struct{}, len(existingTables))
	for _, t := range existingTables {
		allowed[t] = struct{}{}
	}
	if len(requestedSamples) > 0 {
		selected := make([]string, 0, len(requestedSamples))
		for _, req := range requestedSamples {
			if _, ok := allowed[req]; ok {
				selected = append(selected, req)
			}
		}
		return selected
	}
	if len(existingTables) > 5 {
		return existingTables[:5]
	}
	return existingTables
}
