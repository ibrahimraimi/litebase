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
)

type Database struct {
	path       string
	opts       Options
	writerPool *sql.DB
	readerPool *sql.DB
	writer     *Writer
	isClosed   atomic.Bool
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
	return &Database{
		path:       dbPath,
		opts:       resolvedOpts,
		writerPool: writerPool,
		readerPool: readerPool,
		writer:     writer,
	}, nil
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

func (db *Database) VacuumInto(ctx context.Context, destinationPath string) error {
	if db.isClosed.Load() {
		return ErrDatabaseClosed
	}
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	return db.writer.RunMaintenance(ctx, func(maintCtx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(maintCtx, "VACUUM INTO ?", destinationPath)
		if err != nil {
			return fmt.Errorf("vacuum into %q: %w", destinationPath, err)
		}
		return nil
	})
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
	return executeStatementOnConn(ctx, readerConn, stmt, db.opts.MaxRows)
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
