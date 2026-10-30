package control

import (
	"context"
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
