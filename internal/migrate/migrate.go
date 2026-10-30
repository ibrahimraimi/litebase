package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
)

var (
	ErrChecksumMismatch = errors.New("applied migration checksum mismatch")
	ErrDeployInProgress = errors.New("another migration deploy is already in progress for this database")
)

type MigrationInput struct {
	Version string `json:"version"`
	Name    string `json:"name"`
	SQL     string `json:"sql"`
}

type MigrationPlan struct {
	Applied []control.AppliedMigrationRecord `json:"applied"`
	Pending []MigrationInput                 `json:"pending"`
}

type ApplyResult struct {
	AppliedVersions []string `json:"applied_versions"`
	DryRun          bool     `json:"dry_run"`
}

type SchemaObject struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	TableName string `json:"table_name"`
	SQL       string `json:"sql"`
}

type Runner struct {
	mgr     *manager.Manager
	locksMu sync.Mutex
	dbLocks map[string]*sync.Mutex
}

func NewRunner(mgr *manager.Manager) *Runner {
	return &Runner{
		mgr:     mgr,
		dbLocks: make(map[string]*sync.Mutex),
	}
}

func ComputeChecksum(sqlContent string) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(sqlContent), "\r\n", "\n")
	digest := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(digest[:])
}

func LoadDirectoryMigrations(dirPath string) ([]MigrationInput, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read migrations directory %q: %w", dirPath, err)
	}
	var migrations []MigrationInput
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := parseMigrationVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		contentBytes, err := os.ReadFile(filepath.Join(dirPath, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration file %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, MigrationInput{
			Version: version,
			Name:    entry.Name(),
			SQL:     string(contentBytes),
		})
	}
	sortMigrations(migrations)
	return migrations, nil
}

func parseMigrationVersion(filename string) (string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	parts := strings.SplitN(base, "_", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
		return "", fmt.Errorf("invalid migration filename %q: expected format NNNN_description.sql", filename)
	}
	return parts[0], nil
}

func sortMigrations(migrations []MigrationInput) {
	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
}

