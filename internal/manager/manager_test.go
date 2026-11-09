package manager

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/testhook"
)

func TestManagerLifecycleAndIdleEviction(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := NewManager(Config{
		DataDir: dataDir,
		Store:   store,
		Engine:  engine.NewSQLiteEngine(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	rec, err := mgr.CreateDatabase(ctx, "orders", engine.DurabilityNormal)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if rec.Name != "orders" || rec.SizeBytes <= 0 {
		t.Fatalf("unexpected database record: %+v", rec)
	}

	entry, err := mgr.Acquire(ctx, "orders")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{
		SQL: "CREATE TABLE orders (id INTEGER PRIMARY KEY, total INTEGER)",
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	mgr.Release(entry)

	time.Sleep(15 * time.Millisecond)
	evicted := mgr.EvictIdle(ctx, 5*time.Millisecond)
	if evicted != 1 {
		t.Fatalf("expected 1 idle database to be evicted, got %d", evicted)
	}

	reopened, err := mgr.Acquire(ctx, "orders")
	if err != nil {
		t.Fatalf("Acquire after eviction: %v", err)
	}
	outcome, err := reopened.DB.ExecuteReadOnly(ctx, engine.Statement{
		SQL: "SELECT COUNT(*) FROM orders",
	})
	mgr.Release(reopened)
	if err != nil || len(outcome.Rows) != 1 {
		t.Fatalf("query after reopen failed: %v", err)
	}
}

func TestSafeRestoreDrainRollbackAndConcurrency(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := NewManager(Config{
		DataDir: dataDir,
		Store:   store,
		Engine:  engine.NewSQLiteEngine(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	if _, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase main: %v", err)
	}
	if _, err := mgr.CreateDatabase(ctx, "other", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase other: %v", err)
	}

	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire main: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE t (id INT)"}); err != nil {
		t.Fatalf("create table t: %v", err)
	}
	snapPath := filepath.Join(dataDir, "snap.sqlite")
	if err := entry.DB.VacuumInto(ctx, snapPath); err != nil {
		t.Fatalf("VacuumInto: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO t VALUES (1)"}); err != nil {
		t.Fatalf("insert into t: %v", err)
	}
	mgr.Release(entry)

	mgr.renameFile = func(_, _ string) error {
		return errors.New("injected rename failure")
	}
	if err := mgr.ReplaceDatabaseFile(ctx, "main", snapPath); err == nil {
		t.Fatal("expected ReplaceDatabaseFile to fail on injected rename failure")
	}
	mgr.renameFile = os.Rename

	afterFailEntry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire main after failed restore: %v", err)
	}
	out, err := afterFailEntry.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT COUNT(*) FROM t"})
	if err != nil {
		t.Fatalf("count rows after rollback: %v", err)
	}
	val, _ := out.Rows[0][0].ToDriverValue()
	if val.(int64) != 1 {
		t.Fatalf("expected original database with 1 row to survive failed rename, got %d", val.(int64))
	}

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- mgr.ReplaceDatabaseFile(ctx, "main", snapPath)
	}()
	time.Sleep(15 * time.Millisecond)

	otherEntry, err := mgr.Acquire(ctx, "other")
	if err != nil {
		t.Fatalf("expected unrelated database 'other' to stay responsive while 'main' drains for restore: %v", err)
	}
	mgr.Release(otherEntry)

	if _, err := mgr.Acquire(ctx, "main"); !errors.Is(err, ErrDatabaseRestoring) {
		t.Fatalf("expected Acquire('main') during restore to return ErrDatabaseRestoring, got %v", err)
	}
	mgr.Release(afterFailEntry)

	if err := <-restoreDone; err != nil {
		t.Fatalf("ReplaceDatabaseFile after drain failed: %v", err)
	}
	restoredEntry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire main after successful restore: %v", err)
	}
	defer mgr.Release(restoredEntry)
	out2, err := restoredEntry.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT COUNT(*) FROM t"})
	if err != nil {
		t.Fatalf("count rows after restore: %v", err)
	}
	val2, _ := out2.Rows[0][0].ToDriverValue()
	if val2.(int64) != 0 {
		t.Fatalf("expected 0 rows after restoring snap.sqlite, got %d", val2.(int64))
	}
}

const (
	restoreCrashHelperEnv = "LITEBASE_RESTORE_CRASH_HELPER"
	restoreDataDirEnv     = "LITEBASE_RESTORE_DATA_DIR"
	restoreSnapPathEnv    = "LITEBASE_RESTORE_SNAP_PATH"
)

func TestRestoreCrashSubprocessHelper(t *testing.T) {
	if os.Getenv(restoreCrashHelperEnv) != "1" {
		return
	}
	ctx := context.Background()
	dataDir := os.Getenv(restoreDataDirEnv)
	snapPath := os.Getenv(restoreSnapPathEnv)

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		os.Exit(2)
	}
	defer store.Close()

	mgr, err := NewManager(Config{DataDir: dataDir, Store: store})
	if err != nil {
		os.Exit(3)
	}
	_ = mgr.ReplaceDatabaseFile(ctx, "main", snapPath)
	os.Exit(0)
}

func TestRestoreCrashRecoveryOnStartup(t *testing.T) {
	failpoints := []string{
		"restore.during-staging",
		"restore.after-rename",
		"restore.after-wal-remove",
	}
	for _, fp := range failpoints {
		t.Run(fp, func(t *testing.T) {
			runRestoreCrashRecoveryCase(t, fp)
		})
	}
}

func runRestoreCrashRecoveryCase(t *testing.T, failpoint string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	snapPath := seedDatabaseAndEmptySnapshot(t, ctx, dataDir)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve executable: %v", err)
	}
	cmd := exec.Command(testBinary, "-test.run=^TestRestoreCrashSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		restoreCrashHelperEnv+"=1",
		restoreDataDirEnv+"="+dataDir,
		restoreSnapPathEnv+"="+snapPath,
		testhook.FailpointEnvVar+"="+failpoint,
	)
	_ = cmd.Run()

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()

	mgr, err := NewManager(Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager after crash (%s): %v", failpoint, err)
	}
	defer mgr.CloseAll(ctx)

	rec, err := store.GetDatabaseByName(ctx, control.DefaultProjectID, "main")
	if err != nil {
		t.Fatalf("GetDatabaseByName: %v", err)
	}
	if _, statErr := os.Stat(rec.Path + ".restore-staging"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected .restore-staging to be removed on startup recovery after %s", failpoint)
	}
	if failpoint != "restore.during-staging" {
		auditCount, err := store.CountAuditEvents(ctx, "database.restore.recovered", "main")
		if err != nil || auditCount != 1 {
			t.Fatalf("expected 1 database.restore.recovered audit event after %s, got %d (err=%v)", failpoint, auditCount, err)
		}
	}

	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire main after recovery (%s): %v", failpoint, err)
	}
	defer mgr.Release(entry)

	if err := entry.DB.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck after recovery (%s): %v", failpoint, err)
	}
	out, err := entry.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT COUNT(*) FROM items"})
	if err != nil {
		t.Fatalf("query items after recovery (%s): %v", failpoint, err)
	}
	val, _ := out.Rows[0][0].ToDriverValue()
	if val.(int64) != 2 {
		t.Fatalf("expected original 2 rows in items after interrupted restore (%s), got %d", failpoint, val.(int64))
	}
}

