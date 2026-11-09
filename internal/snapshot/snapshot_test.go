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

func TestMovedDataDirRestore(t *testing.T) {
	ctx := context.Background()
	parentDir := t.TempDir()
	dirA := filepath.Join(parentDir, "loc-a")
	dirB := filepath.Join(parentDir, "loc-b")

	seedDatabaseForSnapshotCrashTest(t, ctx, dirA)

	storeA, err := control.OpenStore(ctx, filepath.Join(dirA, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore A: %v", err)
	}
	mgrA, err := manager.NewManager(manager.Config{DataDir: dirA, Store: storeA})
	if err != nil {
		t.Fatalf("NewManager A: %v", err)
	}
	svcA := NewService(mgrA)
	snap, err := svcA.Create(ctx, "main", "before-move")
	if err != nil {
		t.Fatalf("Create snapshot: %v", err)
	}
	entryA, err := mgrA.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	if _, err := entryA.DB.Execute(ctx, engine.Statement{SQL: "DELETE FROM items"}); err != nil {
		t.Fatalf("DELETE FROM items: %v", err)
	}
	mgrA.Release(entryA)
	_ = mgrA.CloseAll(ctx)
	_ = storeA.Close()

	if err := os.Rename(dirA, dirB); err != nil {
		t.Fatalf("Rename dirA -> dirB: %v", err)
	}

	storeB, err := control.OpenStore(ctx, filepath.Join(dirB, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore B: %v", err)
	}
	defer storeB.Close()

	mgrB, err := manager.NewManager(manager.Config{DataDir: dirB, Store: storeB})
	if err != nil {
		t.Fatalf("NewManager B: %v", err)
	}
	defer mgrB.CloseAll(ctx)

	svcB := NewService(mgrB)
	if _, err := svcB.Restore(ctx, "main", snap.ID); err != nil {
		t.Fatalf("Restore after moving dataDir failed: %v", err)
	}
}

func TestSnapshotChecksumTamperRefusesRestoreBeforeTouchingLiveDB(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDatabaseForSnapshotCrashTest(t, ctx, dataDir)

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

	svc := NewService(mgr)
	snap, err := svc.Create(ctx, "main", "tamper-test")
	if err != nil {
		t.Fatalf("Create snapshot: %v", err)
	}

	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := entry.DB.Execute(ctx, engine.Statement{SQL: "UPDATE items SET v = 'live-untouched'"}); err != nil {
		t.Fatalf("update live db: %v", err)
	}
	mgr.Release(entry)

	rawBytes, err := os.ReadFile(snap.Path)
	if err != nil {
		t.Fatalf("ReadFile snapshot: %v", err)
	}
	mutated := strings.Replace(string(rawBytes), "keep-me", "Xeep-me", 1)
	if mutated == string(rawBytes) {
		t.Fatal("expected to find 'keep-me' in snapshot bytes")
	}
	if err := os.WriteFile(snap.Path, []byte(mutated), 0o600); err != nil {
		t.Fatalf("WriteFile tampered snapshot: %v", err)
	}

	if _, err := svc.Restore(ctx, "main", snap.ID); err == nil {
		t.Fatal("expected Restore to fail when snapshot checksum is tampered")
	}

	entryAfter, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire after failed restore: %v", err)
	}
	defer mgr.Release(entryAfter)

	res, err := entryAfter.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT v FROM items"})
	if err != nil {
		t.Fatalf("query live db: %v", err)
	}
	gotVal, _ := res.Rows[0][0].ToDriverValue()
	if gotVal.(string) != "live-untouched" {
		t.Fatalf("expected live database to remain 'live-untouched', got %q", gotVal)
	}
}

func TestSnapshotDeleteProtectionAndRetentionMatrix(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	cases := []struct {
		name           string
		keepLast       int
		keepDays       int
		dryRun         bool
		snaps          []control.SnapshotRecord
		branchRefIDs   map[string]struct{}
		expectedPruned []string
	}{
		{
			name:     "fewer snapshots than keep_last even if older than keep_days",
			keepLast: 3,
			keepDays: 7,
			snaps: []control.SnapshotRecord{
				{ID: "s1", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-20 * day)},
				{ID: "s2", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-30 * day)},
			},
			expectedPruned: nil,
		},
		{
			name:     "all older than keep_days keeps newest keep_last and prunes older",
			keepLast: 2,
			keepDays: 7,
			snaps: []control.SnapshotRecord{
				{ID: "s1", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-10 * day)},
				{ID: "s2", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-11 * day)},
				{ID: "s3", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-12 * day)},
				{ID: "s4", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-13 * day)},
			},
			expectedPruned: []string{"s3", "s4"},
		},
		{
			name:     "pinned and branch-referenced snapshots are protected",
			keepLast: 1,
			keepDays: 7,
			branchRefIDs: map[string]struct{}{
				"s3": {},
			},
			snaps: []control.SnapshotRecord{
				{ID: "s1", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-1 * day)},
				{ID: "s2", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-20 * day), Pinned: true},
				{ID: "s3", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-25 * day)},
				{ID: "s4", Kind: control.SnapshotKindScheduled, CreatedAt: now.Add(-30 * day)},
			},
			expectedPruned: []string{"s4"},
		},
		{
			name:     "mixed kinds: manual and branch untouched; pre-restore capped at 5 and 7d",
			keepLast: 1,
			keepDays: 7,
			snaps: []control.SnapshotRecord{
				{ID: "m1", Kind: control.SnapshotKindManual, CreatedAt: now.Add(-90 * day)},
				{ID: "b1", Kind: control.SnapshotKindBranch, CreatedAt: now.Add(-90 * day)},
				{ID: "pr1", Kind: control.SnapshotKindPreRestore, CreatedAt: now.Add(-1 * day)},
				{ID: "pr2", Kind: control.SnapshotKindPreRestore, CreatedAt: now.Add(-2 * day)},
				{ID: "pr3", Kind: control.SnapshotKindPreRestore, CreatedAt: now.Add(-8 * day)},
				{ID: "pr4", Kind: control.SnapshotKindPreRestore, CreatedAt: now.Add(-9 * day)},
				{ID: "pr5", Kind: control.SnapshotKindPreRestore, CreatedAt: now.Add(-10 * day)},
				{ID: "pr6", Kind: control.SnapshotKindPreRestore, CreatedAt: now.Add(-11 * day)},
			},
			expectedPruned: []string{"pr6"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidates := selectPruneCandidates(tc.snaps, tc.branchRefIDs, tc.keepLast, tc.keepDays, now)
			got := make([]string, 0, len(candidates))
			for _, c := range candidates {
				got = append(got, c.ID)
			}
			if strings.Join(got, ",") != strings.Join(tc.expectedPruned, ",") {
				t.Fatalf("expected pruned=%v, got=%v", tc.expectedPruned, got)
			}
		})
	}
}

