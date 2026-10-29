package engine

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

type DurabilityMode string

const (
	DurabilityNormal DurabilityMode = "normal"
	DurabilityFull   DurabilityMode = "full"

	defaultBusyTimeoutMS     = 5000
	defaultWALAutoCheckpoint = 1000
	defaultReaderPoolSize    = 8
	defaultMaxRows           = 10000
	defaultMaxQueueDepth     = 4096
	defaultMaxBatchSize      = 64
)

var (
	ErrEmptyStatement            = errors.New("SQL statement is empty")
	ErrMultiStatementNotAllowed  = errors.New("multiple SQL statements are not allowed in a single query; use /batch")
	ErrInteractiveTxNotSupported = errors.New("interactive transaction statements (BEGIN, COMMIT, ROLLBACK, SAVEPOINT, RELEASE) are not supported over HTTP; use /batch")
	ErrAuthorizationDenied       = errors.New("SQL statement denied by security authorizer")
	ErrQueueFull                 = errors.New("write queue is full")
	ErrDatabaseClosed            = errors.New("database is closed")
	ErrReadOnlyViolation         = errors.New("write statement is not permitted with read-only access")
)

type Options struct {
	Durability    DurabilityMode
	ReaderPoolMax int
	MaxRows       int
	MaxQueueDepth int
	MaxBatchSize  int
	BatchWait     time.Duration
}

func (opts Options) withDefaults() Options {
	resolved := opts
	if resolved.Durability == "" {
		resolved.Durability = DurabilityNormal
	}
	if resolved.ReaderPoolMax <= 0 {
		resolved.ReaderPoolMax = defaultReaderPoolSize
	}
	if resolved.MaxRows <= 0 {
		resolved.MaxRows = defaultMaxRows
	}
	if resolved.MaxQueueDepth <= 0 {
		resolved.MaxQueueDepth = defaultMaxQueueDepth
	}
	if resolved.MaxBatchSize <= 0 {
		resolved.MaxBatchSize = defaultMaxBatchSize
	}
	return resolved
}

type Statement struct {
	SQL    string       `json:"sql"`
	Params []TypedValue `json:"params,omitempty"`
}

type QueryResult struct {
	Columns         []string       `json:"columns"`
	Rows            [][]TypedValue `json:"rows"`
	RowsAffected    int64          `json:"rows_affected"`
	LastInsertRowID *int64         `json:"last_insert_rowid"`
	Truncated       bool           `json:"truncated,omitempty"`
	DurationMS      float64        `json:"duration_ms"`
}

type Engine interface {
	OpenWriterPool(dbPath string, opts Options) (*sql.DB, *connectionSecurityState, error)
	OpenReaderPool(dbPath string, opts Options) (*sql.DB, error)
}

type SQLiteEngine struct{}

func NewSQLiteEngine() *SQLiteEngine {
	return &SQLiteEngine{}
}

func (e *SQLiteEngine) OpenWriterPool(dbPath string, opts Options) (*sql.DB, *connectionSecurityState, error) {
	resolvedOpts := opts.withDefaults()
	secState := &connectionSecurityState{}
	dsn := buildWriterDSN(dbPath)
	sqliteDriver := &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := applyWriterPragmas(conn, resolvedOpts.Durability); err != nil {
				return err
			}
			registerWriterAuthorizer(conn, secState)
			return nil
		},
	}
	pool := sql.OpenDB(&dsnConnector{dsn: dsn, sqliteDriver: sqliteDriver})
	// Keep exactly one persistent writer connection so connection-scoped pragmas
	// and authorizer state are never lost on pool recycle.
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxLifetime(0)
	pool.SetConnMaxIdleTime(0)
	return pool, secState, nil
}

func (e *SQLiteEngine) OpenReaderPool(dbPath string, opts Options) (*sql.DB, error) {
	resolvedOpts := opts.withDefaults()
	dsn := buildReaderDSN(dbPath)
	sqliteDriver := &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			if err := applyReaderPragmas(conn); err != nil {
				return err
			}
			conn.SetLimit(sqlite3.SQLITE_LIMIT_ATTACHED, 0)
			registerReaderAuthorizer(conn)
			return nil
		},
	}
	pool := sql.OpenDB(&dsnConnector{dsn: dsn, sqliteDriver: sqliteDriver})
	pool.SetMaxOpenConns(resolvedOpts.ReaderPoolMax)
	pool.SetMaxIdleConns(resolvedOpts.ReaderPoolMax)
	pool.SetConnMaxLifetime(0)
	return pool, nil
}