func (r *Runner) ListApplied(ctx context.Context, dbName string) ([]control.AppliedMigrationRecord, error) {
	entry, err := r.mgr.Acquire(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer r.mgr.Release(entry)
	return listAppliedFromDatabase(ctx, entry.DB, entry.Record.ID)
}

func listAppliedFromDatabase(
	ctx context.Context,
	db *engine.Database,
	databaseID string,
) ([]control.AppliedMigrationRecord, error) {
	outcome, err := db.ExecuteReadOnly(ctx, engine.Statement{
		SQL: `SELECT version, name, checksum, applied_at, applied_by
		      FROM _litebase_migrations
		      ORDER BY version ASC`,
	})
	if err != nil {
		return nil, fmt.Errorf("query _litebase_migrations: %w", err)
	}
	records := make([]control.AppliedMigrationRecord, 0, len(outcome.Rows))
	for _, row := range outcome.Rows {
		if len(row) < 5 {
			continue
		}
		appliedAt, _ := time.Parse(time.RFC3339Nano, derefString(row[3].Value))
		records = append(records, control.AppliedMigrationRecord{
			DatabaseID: databaseID,
			Version:    derefString(row[0].Value),
			Name:       derefString(row[1].Value),
			Checksum:   derefString(row[2].Value),
			AppliedAt:  appliedAt,
			AppliedBy:  derefString(row[4].Value),
		})
	}
	return records, nil
}

func (r *Runner) BuildPlan(
	ctx context.Context,
	dbName string,
	inputs []MigrationInput,
) (MigrationPlan, error) {
	entry, err := r.mgr.Acquire(ctx, dbName)
	if err != nil {
		return MigrationPlan{}, err
	}
	defer r.mgr.Release(entry)
	return buildPlanWithDB(ctx, entry.DB, entry.Record.ID, inputs)
}

func buildPlanWithDB(
	ctx context.Context,
	db *engine.Database,
	databaseID string,
	inputs []MigrationInput,
) (MigrationPlan, error) {
	sorted := append([]MigrationInput(nil), inputs...)
	sortMigrations(sorted)

	applied, err := listAppliedFromDatabase(ctx, db, databaseID)
	if err != nil {
		return MigrationPlan{}, err
	}
	appliedByVersion := make(map[string]control.AppliedMigrationRecord, len(applied))
	for _, rec := range applied {
		appliedByVersion[rec.Version] = rec
	}

	var pending []MigrationInput
	for _, candidate := range sorted {
		checksum := ComputeChecksum(candidate.SQL)
		if existing, isApplied := appliedByVersion[candidate.Version]; isApplied {
			if existing.Checksum != checksum {
				return MigrationPlan{}, fmt.Errorf(
					"%w for version %s (%s): expected %s, got %s",
					ErrChecksumMismatch,
					candidate.Version,
					candidate.Name,
					existing.Checksum,
					checksum,
				)
			}
			continue
		}
		pending = append(pending, candidate)
	}
	return MigrationPlan{Applied: applied, Pending: pending}, nil
}

func (r *Runner) ApplyPending(
	ctx context.Context,
	dbName string,
	actor string,
	inputs []MigrationInput,
) ([]string, error) {
	dbLock := r.lockForDatabase(dbName)
	if !dbLock.TryLock() {
		return nil, ErrDeployInProgress
	}
	defer dbLock.Unlock()

	entry, err := r.mgr.Acquire(ctx, dbName)
	if err != nil {
		return nil, err
	}
	defer r.mgr.Release(entry)

	plan, err := buildPlanWithDB(ctx, entry.DB, entry.Record.ID, inputs)
	if err != nil {
		return nil, err
	}
	if len(plan.Pending) == 0 {
		return []string{}, nil
	}
	if err := executePendingAndRecordInOneTx(ctx, entry.DB, actor, plan.Pending); err != nil {
		return nil, err
	}
	r.recordAuditCopyBestEffort(ctx, entry.Record.ID, actor, plan.Pending)
	appliedVersions := make([]string, len(plan.Pending))
	for i, mig := range plan.Pending {
		appliedVersions[i] = mig.Version
	}
	return appliedVersions, nil
}

func executePendingAndRecordInOneTx(
	ctx context.Context,
	db *engine.Database,
	actor string,
	pending []MigrationInput,
) error {
	var allStatements []engine.Statement
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for _, mig := range pending {
		splitStmts := engine.SplitSQLStatements(mig.SQL)
		for _, rawSQL := range splitStmts {
			allStatements = append(allStatements, engine.Statement{SQL: rawSQL})
		}
		vVal, _ := engine.NewTextValue(mig.Version)
		nVal, _ := engine.NewTextValue(mig.Name)
		cVal, _ := engine.NewTextValue(ComputeChecksum(mig.SQL))
		tVal, _ := engine.NewTextValue(nowStr)
		bVal, _ := engine.NewTextValue(actor)
		allStatements = append(allStatements, engine.Statement{
			SQL: `INSERT INTO _litebase_migrations (version, name, checksum, applied_at, applied_by)
			      VALUES (?, ?, ?, ?, ?)`,
			Params: []engine.TypedValue{vVal, nVal, cVal, tVal, bVal},
		})
	}
	_, err := db.ExecutePrivilegedBatch(ctx, allStatements)
	return err
}

func (r *Runner) recordAuditCopyBestEffort(
	ctx context.Context,
	databaseID string,
	actor string,
	pending []MigrationInput,
) {
	for _, mig := range pending {
		_ = r.mgr.Store().RecordAppliedMigration(ctx, control.AppliedMigrationRecord{
			DatabaseID: databaseID,
			Version:    mig.Version,
			Name:       mig.Name,
			Checksum:   ComputeChecksum(mig.SQL),
			AppliedBy:  actor,
		})
	}
}

func (r *Runner) lockForDatabase(dbName string) *sync.Mutex {
	r.locksMu.Lock()
	defer r.locksMu.Unlock()
	if existing, ok := r.dbLocks[dbName]; ok {
		return existing
	}
	created := &sync.Mutex{}
	r.dbLocks[dbName] = created
	return created
}

func InspectSchema(ctx context.Context, db *engine.Database) ([]SchemaObject, error) {
	outcome, err := db.ExecuteReadOnly(ctx, engine.Statement{
		SQL: `SELECT type, name, tbl_name, COALESCE(sql, '')
		      FROM sqlite_master
		      WHERE name NOT LIKE 'sqlite_%'
		        AND name NOT LIKE '_litebase_%'
		        AND tbl_name NOT LIKE '_litebase_%'
		        AND sql IS NOT NULL AND sql != ''
		      ORDER BY CASE type WHEN 'table' THEN 1 WHEN 'index' THEN 2 WHEN 'trigger' THEN 3 WHEN 'view' THEN 4 ELSE 5 END, name ASC`,
	})
	if err != nil {
		return nil, fmt.Errorf("inspect sqlite_master schema: %w", err)
	}
	objects := make([]SchemaObject, 0, len(outcome.Rows))
	for _, row := range outcome.Rows {
		if len(row) < 4 {
			continue
		}
		objects = append(objects, SchemaObject{
			Type:      derefString(row[0].Value),
			Name:      derefString(row[1].Value),
			TableName: derefString(row[2].Value),
			SQL:       derefString(row[3].Value),
		})
	}
	return objects, nil
}

func InspectSchemaFile(ctx context.Context, dbFilePath string) ([]SchemaObject, error) {
	pool, err := engine.NewSQLiteEngine().OpenReaderPool(dbFilePath, engine.Options{ReaderPoolMax: 1})
	if err != nil {
		return nil, fmt.Errorf("open database file for schema inspection: %w", err)
	}
	defer pool.Close()

	rows, err := pool.QueryContext(ctx, `
		SELECT type, name, tbl_name, COALESCE(sql, '')
		FROM sqlite_master
		WHERE name NOT LIKE 'sqlite_%'
		  AND name NOT LIKE '_litebase_%'
		  AND tbl_name NOT LIKE '_litebase_%'
		  AND sql IS NOT NULL AND sql != ''
		ORDER BY CASE type WHEN 'table' THEN 1 WHEN 'index' THEN 2 WHEN 'trigger' THEN 3 WHEN 'view' THEN 4 ELSE 5 END, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("inspect sqlite_master schema from file: %w", err)
	}
	defer rows.Close()

	var objects []SchemaObject
	for rows.Next() {
		var obj SchemaObject
		if err := rows.Scan(&obj.Type, &obj.Name, &obj.TableName, &obj.SQL); err != nil {
			return nil, fmt.Errorf("scan schema object: %w", err)
		}
		objects = append(objects, obj)
	}
	return objects, rows.Err()
}

func derefString(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}