func TestSnapshotDeleteRefusesPinnedAndBranchParentAndRestoreAsNewDB(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDatabaseForSnapshotCrashTest(t, ctx, dataDir)

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

	svc := NewService(mgr)
	pinnedSnap, err := svc.CreateWithOptions(ctx, "main", CreateOptions{
		Label:  "pinned-one",
		Pinned: true,
	})
	if err != nil {
		t.Fatalf("CreateWithOptions pinned: %v", err)
	}
	if err := svc.Delete(ctx, "main", pinnedSnap.ID); err == nil {
		t.Fatal("expected Delete on pinned snapshot to fail")
	}

	branchSnap, err := svc.Create(ctx, "main", "branch-feat")
	if err != nil {
		t.Fatalf("Create branch snap: %v", err)
	}
	branchDB, err := mgr.CreateDatabase(ctx, "main--feat", engine.DurabilityNormal)
	if err != nil {
		t.Fatalf("CreateDatabase branch: %v", err)
	}
	if _, err := store.CreateBranchRecord(ctx, control.CreateBranchParams{
		BranchDatabaseID: branchDB.ID,
		ParentDatabaseID: branchSnap.DatabaseID,
		ParentSnapshotID: branchSnap.ID,
		Name:             "feat",
	}); err != nil {
		t.Fatalf("CreateBranchRecord: %v", err)
	}
	if err := svc.Delete(ctx, "main", branchSnap.ID); err == nil {
		t.Fatal("expected Delete on branch-referenced snapshot to fail")
	}

	dryRes, err := svc.Prune(ctx, "main", PruneOptions{DryRun: true, KeepLast: 1, KeepDays: 1})
	if err != nil || !dryRes.DryRun {
		t.Fatalf("Prune dry-run failed: %+v, err=%v", dryRes, err)
	}

	if _, err := svc.RestoreWithOptions(ctx, "main", RestoreOptions{
		SnapshotID: pinnedSnap.ID,
		AsNewDB:    "investigate",
	}); err != nil {
		t.Fatalf("RestoreWithOptions --as investigate: %v", err)
	}
	invEntry, err := mgr.Acquire(ctx, "investigate")
	if err != nil {
		t.Fatalf("Acquire investigate: %v", err)
	}
	defer mgr.Release(invEntry)
	out, err := invEntry.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT v FROM items"})
	if err != nil || len(out.Rows) != 1 {
		t.Fatalf("expected 1 row in restored-as database 'investigate', got %+v, err=%v", out, err)
	}
}
