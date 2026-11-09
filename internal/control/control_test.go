package control

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/ibrahimraimi/litebase/internal/engine"
)

func TestControlStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	controlPath := filepath.Join(t.TempDir(), "control.db")
	store, err := OpenStore(ctx, controlPath)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	dbRec, err := store.CreateDatabase(ctx, CreateDatabaseParams{
		Name:       "main",
		Path:       filepath.Join(t.TempDir(), "main.sqlite"),
		Durability: engine.DurabilityFull,
	})
	if err != nil {
		t.Fatalf("CreateDatabase failed: %v", err)
	}

	if _, err := store.CreateDatabase(ctx, CreateDatabaseParams{
		Name: "main",
		Path: filepath.Join(t.TempDir(), "dup.sqlite"),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists on duplicate database name, got %v", err)
	}

	fetched, err := store.GetDatabaseByName(ctx, DefaultProjectID, "main")
	if err != nil || fetched.ID != dbRec.ID {
		t.Fatalf("GetDatabaseByName mismatch: got %+v, err %v", fetched, err)
	}

	tokRec, plaintext, err := store.CreateToken(ctx, CreateTokenParams{
		DatabaseID: dbRec.ID,
		Scopes:     []auth.Scope{auth.ScopeRead, auth.ScopeWrite},
	})
	if err != nil {
		t.Fatalf("CreateToken failed: %v", err)
	}

	authenticated, err := store.AuthenticateToken(ctx, plaintext)
	if err != nil || authenticated.ID != tokRec.ID {
		t.Fatalf("AuthenticateToken failed: got %+v, err %v", authenticated, err)
	}

	if err := store.RecordAppliedMigration(ctx, AppliedMigrationRecord{
		DatabaseID: dbRec.ID,
		Version:    "0001",
		Name:       "0001_init.sql",
		Checksum:   "abc123",
		AppliedBy:  tokRec.ID,
	}); err != nil {
		t.Fatalf("RecordAppliedMigration failed: %v", err)
	}

	migrations, err := store.ListAppliedMigrations(ctx, dbRec.ID)
	if err != nil || len(migrations) != 1 || migrations[0].Version != "0001" {
		t.Fatalf("ListAppliedMigrations mismatch: %+v, err %v", migrations, err)
	}

	snapRec, err := store.CreateSnapshotRecord(ctx, CreateSnapshotParams{
		DatabaseID:   dbRec.ID,
		SnapshotPath: "/tmp/snap.sqlite",
		SizeBytes:    4096,
		Label:        "pre-deploy",
	})
	if err != nil {
		t.Fatalf("CreateSnapshotRecord failed: %v", err)
	}
	snaps, err := store.ListSnapshotRecords(ctx, dbRec.ID)
	if err != nil || len(snaps) != 1 || snaps[0].ID != snapRec.ID {
		t.Fatalf("ListSnapshotRecords mismatch: %+v, err %v", snaps, err)
	}

	if err := store.RecordAudit(ctx, AuditParams{
		Actor:   tokRec.ID,
		Action:  "database.create",
		Target:  dbRec.Name,
		Details: map[string]string{"durability": "full"},
	}); err != nil {
		t.Fatalf("RecordAudit failed: %v", err)
	}

	if err := store.SoftDeleteDatabase(ctx, DefaultProjectID, "main"); err != nil {
		t.Fatalf("SoftDeleteDatabase failed: %v", err)
	}
	if _, err := store.GetDatabaseByName(ctx, DefaultProjectID, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after SoftDeleteDatabase, got %v", err)
	}
}

