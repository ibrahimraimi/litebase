package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/mattn/go-sqlite3"
)

type Database struct {
	path               string
	opts               Options
	writerPool         *sql.DB
	readerPool         *sql.DB
	writer             *Writer
	hadMigrationsTable bool
	isClosed           atomic.Bool
}

func OpenDatabase(ctx context.Context, eng Engine, dbPath string, opts Options) (*Database, error) {
	resolvedOpts := opts.withDefaults()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory for %q: %w", dbPath, err)
	}
	writerPool, secState, err := eng.OpenWriterPool(dbPath, resolvedOpts)
	if err != nil {
		return nil, fmt.Errorf("open writer pool for %q: %w", dbPath, err)
	}
	writerConn, err := writerPool.Conn(ctx)
	if err != nil {
		_ = writerPool.Close()
		return nil, fmt.Errorf("acquire writer connection for %q: %w", dbPath, err)
	}
	readerPool, err := eng.OpenReaderPool(dbPath, resolvedOpts)
	if err != nil {
		_ = writerConn.Close()
		_ = writerPool.Close()
		return nil, fmt.Errorf("open reader pool for %q: %w", dbPath, err)
	}
	writer := NewWriter(writerConn, secState, resolvedOpts)
	db := &Database{
		path:       dbPath,
		opts:       resolvedOpts,
		writerPool: writerPool,
		readerPool: readerPool,
		writer:     writer,
	}
	if err := db.ensureInternalSchema(ctx); err != nil {
		_ = db.Close(ctx)
		return nil, err
	}
	return db, nil
}

func (db *Database) ensureInternalSchema(ctx context.Context) error {
	existing, err := db.ExecuteReadOnly(ctx, Statement{
		SQL: "SELECT name FROM sqlite_master WHERE type = 'table' AND name = '_litebase_migrations'",
	})
	if err == nil && len(existing.Rows) > 0 {
		db.hadMigrationsTable = true
		return nil
	}
	_, err = db.writer.SubmitPrivileged(ctx, []Statement{
		{
			SQL: `CREATE TABLE IF NOT EXISTS _litebase_migrations (
				version    TEXT PRIMARY KEY,
				name       TEXT NOT NULL,
				checksum   TEXT NOT NULL,
				applied_at TEXT NOT NULL,
				applied_by TEXT NOT NULL
			)`,
		},
	})
	return err
}

func (db *Database) HadInternalMigrationsTable() bool {
	return db.hadMigrationsTable
}

func (db *Database) Path() string {
	return db.path
}

func (db *Database) Durability() DurabilityMode {
	return db.opts.Durability
}

func (db *Database) Stats() WriterStats {
	return db.writer.Stats()
}

func (db *Database) Execute(ctx context.Context, stmt Statement) (QueryResult, error) {
	if db.isClosed.Load() {
		return QueryResult{}, ErrDatabaseClosed
	}
	if err := ValidateSingleStatement(stmt.SQL); err != nil {
		return QueryResult{}, err
	}
	if isReadCandidate(stmt.SQL) {
		return db.executeOnReaderPool(ctx, stmt)
	}
	outcomes, err := db.writer.Submit(ctx, []Statement{stmt})
	if err != nil {
		return QueryResult{}, err
	}
	return outcomes[0], nil
}

func (db *Database) ExecuteReadOnly(ctx context.Context, stmt Statement) (QueryResult, error) {
	if db.isClosed.Load() {
		return QueryResult{}, ErrDatabaseClosed
	}
	if err := ValidateSingleStatement(stmt.SQL); err != nil {
		return QueryResult{}, err
	}
	if !isReadCandidate(stmt.SQL) {
		return QueryResult{}, fmt.Errorf("%w: statement %q modifies database state", ErrReadOnlyViolation, extractLeadingKeyword(stmt.SQL))
	}
	return db.executeOnReaderPool(ctx, stmt)
}

func (db *Database) ExecuteBatch(ctx context.Context, statements []Statement) ([]QueryResult, error) {
	if db.isClosed.Load() {
		return nil, ErrDatabaseClosed
	}
	if len(statements) == 0 {
		return nil, ErrEmptyStatement
	}
	for idx, stmt := range statements {
		if err := ValidateSingleStatement(stmt.SQL); err != nil {
			return nil, fmt.Errorf("batch statement %d: %w", idx, err)
		}
	}
	return db.writer.Submit(ctx, statements)
}