type dsnConnector struct {
	dsn          string
	sqliteDriver driver.Driver
}

func (c *dsnConnector) Connect(_ context.Context) (driver.Conn, error) {
	return c.sqliteDriver.Open(c.dsn)
}

func (c *dsnConnector) Driver() driver.Driver {
	return c.sqliteDriver
}

func buildWriterDSN(dbPath string) string {
	query := url.Values{}
	query.Set("_txlock", "immediate")
	return fmt.Sprintf("file:%s?%s", dbPath, query.Encode())
}

func buildReaderDSN(dbPath string) string {
	query := url.Values{}
	query.Set("mode", "ro")
	return fmt.Sprintf("file:%s?%s", dbPath, query.Encode())
}

func applyWriterPragmas(conn *sqlite3.SQLiteConn, durability DurabilityMode) error {
	syncValue := "NORMAL"
	if durability == DurabilityFull {
		syncValue = "FULL"
	}
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = " + syncValue,
		"PRAGMA foreign_keys = ON",
		fmt.Sprintf("PRAGMA busy_timeout = %d", defaultBusyTimeoutMS),
		fmt.Sprintf("PRAGMA wal_autocheckpoint = %d", defaultWALAutoCheckpoint),
		"PRAGMA temp_store = MEMORY",
	}
	return execRawPragmas(conn, pragmas)
}

func applyReaderPragmas(conn *sqlite3.SQLiteConn) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		fmt.Sprintf("PRAGMA busy_timeout = %d", defaultBusyTimeoutMS),
		"PRAGMA temp_store = MEMORY",
		"PRAGMA query_only = ON",
	}
	return execRawPragmas(conn, pragmas)
}

func execRawPragmas(conn *sqlite3.SQLiteConn, pragmas []string) error {
	for _, pragmaSQL := range pragmas {
		if _, err := conn.Exec(pragmaSQL, nil); err != nil {
			return fmt.Errorf("apply connection pragma %q: %w", pragmaSQL, err)
		}
	}
	return nil
}

func ValidateSingleStatement(sqlText string) error {
	trimmed := stripLeadingCommentsAndSpace(sqlText)
	if trimmed == "" {
		return ErrEmptyStatement
	}
	if hasMultipleStatements(trimmed) {
		return ErrMultiStatementNotAllowed
	}
	firstKeyword := extractLeadingKeyword(trimmed)
	switch firstKeyword {
	case "begin", "commit", "end", "rollback", "savepoint", "release":
		return ErrInteractiveTxNotSupported
	case "attach", "detach", "vacuum":
		return fmt.Errorf("%w: %s is not allowed", ErrAuthorizationDenied, strings.ToUpper(firstKeyword))
	default:
		return nil
	}
}

func inspectStatementReadOnly(conn *sql.Conn, sqlText string) (bool, error) {
	if err := ValidateSingleStatement(sqlText); err != nil {
		return false, err
	}
	var isReadOnly bool
	err := conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("unexpected underlying SQLite driver connection type")
		}
		stmt, err := sqliteConn.Prepare(sqlText)
		if err != nil {
			return normalizeSQLiteError(err)
		}
		defer stmt.Close()
		sqliteStmt, ok := stmt.(*sqlite3.SQLiteStmt)
		if !ok {
			return errors.New("unexpected underlying SQLite statement type")
		}
		isReadOnly = sqliteStmt.Readonly()
		return nil
	})
	return isReadOnly, err
}

func normalizeSQLiteError(err error) error {
	if err == nil {
		return nil
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		if sqliteErr.Code == sqlite3.ErrAuth {
			return fmt.Errorf("%w: %s", ErrAuthorizationDenied, sqliteErr.Error())
		}
		if sqliteErr.Code == sqlite3.ErrReadonly {
			return fmt.Errorf("%w: %s", ErrReadOnlyViolation, sqliteErr.Error())
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "not authorized") {
		return fmt.Errorf("%w: %s", ErrAuthorizationDenied, err.Error())
	}
	return err
}
