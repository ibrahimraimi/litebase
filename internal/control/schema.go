package control

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/mattn/go-sqlite3"
)

const (
	DefaultProjectID             = "proj_default"
	DefaultProjectName           = "default"
	defaultTokenCacheTTL         = 30 * time.Second
	defaultLastUsedFlushInterval = 5 * time.Second
)

var (
	ErrNotFound      = errors.New("record not found")
	ErrAlreadyExists = errors.New("record already exists")
)

type cachedTokenEntry struct {
	record    auth.TokenRecord
	expiresAt time.Time
}

type Store struct {
	db          *sql.DB
	cacheMu     sync.RWMutex
	tokenCache  map[string]cachedTokenEntry
	lastUsed    map[string]time.Time
	lastFlushAt time.Time
}

func OpenStore(ctx context.Context, controlDBPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(controlDBPath), 0o750); err != nil {
		return nil, fmt.Errorf("create control db directory: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate", controlDBPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open control db %q: %w", controlDBPath, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	store := &Store{
		db:          db,
		tokenCache:  make(map[string]cachedTokenEntry),
		lastUsed:    make(map[string]time.Time),
		lastFlushAt: time.Now().UTC(),
	}
	if err := store.migrateSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) Close() error {
	s.flushPendingLastUsed(context.Background())
	return s.db.Close()
}

func (s *Store) migrateSchema(ctx context.Context) error {
	for _, ddl := range controlSchemaStatements {
		if _, err := s.db.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("migrate control schema: %w", err)
		}
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO projects (id, name, created_at) VALUES (?, ?, ?)`,
		DefaultProjectID,
		DefaultProjectName,
		time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

func NewSortableID(prefix string) (string, error) {
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate id entropy: %w", err)
	}
	timestampHex := fmt.Sprintf("%012x", time.Now().UTC().UnixMilli())
	return fmt.Sprintf("%s_%s%s", prefix, timestampHex, hex.EncodeToString(entropy[:])), nil
}

func isUniqueConstraintError(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique ||
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}
	return false
}

var controlSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS projects (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS databases (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id),
		name TEXT NOT NULL,
		path TEXT NOT NULL,
		durability TEXT NOT NULL,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		deleted_at TEXT
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_databases_active_name
		ON databases(project_id, name) WHERE deleted_at IS NULL`,
	`CREATE TABLE IF NOT EXISTS tokens (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL REFERENCES projects(id),
		database_id TEXT,
		prefix TEXT NOT NULL,
		hash TEXT NOT NULL UNIQUE,
		scopes TEXT NOT NULL,
		expires_at TEXT,
		last_used_at TEXT,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS migrations_applied (
		database_id TEXT NOT NULL REFERENCES databases(id),
		version TEXT NOT NULL,
		name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL,
		applied_by TEXT NOT NULL,
		PRIMARY KEY (database_id, version)
	)`,
	`CREATE TABLE IF NOT EXISTS snapshots (
		id TEXT PRIMARY KEY,
		database_id TEXT NOT NULL REFERENCES databases(id),
		path TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		label TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS branches (
		id TEXT PRIMARY KEY,
		database_id TEXT NOT NULL REFERENCES databases(id),
		parent_id TEXT NOT NULL REFERENCES databases(id),
		parent_snapshot_id TEXT REFERENCES snapshots(id),
		name TEXT NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE(parent_id, name)
	)`,
	`CREATE TABLE IF NOT EXISTS audit_log (
		id TEXT PRIMARY KEY,
		ts TEXT NOT NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL,
		detail_json TEXT NOT NULL
	)`,
}