func (db *Database) ExecutePrivilegedBatch(ctx context.Context, statements []Statement) ([]QueryResult, error) {
	if db.isClosed.Load() {
		return nil, ErrDatabaseClosed
	}
	if len(statements) == 0 {
		return nil, ErrEmptyStatement
	}
	for idx, stmt := range statements {
		if err := ValidateSingleStatement(stmt.SQL); err != nil {
			return nil, fmt.Errorf("privileged batch statement %d: %w", idx, err)
		}
	}
	return db.writer.SubmitPrivileged(ctx, statements)
}

func (db *Database) SetCommitHook(hook func() error) {
	db.writer.SetCommitHook(hook)
}

func (db *Database) VacuumInto(ctx context.Context, destinationPath string) error {
	if db.isClosed.Load() {
		return ErrDatabaseClosed
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	backupPool, err := openVacuumIntoPool(db.path)
	if err != nil {
		return err
	}
	defer backupPool.Close()

	if _, err := backupPool.ExecContext(ctx, "VACUUM INTO ?", destinationPath); err != nil {
		return fmt.Errorf("vacuum into %q: %w", destinationPath, err)
	}
	return nil
}

func openVacuumIntoPool(dbPath string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_busy_timeout=%d", dbPath, defaultBusyTimeoutMS)
	sqliteDriver := &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			return execRawPragmas(conn, []string{
				fmt.Sprintf("PRAGMA busy_timeout = %d", defaultBusyTimeoutMS),
				"PRAGMA temp_store = MEMORY",
			})
		},
	}
	pool := sql.OpenDB(&dsnConnector{dsn: dsn, sqliteDriver: sqliteDriver})
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	return pool, nil
}

func (db *Database) Checkpoint(ctx context.Context) error {
	if db.isClosed.Load() {
		return ErrDatabaseClosed
	}
	return db.writer.RunMaintenance(ctx, func(maintCtx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(maintCtx, "PRAGMA wal_checkpoint(TRUNCATE)")
		return err
	})
}

func (db *Database) IntegrityCheck(ctx context.Context) error {
	outcome, err := db.ExecuteReadOnly(ctx, Statement{SQL: "PRAGMA integrity_check"})
	if err != nil {
		return fmt.Errorf("run integrity_check: %w", err)
	}
	if len(outcome.Rows) == 0 || len(outcome.Rows[0]) == 0 || outcome.Rows[0][0].Value == nil {
		return errors.New("integrity_check returned no rows")
	}
	status := *outcome.Rows[0][0].Value
	if !strings.EqualFold(status, "ok") {
		return fmt.Errorf("integrity_check failed: %s", status)
	}
	return nil
}

func (db *Database) Close(ctx context.Context) error {
	if db.isClosed.Swap(true) {
		return nil
	}
	writerErr := db.writer.Close(ctx)
	readerErr := db.readerPool.Close()
	poolErr := db.writerPool.Close()
	return errors.Join(writerErr, readerErr, poolErr)
}

func (db *Database) executeOnReaderPool(ctx context.Context, stmt Statement) (QueryResult, error) {
	readerConn, err := db.readerPool.Conn(ctx)
	if err != nil {
		return QueryResult{}, fmt.Errorf("acquire reader connection: %w", err)
	}
	defer readerConn.Close()

	isReadOnly, err := inspectStatementReadOnly(readerConn, stmt.SQL)
	if err != nil {
		return QueryResult{}, err
	}
	if !isReadOnly {
		return QueryResult{}, fmt.Errorf("%w: statement is not read-only according to sqlite3_stmt_readonly", ErrReadOnlyViolation)
	}
	return executeStatementOnConn(ctx, readerConn, stmt, queryLimits{
		maxRows:          db.opts.MaxRows,
		maxResponseBytes: db.opts.MaxResponseBytes,
	})
}

func isReadCandidate(sqlText string) bool {
	keyword := extractLeadingKeyword(sqlText)
	switch keyword {
	case "select", "pragma", "values", "explain":
		return true
	case "with":
		return !containsMutationKeyword(sqlText)
	default:
		return false
	}
}

func containsMutationKeyword(sqlText string) bool {
	for _, token := range extractUnquotedTokens(sqlText) {
		switch strings.ToLower(token) {
		case "insert", "update", "delete", "replace", "create", "drop", "alter":
			return true
		}
	}
	return false
}
