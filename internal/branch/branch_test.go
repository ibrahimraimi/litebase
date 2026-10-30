package branch

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/migrate"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
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

func TestThreeWayMergeAndBranchNamespaces(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	if _, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase main: %v", err)
	}
	if _, err := mgr.CreateDatabase(ctx, "analytics", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase analytics: %v", err)
	}

	execOnDB(t, ctx, mgr, "main", "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL)")
	execOnDB(t, ctx, mgr, "main", "INSERT INTO users (name) VALUES ('Ada')")

	snapSvc := snapshot.NewService(mgr)
	branchSvc := NewService(mgr, snapSvc)

	_, mainDevDB, err := branchSvc.Create(ctx, "main", "dev")
	if err != nil {
		t.Fatalf("create main/dev branch: %v", err)
	}
	_, analyticsDevDB, err := branchSvc.Create(ctx, "analytics", "dev")
	if err != nil {
		t.Fatalf("expected second parent 'analytics' to also be able to create branch 'dev', got: %v", err)
	}
	if mainDevDB.Name == analyticsDevDB.Name {
		t.Fatalf("expected distinct internal database names for branches, got %q", mainDevDB.Name)
	}

	execOnDB(t, ctx, mgr, mainDevDB.Name, "ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'")
	execOnDB(t, ctx, mgr, "main", "INSERT INTO users (name) VALUES ('Grace')")
	execOnDB(t, ctx, mgr, "main", "CREATE TABLE parent_only_logs (id INTEGER PRIMARY KEY, msg TEXT)")

	merged, err := branchSvc.MergeSchema(ctx, "main", "dev")
	if err != nil {
		t.Fatalf("MergeSchema failed: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("expected 1 ALTER TABLE statement, got: %v", merged)
	}
	if countRowsOnDB(t, ctx, mgr, "main", "users") != 2 {
		t.Fatalf("expected 2 rows in main.users after merge (no data loss!), got %d", countRowsOnDB(t, ctx, mgr, "main", "users"))
	}
	if countRowsOnDB(t, ctx, mgr, "main", "parent_only_logs") != 0 {
		t.Fatal("expected parent_only_logs table to survive three-way merge")
	}

	assertMergeConflictAbortsCleanly(t, ctx, mgr, branchSvc)
	assertDropTableRequiresAllowDrop(t, ctx, mgr, branchSvc)
	assertBranchCreateMatchesParentSnapshotUnderConcurrentWrites(t, ctx, mgr, branchSvc)
}

func assertMergeConflictAbortsCleanly(
	t *testing.T,
	ctx context.Context,
	mgr *manager.Manager,
	branchSvc *Service,
) {
	t.Helper()
	_, conflictDB, err := branchSvc.Create(ctx, "main", "conflict-br")
	if err != nil {
		t.Fatalf("Create conflict-br: %v", err)
	}
	execOnDB(t, ctx, mgr, "main", "ALTER TABLE users ADD COLUMN parent_col TEXT")
	execOnDB(t, ctx, mgr, conflictDB.Name, "ALTER TABLE users ADD COLUMN branch_col TEXT")

	_, err = branchSvc.MergeSchema(ctx, "main", "conflict-br")
	if !errors.Is(err, migrate.ErrMergeConflict) {
		t.Fatalf("expected ErrMergeConflict when both parent and branch modify users, got: %v", err)
	}
}

func assertDropTableRequiresAllowDrop(
	t *testing.T,
	ctx context.Context,
	mgr *manager.Manager,
	branchSvc *Service,
) {
	t.Helper()
	_, dropDB, err := branchSvc.Create(ctx, "main", "drop-br")
	if err != nil {
		t.Fatalf("Create drop-br: %v", err)
	}
	execOnDB(t, ctx, mgr, dropDB.Name, "DROP TABLE parent_only_logs")

	_, err = branchSvc.MergeSchema(ctx, "main", "drop-br")
	if !errors.Is(err, migrate.ErrDropTableNotAllowed) {
		t.Fatalf("expected ErrDropTableNotAllowed without AllowDrop, got: %v", err)
	}

	merged, err := branchSvc.MergeSchemaWithOptions(ctx, "main", "drop-br", migrate.DiffOptions{AllowDrop: true})
	if err != nil || len(merged) != 1 {
		t.Fatalf("expected MergeSchemaWithOptions(AllowDrop: true) to succeed, got %v, err=%v", merged, err)
	}
}

func assertBranchCreateMatchesParentSnapshotUnderConcurrentWrites(
	t *testing.T,
	ctx context.Context,
	mgr *manager.Manager,
	branchSvc *Service,
) {
	t.Helper()
	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopCh:
				return
			default:
				entry, err := mgr.Acquire(ctx, "main")
				if err == nil {
					_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO users (name) VALUES ('Concurrent')"})
					mgr.Release(entry)
				}
			}
		}
	}()

	branchRec, branchDB, err := branchSvc.Create(ctx, "main", "concurrent-br")
	close(stopCh)
	wg.Wait()
	if err != nil {
		t.Fatalf("Create concurrent-br: %v", err)
	}

	parentRec, err := mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, "main")
	if err != nil {
		t.Fatalf("GetDatabaseByName main: %v", err)
	}
	snapRec, err := mgr.Store().GetSnapshotRecord(ctx, parentRec.ID, branchRec.ParentSnapshotID)
	if err != nil {
		t.Fatalf("GetSnapshotRecord: %v", err)
	}
	snapDB, err := engine.OpenDatabase(ctx, engine.NewSQLiteEngine(), snapRec.Path, engine.Options{})
	if err != nil {
		t.Fatalf("OpenDatabase snapshot: %v", err)
	}
	defer snapDB.Close(ctx)

	snapOutcome, err := snapDB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT COUNT(*) FROM users"})
	if err != nil {
		t.Fatalf("count snapshot users: %v", err)
	}
	snapCount, _ := snapOutcome.Rows[0][0].ToDriverValue()
	branchCount := countRowsOnDB(t, ctx, mgr, branchDB.Name, "users")
	if snapCount.(int64) != branchCount {
		t.Fatalf("expected branch row count (%d) to equal parent snapshot row count (%d)", branchCount, snapCount.(int64))
	}
}
