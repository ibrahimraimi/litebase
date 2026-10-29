package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/manager"
)

func TestMigrationsOrderIdempotencyChecksumAndRollback(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{
		DataDir: dataDir,
		Store:   store,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	if _, err := mgr.CreateDatabase(ctx, "app", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	runner := NewRunner(mgr)
	initialMigrations := []MigrationInput{
		{Version: "0002", Name: "0002_posts.sql", SQL: "CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER, title TEXT);"},
		{Version: "0001", Name: "0001_users.sql", SQL: "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE);"},
	}

	applied, err := runner.ApplyPending(ctx, "app", "test-actor", initialMigrations)
	if err != nil {
		t.Fatalf("ApplyPending failed: %v", err)
	}
	if len(applied) != 2 || applied[0] != "0001" || applied[1] != "0002" {
		t.Fatalf("expected [0001 0002], got %v", applied)
	}

	reRunApplied, err := runner.ApplyPending(ctx, "app", "test-actor", initialMigrations)
	if err != nil || len(reRunApplied) != 0 {
		t.Fatalf("expected idempotent re-run to apply 0 migrations, got %v (err %v)", reRunApplied, err)
	}

	tamperedMigrations := []MigrationInput{
		{Version: "0001", Name: "0001_users.sql", SQL: "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT, role TEXT);"},
		{Version: "0002", Name: "0002_posts.sql", SQL: "CREATE TABLE posts (id INTEGER PRIMARY KEY, user_id INTEGER, title TEXT);"},
	}
	if _, err := runner.ApplyPending(ctx, "app", "test-actor", tamperedMigrations); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch for tampered migration, got: %v", err)
	}

	failingMigrations := append(initialMigrations,
		MigrationInput{Version: "0003", Name: "0003_comments.sql", SQL: "CREATE TABLE comments (id INTEGER PRIMARY KEY, body TEXT);"},
		MigrationInput{Version: "0004", Name: "0004_broken.sql", SQL: "INVALID SQL SYNTAX HERE;"},
	)
	if _, err := runner.ApplyPending(ctx, "app", "test-actor", failingMigrations); err == nil {
		t.Fatal("expected broken migration 0004 to fail")
	}

	entry, err := mgr.Acquire(ctx, "app")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer mgr.Release(entry)

	schemaObjects, err := InspectSchema(ctx, entry.DB)
	if err != nil {
		t.Fatalf("InspectSchema: %v", err)
	}
	for _, obj := range schemaObjects {
		if obj.Name == "comments" {
			t.Fatal("expected table 'comments' from 0003 to be rolled back when 0004 failed in the same deploy")
		}
	}
}
