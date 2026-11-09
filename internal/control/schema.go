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
	"strings"
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
	controlPath string
	dataDir     string
	cacheMu     sync.RWMutex
	tokenCache  map[string]cachedTokenEntry
	lastUsed    map[string]time.Time
	lastFlushAt time.Time
}

func OpenStore(ctx context.Context, controlDBPath string) (*Store, error) {
	cleanControlPath := filepath.Clean(controlDBPath)
	dataDir := filepath.Dir(cleanControlPath)
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create control db directory: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate", cleanControlPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open control db %q: %w", cleanControlPath, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	store := &Store{
		db:          db,
		controlPath: cleanControlPath,
		dataDir:     dataDir,
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

func (s *Store) ControlPath() string {
	return s.controlPath
}

func (s *Store) DataDir() string {
	return s.dataDir
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
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO projects (id, name, created_at) VALUES (?, ?, ?)`,
		DefaultProjectID,
		DefaultProjectName,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return err
	}
	return s.migrateToV1(ctx)
}

func (s *Store) migrateToV1(ctx context.Context) error {
	existingCols, err := s.listTableColumns(ctx, "snapshots")
	if err != nil {
		return err
	}
	alterCols := []struct {
		name string
		ddl  string
	}{
		{"rel_path", "ALTER TABLE snapshots ADD COLUMN rel_path TEXT"},
		{"sha256", "ALTER TABLE snapshots ADD COLUMN sha256 TEXT"},
		{"kind", "ALTER TABLE snapshots ADD COLUMN kind TEXT NOT NULL DEFAULT 'manual'"},
		{"pinned", "ALTER TABLE snapshots ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0"},
		{"remote_key", "ALTER TABLE snapshots ADD COLUMN remote_key TEXT"},
		{"remote_status", "ALTER TABLE snapshots ADD COLUMN remote_status TEXT NOT NULL DEFAULT 'none'"},
		{"remote_uploaded_at", "ALTER TABLE snapshots ADD COLUMN remote_uploaded_at TEXT"},
	}
	for _, col := range alterCols {
		if _, exists := existingCols[col.name]; !exists {
			if _, err := s.db.ExecContext(ctx, col.ddl); err != nil {
				return fmt.Errorf("alter snapshots table (%s): %w", col.name, err)
			}
		}
	}
	if err := s.backfillLegacySnapshots(ctx); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "PRAGMA user_version = 1")
	return err
}

func (s *Store) listTableColumns(ctx context.Context, tableName string) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]struct{})
	for rows.Next() {
		var cid, notNull, pk int
		var name, colType string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
}

func (s *Store) backfillLegacySnapshots(ctx context.Context) error {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, path, label FROM snapshots WHERE rel_path IS NULL OR rel_path = ''`,
	)
	if err != nil {
		return err
	}
	type legacyRow struct {
		id    string
		path  string
		label string
	}
	var pending []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.id, &r.path, &r.label); err != nil {
			_ = rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, r := range pending {
		relPath := extractRelativePathInDataDir(s.dataDir, r.path, "snapshots")
		kind := InferSnapshotKind(r.label)
		if _, err := s.db.ExecContext(
			ctx,
			`UPDATE snapshots SET rel_path = ?, kind = ? WHERE id = ?`,
			relPath,
			kind,
			r.id,
		); err != nil {
			return err
		}
	}
	return nil
}

func InferSnapshotKind(label string) string {
	switch {
	case label == "pre-restore":
		return "pre-restore"
	case label == "pre-push":
		return "pre-push"
	case label == "scheduled":
		return "scheduled"
	case strings.HasPrefix(label, "branch-"):
		return "branch"
	default:
		return "manual"
	}
}

func extractRelativePathInDataDir(dataDir string, storedPath string, markerDir string) string {
	cleanStored := filepath.Clean(storedPath)
	if !filepath.IsAbs(cleanStored) {
		return filepath.ToSlash(cleanStored)
	}
	if dataDir != "" {
		if rel, err := filepath.Rel(filepath.Clean(dataDir), cleanStored); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	slashPath := filepath.ToSlash(cleanStored)
	needle := "/" + markerDir + "/"
	if idx := strings.LastIndex(slashPath, needle); idx >= 0 {
		return slashPath[idx+1:]
	}
	return ""
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
	`CREATE TABLE IF NOT EXISTS backup_policies (
		database_id TEXT PRIMARY KEY REFERENCES databases(id),
		interval_seconds INTEGER NOT NULL,
		keep_last INTEGER NOT NULL DEFAULT 24,
		keep_days INTEGER NOT NULL DEFAULT 14,
		remote INTEGER NOT NULL DEFAULT 0,
		enabled INTEGER NOT NULL DEFAULT 1,
		last_run_at TEXT,
		last_success_at TEXT,
		last_error TEXT,
		created_at TEXT NOT NULL
	)`,
}
