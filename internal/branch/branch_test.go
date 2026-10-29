package branch

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/manager"
	"github.com/litebase/litebase/internal/snapshot"
)

func TestSnapshotsRestoreAndBranchIsolationDiffMerge(t *testing.T) {
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

	if _, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}

	execOnDB(t, ctx, mgr, "main", "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
	execOnDB(t, ctx, mgr, "main", "INSERT INTO users (name) VALUES ('Ada')")

	snapSvc := snapshot.NewService(mgr)
	snap1, err := snapSvc.Create(ctx, "main", "before-second-insert")
	if err != nil {
		t.Fatalf("snapshot Create: %v", err)
	}

	execOnDB(t, ctx, mgr, "main", "INSERT INTO users (name) VALUES ('Grace')")
	if countRowsOnDB(t, ctx, mgr, "main", "users") != 2 {
		t.Fatal("expected 2 rows in main before restore")
	}

	if _, err := snapSvc.Restore(ctx, "main", snap1.ID); err != nil {
		t.Fatalf("snapshot Restore: %v", err)
	}
	if countRowsOnDB(t, ctx, mgr, "main", "users") != 1 {
		t.Fatal("expected 1 row in main after restoring snap1")
	}

	branchSvc := NewService(mgr, snapSvc)
	_, _, err = branchSvc.Create(ctx, "main", "feature-audit")
	if err != nil {
		t.Fatalf("branch Create: %v", err)
	}

	execOnDB(t, ctx, mgr, "feature-audit", "INSERT INTO users (name) VALUES ('BranchOnlyUser')")
	execOnDB(t, ctx, mgr, "feature-audit", "CREATE TABLE audit_events (id INTEGER PRIMARY KEY, action TEXT NOT NULL)")

	if countRowsOnDB(t, ctx, mgr, "main", "users") != 1 {
		t.Fatal("branch write leaked into parent main database")
	}

	execOnDB(t, ctx, mgr, "main", "INSERT INTO users (name) VALUES ('MainOnlyUser')")
	if countRowsOnDB(t, ctx, mgr, "feature-audit", "users") != 2 {
		t.Fatal("parent write leaked into branch database")
	}

	diffSQL, err := branchSvc.DiffSchema(ctx, "main", "feature-audit")
	if err != nil {
		t.Fatalf("DiffSchema: %v", err)
	}
	if len(diffSQL) != 1 {
		t.Fatalf("expected 1 schema diff statement, got %v", diffSQL)
	}

	mergedSQL, err := branchSvc.MergeSchema(ctx, "main", "feature-audit")
	if err != nil || len(mergedSQL) != 1 {
		t.Fatalf("MergeSchema failed: %v, err %v", mergedSQL, err)
	}

	execOnDB(t, ctx, mgr, "main", "INSERT INTO audit_events (action) VALUES ('login')")
	if countRowsOnDB(t, ctx, mgr, "main", "audit_events") != 1 {
		t.Fatal("expected merged table audit_events to be writable on main")
	}
}

func execOnDB(t *testing.T, ctx context.Context, mgr *manager.Manager, dbName string, sqlText string) {
	t.Helper()
	entry, err := mgr.Acquire(ctx, dbName)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", dbName, err)
	}
	defer mgr.Release(entry)
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: sqlText}); err != nil {
		t.Fatalf("Execute on %q (%s): %v", dbName, sqlText, err)
	}
}

func countRowsOnDB(t *testing.T, ctx context.Context, mgr *manager.Manager, dbName string, tableName string) int64 {
	t.Helper()
	entry, err := mgr.Acquire(ctx, dbName)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", dbName, err)
	}
	defer mgr.Release(entry)
	outcome, err := entry.DB.ExecuteReadOnly(ctx, engine.Statement{
		SQL: "SELECT COUNT(*) FROM " + tableName,
	})
	if err != nil {
		t.Fatalf("count rows on %s.%s: %v", dbName, tableName, err)
	}
	val, _ := outcome.Rows[0][0].ToDriverValue()
	return val.(int64)
}
