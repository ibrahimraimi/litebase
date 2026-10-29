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

	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/manager"
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

func (r *Runner) BuildPlan(
	ctx context.Context,
	databaseID string,
	inputs []MigrationInput,
) (MigrationPlan, error) {
	sorted := append([]MigrationInput(nil), inputs...)
	sortMigrations(sorted)

	applied, err := r.mgr.Store().ListAppliedMigrations(ctx, databaseID)
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

	plan, err := r.BuildPlan(ctx, entry.Record.ID, inputs)
	if err != nil {
		return nil, err
	}
	if len(plan.Pending) == 0 {
		return []string{}, nil
	}
	if err := executePendingBatch(ctx, entry.DB, plan.Pending); err != nil {
		return nil, err
	}
	return r.recordAppliedList(ctx, entry.Record.ID, actor, plan.Pending)
}

func executePendingBatch(ctx context.Context, db *engine.Database, pending []MigrationInput) error {
	var allStatements []engine.Statement
	for _, mig := range pending {
		splitStmts := engine.SplitSQLStatements(mig.SQL)
		for _, rawSQL := range splitStmts {
			allStatements = append(allStatements, engine.Statement{SQL: rawSQL})
		}
	}
	if len(allStatements) == 0 {
		return nil
	}
	_, err := db.ExecuteBatch(ctx, allStatements)
	return err
}

func (r *Runner) recordAppliedList(
	ctx context.Context,
	databaseID string,
	actor string,
	pending []MigrationInput,
) ([]string, error) {
	appliedVersions := make([]string, 0, len(pending))
	for _, mig := range pending {
		err := r.mgr.Store().RecordAppliedMigration(ctx, control.AppliedMigrationRecord{
			DatabaseID: databaseID,
			Version:    mig.Version,
			Name:       mig.Name,
			Checksum:   ComputeChecksum(mig.SQL),
			AppliedBy:  actor,
		})
		if err != nil {
			return appliedVersions, err
		}
		appliedVersions = append(appliedVersions, mig.Version)
	}
	return appliedVersions, nil
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
		      WHERE name NOT LIKE 'sqlite_%' AND sql IS NOT NULL AND sql != ''
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

func GenerateSchemaDiffSQL(baseSchema []SchemaObject, targetSchema []SchemaObject) []string {
	baseMap := indexSchemaByKey(baseSchema)
	targetMap := indexSchemaByKey(targetSchema)

	var statements []string
	for _, targetObj := range targetSchema {
		key := schemaKey(targetObj)
		baseObj, exists := baseMap[key]
		if !exists {
			statements = append(statements, ensureTrailingSemicolon(targetObj.SQL))
			continue
		}
		if normalizeSQLWhitespace(baseObj.SQL) != normalizeSQLWhitespace(targetObj.SQL) {
			statements = append(statements,
				fmt.Sprintf("DROP %s IF EXISTS %s;", strings.ToUpper(targetObj.Type), targetObj.Name),
				ensureTrailingSemicolon(targetObj.SQL),
			)
		}
	}
	for _, baseObj := range baseSchema {
		key := schemaKey(baseObj)
		if _, stillExists := targetMap[key]; !stillExists {
			statements = append(statements,
				fmt.Sprintf("DROP %s IF EXISTS %s;", strings.ToUpper(baseObj.Type), baseObj.Name),
			)
		}
	}
	return statements
}

func indexSchemaByKey(objects []SchemaObject) map[string]SchemaObject {
	indexed := make(map[string]SchemaObject, len(objects))
	for _, obj := range objects {
		indexed[schemaKey(obj)] = obj
	}
	return indexed
}

func schemaKey(obj SchemaObject) string {
	return obj.Type + ":" + obj.Name
}

func normalizeSQLWhitespace(sqlText string) string {
	return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(sqlText), ";")), " ")
}

func ensureTrailingSemicolon(sqlText string) string {
	trimmed := strings.TrimSpace(sqlText)
	if strings.HasSuffix(trimmed, ";") {
		return trimmed
	}
	return trimmed + ";"
}

func derefString(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}
