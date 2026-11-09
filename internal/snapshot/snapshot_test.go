package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/testhook"
)

const (
	snapshotCrashHelperEnv = "LITEBASE_SNAPSHOT_CRASH_HELPER"
	snapshotDataDirEnv     = "LITEBASE_SNAPSHOT_DATA_DIR"
)

func TestSnapshotCrashSubprocessHelper(t *testing.T) {
	if os.Getenv(snapshotCrashHelperEnv) != "1" {
		return
	}
	ctx := context.Background()
	dataDir := os.Getenv(snapshotDataDirEnv)

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		os.Exit(2)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{DataDir: dataDir, Store: store})
	if err != nil {
		os.Exit(3)
	}
	svc := NewService(mgr)
	_, _ = svc.Create(ctx, "main", "interrupted")
	os.Exit(0)
}

func TestSnapshotCrashRecoveryLeavesNoUnrecordedFilesOrTmp(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDatabaseForSnapshotCrashTest(t, ctx, dataDir)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve executable: %v", err)
	}
	cmd := exec.Command(testBinary, "-test.run=^TestSnapshotCrashSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		snapshotCrashHelperEnv+"=1",
		snapshotDataDirEnv+"="+dataDir,
		testhook.FailpointEnvVar+"=snapshot.during-vacuum",
	)
	_ = cmd.Run()

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager after crash: %v", err)
	}
	defer mgr.CloseAll(ctx)

	svc := NewService(mgr)
	records, err := svc.List(ctx, "main")
	if err != nil {
		t.Fatalf("List snapshots: %v", err)
	}
	recordedPaths := make(map[string]struct{}, len(records))
	for _, r := range records {
		recordedPaths[r.Path] = struct{}{}
	}

	snapDir := filepath.Join(dataDir, "snapshots", "main")
	entries, err := os.ReadDir(snapDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("ReadDir(%s): %v", snapDir, err)
	}
	for _, entry := range entries {
		fullPath := filepath.Join(snapDir, entry.Name())
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("expected startup recovery to delete .tmp file %s", fullPath)
		}
		if _, ok := recordedPaths[fullPath]; !ok {
			t.Fatalf("found unrecorded final snapshot file %s after SIGKILL during snapshot", fullPath)
		}
	}

	dbEntry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire main: %v", err)
	}
	defer mgr.Release(dbEntry)
	if err := dbEntry.DB.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck on main after snapshot crash: %v", err)
	}
}

func seedDatabaseForSnapshotCrashTest(t *testing.T, ctx context.Context, dataDir string) {
	t.Helper()
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

	if _, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityFull); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, v TEXT)"}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO items (v) VALUES ('keep-me')"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	mgr.Release(entry)
}

func TestSnapshotStartupOrphanCleanupAndCorruptTmpRejection(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDatabaseForSnapshotCrashTest(t, ctx, dataDir)

	snapDir := filepath.Join(dataDir, "snapshots", "main")
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	staleTmp := filepath.Join(snapDir, "stale.sqlite.tmp")
	staleOrphan := filepath.Join(snapDir, "orphan.sqlite")
	for _, p := range []string{staleTmp, staleOrphan} {
		if err := os.WriteFile(p, []byte("orphan"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
		twoHoursAgo := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(p, twoHoursAgo, twoHoursAgo); err != nil {
			t.Fatalf("Chtimes(%s): %v", p, err)
		}
	}

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgrDefault, err := manager.NewManager(manager.Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager default: %v", err)
	}
	_ = mgrDefault.CloseAll(ctx)

	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("expected stale .tmp file to be deleted on startup, got err=%v", err)
	}
	if _, err := os.Stat(staleOrphan); err != nil {
		t.Fatalf("expected orphan snapshot to be kept by default (logged only), got err=%v", err)
	}

	mgrDeleteOrphans, err := manager.NewManager(manager.Config{
		DataDir:               dataDir,
		Store:                 store,
		DeleteOrphanSnapshots: true,
	})
	if err != nil {
		t.Fatalf("NewManager DeleteOrphanSnapshots: %v", err)
	}
	defer mgrDeleteOrphans.CloseAll(ctx)

	if _, err := os.Stat(staleOrphan); !os.IsNotExist(err) {
		t.Fatalf("expected orphan snapshot to be deleted when DeleteOrphanSnapshots=true, got err=%v", err)
	}
}