func seedDatabaseAndEmptySnapshot(t *testing.T, ctx context.Context, dataDir string) string {
	t.Helper()
	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := NewManager(Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	if _, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityFull); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)"}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	snapPath := filepath.Join(dataDir, "empty-snap.sqlite")
	if err := entry.DB.VacuumInto(ctx, snapPath); err != nil {
		t.Fatalf("VacuumInto: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO items (name) VALUES ('one'), ('two')"}); err != nil {
		t.Fatalf("insert items: %v", err)
	}
	mgr.Release(entry)
	return snapPath
}

func TestStartupPreservesCompletedPreRestoreSafetyCopy(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	snapPath := seedDatabaseAndEmptySnapshot(t, ctx, dataDir)

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := NewManager(Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if err := mgr.ReplaceDatabaseFile(ctx, "main", snapPath); err != nil {
		t.Fatalf("ReplaceDatabaseFile: %v", err)
	}
	_ = mgr.CloseAll(ctx)

	rec, err := store.GetDatabaseByName(ctx, control.DefaultProjectID, "main")
	if err != nil {
		t.Fatalf("GetDatabaseByName: %v", err)
	}
	mgrRestarted, err := NewManager(Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager restart: %v", err)
	}
	defer mgrRestarted.CloseAll(ctx)

	if _, err := os.Stat(rec.Path + ".pre-restore"); err != nil {
		t.Fatalf("expected .pre-restore safety copy to be preserved when main db exists: %v", err)
	}
}