func TestTokenCacheInvalidationAndAdminCheck(t *testing.T) {
	ctx := context.Background()
	controlPath := filepath.Join(t.TempDir(), "control.db")
	store, err := OpenStore(ctx, controlPath)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	hasAdmin, err := store.HasAdminToken(ctx)
	if err != nil || hasAdmin {
		t.Fatalf("expected HasAdminToken=false on empty store, got %v, err=%v", hasAdmin, err)
	}

	tokRec, plaintext, err := store.CreateToken(ctx, CreateTokenParams{
		Scopes: []auth.Scope{auth.ScopeAdmin},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	hasAdmin, err = store.HasAdminToken(ctx)
	if err != nil || !hasAdmin {
		t.Fatalf("expected HasAdminToken=true after creating admin token, got %v, err=%v", hasAdmin, err)
	}

	for i := 0; i < 5; i++ {
		if _, err := store.AuthenticateToken(ctx, plaintext); err != nil {
			t.Fatalf("cached AuthenticateToken(%d) failed: %v", i, err)
		}
	}

	if err := store.RevokeToken(ctx, tokRec.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := store.AuthenticateToken(ctx, plaintext); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected revoked token to fail immediately despite cache, got: %v", err)
	}
}

func TestUpgradeFrom002SchemaBackfillsRelPathAndKind(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	controlPath := filepath.Join(dataDir, "control.db")

	rawDB, err := sql.Open("sqlite3", controlPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	v002DDL := `
	CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL);
	CREATE TABLE databases (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL, path TEXT NOT NULL, durability TEXT NOT NULL, size_bytes INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, deleted_at TEXT);
	CREATE TABLE snapshots (id TEXT PRIMARY KEY, database_id TEXT NOT NULL, path TEXT NOT NULL, size_bytes INTEGER NOT NULL, created_at TEXT NOT NULL, label TEXT NOT NULL);
	INSERT INTO projects (id, name, created_at) VALUES ('proj_default', 'default', '2026-10-29T00:00:00Z');
	INSERT INTO databases (id, project_id, name, path, durability, size_bytes, created_at) VALUES ('db_1', 'proj_default', 'main', '/old/root/databases/default/main/main.sqlite', 'normal', 4096, '2026-10-29T00:00:00Z');
	INSERT INTO snapshots (id, database_id, path, size_bytes, created_at, label) VALUES
		('snap_1', 'db_1', '/old/root/snapshots/main/100-manual.sqlite', 4096, '2026-10-29T01:00:00Z', 'manual'),
		('snap_2', 'db_1', '/old/root/snapshots/main/200-pre-restore.sqlite', 4096, '2026-10-29T02:00:00Z', 'pre-restore'),
		('snap_3', 'db_1', '/old/root/snapshots/main/300-branch-dev.sqlite', 4096, '2026-10-29T03:00:00Z', 'branch-dev');
	`
	if _, err := rawDB.ExecContext(ctx, v002DDL); err != nil {
		t.Fatalf("seed 0.0.2 schema: %v", err)
	}
	_ = rawDB.Close()

	store, err := OpenStore(ctx, controlPath)
	if err != nil {
		t.Fatalf("OpenStore upgrade from 0.0.2: %v", err)
	}
	defer store.Close()

	snaps, err := store.ListSnapshotRecords(ctx, "db_1")
	if err != nil || len(snaps) != 3 {
		t.Fatalf("ListSnapshotRecords after upgrade: len=%d err=%v", len(snaps), err)
	}
	byID := make(map[string]SnapshotRecord, len(snaps))
	for _, s := range snaps {
		byID[s.ID] = s
	}

	if byID["snap_1"].RelPath != "snapshots/main/100-manual.sqlite" || byID["snap_1"].Kind != SnapshotKindManual {
		t.Fatalf("unexpected snap_1 migration: %+v", byID["snap_1"])
	}
	if byID["snap_2"].RelPath != "snapshots/main/200-pre-restore.sqlite" || byID["snap_2"].Kind != SnapshotKindPreRestore {
		t.Fatalf("unexpected snap_2 migration: %+v", byID["snap_2"])
	}
	if byID["snap_3"].RelPath != "snapshots/main/300-branch-dev.sqlite" || byID["snap_3"].Kind != SnapshotKindBranch {
		t.Fatalf("unexpected snap_3 migration: %+v", byID["snap_3"])
	}
	expectedResolved := filepath.Join(dataDir, "snapshots", "main", "100-manual.sqlite")
	if byID["snap_1"].Path != expectedResolved {
		t.Fatalf("expected resolved Path %q, got %q", expectedResolved, byID["snap_1"].Path)
	}
}
