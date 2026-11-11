package backup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
	"github.com/ibrahimraimi/litebase/internal/testhook"
)

const (
	uploadCrashHelperEnv = "LITEBASE_UPLOAD_CRASH_HELPER"
	uploadDataDirEnv     = "LITEBASE_UPLOAD_DATA_DIR"
	uploadRemoteDirEnv   = "LITEBASE_UPLOAD_REMOTE_DIR"
)

func TestBackupUploadCrashSubprocessHelper(t *testing.T) {
	if os.Getenv(uploadCrashHelperEnv) != "1" {
		return
	}
	ctx := context.Background()
	dataDir := os.Getenv(uploadDataDirEnv)
	remoteDir := os.Getenv(uploadRemoteDirEnv)

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		os.Exit(2)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{DataDir: dataDir, Store: store})
	if err != nil {
		os.Exit(3)
	}
	snapSvc := snapshot.NewService(mgr)
	blobStore, err := NewLocalDirStore(remoteDir, "backups")
	if err != nil {
		os.Exit(4)
	}
	bm := NewManager(ManagerConfig{
		DBManager: mgr,
		Snapshots: snapSvc,
		BlobStore: blobStore,
	})
	sched := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
	})
	_, _ = sched.RunNow(ctx, "main")
	os.Exit(0)
}

func TestBackupKillDuringUploadLeavesPendingAndNextRunReuploads(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	remoteDir := t.TempDir()

	seedDatabaseWithPolicyForBackupTest(t, ctx, dataDir, true)

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve executable: %v", err)
	}
	cmd := exec.Command(testBinary, "-test.run=^TestBackupUploadCrashSubprocessHelper$")
	cmd.Env = append(os.Environ(),
		uploadCrashHelperEnv+"=1",
		uploadDataDirEnv+"="+dataDir,
		uploadRemoteDirEnv+"="+remoteDir,
		testhook.FailpointEnvVar+"=backup.after-data-upload",
	)
	_ = cmd.Run()

	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{DataDir: dataDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	snapSvc := snapshot.NewService(mgr)
	blobStore, err := NewLocalDirStore(remoteDir, "backups")
	if err != nil {
		t.Fatalf("NewLocalDirStore: %v", err)
	}
	bm := NewManager(ManagerConfig{
		DBManager: mgr,
		Snapshots: snapSvc,
		BlobStore: blobStore,
	})

	localSnaps, err := snapSvc.List(ctx, "main")
	if err != nil || len(localSnaps) != 1 {
		t.Fatalf("expected 1 local snapshot after interrupted upload, got %d (err=%v)", len(localSnaps), err)
	}
	if localSnaps[0].RemoteStatus != control.RemoteStatusPending {
		t.Fatalf("expected interrupted snapshot to have remote_status='pending', got %q", localSnaps[0].RemoteStatus)
	}

	remoteSnaps, err := bm.ListRemoteSnapshots(ctx, "main")
	if err != nil {
		t.Fatalf("ListRemoteSnapshots: %v", err)
	}
	if len(remoteSnaps) != 0 {
		t.Fatalf("expected 0 completed remote backups when meta.json is missing, got %d", len(remoteSnaps))
	}

	sched := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
	})
	if _, err := sched.RunNow(ctx, "main"); err != nil {
		t.Fatalf("RunNow after crash: %v", err)
	}

	afterSnaps, err := snapSvc.List(ctx, "main")
	if err != nil {
		t.Fatalf("List local snapshots: %v", err)
	}
	for _, sn := range afterSnaps {
		if sn.RemoteStatus != control.RemoteStatusUploaded {
			t.Fatalf("expected snapshot %s to be uploaded after next run, got %q", sn.ID, sn.RemoteStatus)
		}
	}
	completedRemote, err := bm.ListRemoteSnapshots(ctx, "main")
	if err != nil || len(completedRemote) < 1 {
		t.Fatalf("expected completed remote backups after re-upload, got %d (err=%v)", len(completedRemote), err)
	}
}

func TestBlobStoreContractSuite(t *testing.T) {
	ctx := context.Background()

	localStore, err := NewLocalDirStore(t.TempDir(), "test-prefix")
	if err != nil {
		t.Fatalf("NewLocalDirStore: %v", err)
	}
	t.Run("LocalDirStore", func(t *testing.T) {
		runBlobStoreContract(t, ctx, localStore)
	})

	s3Srv := newInMemoryS3Server(t)
	s3Store, err := NewS3Store(S3Config{
		Endpoint:        strings.TrimPrefix(s3Srv.URL, "http://"),
		Region:          "us-east-1",
		Bucket:          "litebase-test",
		Prefix:          "test-prefix",
		AccessKeyID:     "minioadmin",
		SecretAccessKey: "minioadmin",
		UseSSL:          false,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	t.Run("S3Store", func(t *testing.T) {
		runBlobStoreContract(t, ctx, s3Store)
	})
}

func runBlobStoreContract(t *testing.T, ctx context.Context, store BlobStore) {
	t.Helper()

	invalidKeys := []string{
		"",
		"../secret.sqlite",
		"main/../../etc/passwd",
		"/leading/slash",
		"trailing/slash/",
		`windows\backslash`,
		strings.Repeat("a", 600),
	}
	for _, bad := range invalidKeys {
		if err := store.Put(ctx, bad, strings.NewReader("x"), 1, nil); err == nil {
			t.Fatalf("expected Put to reject invalid key %q", bad)
		}
		if _, err := store.Get(ctx, bad); err == nil {
			t.Fatalf("expected Get to reject invalid key %q", bad)
		}
	}

	if _, err := store.Get(ctx, "main/2026/11/missing.sqlite"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected os.ErrNotExist on missing key, got %v", err)
	}

	payloadV1 := []byte("first-payload-bytes")
	key := "main/2026/11/100-snap.sqlite.gz"
	if err := store.Put(ctx, key, bytes.NewReader(payloadV1), int64(len(payloadV1)), map[string]string{"sha256": "abc"}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}

	rc, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	gotBytes, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(gotBytes, payloadV1) {
		t.Fatalf("Get v1 mismatch: got %q, want %q", gotBytes, payloadV1)
	}

	payloadV2 := []byte("overwritten-second-payload")
	if err := store.Put(ctx, key, bytes.NewReader(payloadV2), int64(len(payloadV2)), nil); err != nil {
		t.Fatalf("Put overwrite v2: %v", err)
	}
	rc2, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get v2: %v", err)
	}
	gotBytes2, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if !bytes.Equal(gotBytes2, payloadV2) {
		t.Fatalf("Get v2 mismatch: got %q, want %q", gotBytes2, payloadV2)
	}

	items, err := store.List(ctx, "main")
	if err != nil || len(items) != 1 || items[0].Key != key {
		t.Fatalf("List mismatch: got %+v, err=%v", items, err)
	}

	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	itemsAfter, err := store.List(ctx, "main")
	if err != nil || len(itemsAfter) != 0 {
		t.Fatalf("expected empty List after Delete, got %+v, err=%v", itemsAfter, err)
	}
}

func TestSchedulerFakeClockIdleSkipJitterBackoffAndRestart(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	remoteDir := t.TempDir()

	currentTime := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return currentTime }

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

	dbRec, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityNormal)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"})
	_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO t (v) VALUES ('row1')"})
	mgr.Release(entry)

	if _, err := store.UpsertBackupPolicy(ctx, control.UpsertBackupPolicyParams{
		DatabaseID:      dbRec.ID,
		IntervalSeconds: 3600,
		KeepLast:        5,
		KeepDays:        7,
		Remote:          true,
		Enabled:         true,
	}); err != nil {
		t.Fatalf("UpsertBackupPolicy: %v", err)
	}

	blobStore, err := NewLocalDirStore(remoteDir, "")
	if err != nil {
		t.Fatalf("NewLocalDirStore: %v", err)
	}
	snapSvc := snapshot.NewService(mgr)
	bm := NewManager(ManagerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BlobStore:     blobStore,
		RetryBaseWait: time.Millisecond,
	})
	sched := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
		NowFunc:       nowFn,
		JitterFunc:    func(_ string) float64 { return 0.05 }, // +180s jitter on 3600s
	})

	sched.Tick(ctx)
	snaps1, _ := snapSvc.List(ctx, "main")
	if len(snaps1) != 1 {
		t.Fatalf("expected 1 snapshot on first tick, got %d", len(snaps1))
	}

	// Advance 3600s (without reaching +180s jitter): should NOT be due yet.
	currentTime = currentTime.Add(3600 * time.Second)
	sched.Tick(ctx)
	snapsNotDue, _ := snapSvc.List(ctx, "main")
	if len(snapsNotDue) != 1 {
		t.Fatalf("expected 1 snapshot before jitter window elapses, got %d", len(snapsNotDue))
	}

	// Advance past jitter (+180s) with NO new commits: should skip-when-idle and still advance last_run_at!
	currentTime = currentTime.Add(180 * time.Second)
	sched.Tick(ctx)
	snapsIdle, _ := snapSvc.List(ctx, "main")
	if len(snapsIdle) != 1 {
		t.Fatalf("expected idle database to skip creating a duplicate snapshot, got %d", len(snapsIdle))
	}
	polAfterIdle, err := store.GetBackupPolicy(ctx, dbRec.ID)
	if err != nil || polAfterIdle.LastRunAt == nil || !polAfterIdle.LastRunAt.Equal(currentTime) {
		t.Fatalf("expected last_run_at to advance on idle skip to %v, got %+v", currentTime, polAfterIdle.LastRunAt)
	}

	// Simulate restart: new Scheduler instance reads persisted last_run_at and does not immediately re-run.
	schedRestarted := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
		NowFunc:       nowFn,
		JitterFunc:    func(_ string) float64 { return 0 },
	})
	currentTime = currentTime.Add(60 * time.Second)
	schedRestarted.Tick(ctx)
	snapsAfterRestart, _ := snapSvc.List(ctx, "main")
	if len(snapsAfterRestart) != 1 {
		t.Fatalf("expected restarted scheduler to respect persisted schedule, got %d snapshots", len(snapsAfterRestart))
	}
}

type faultBlobStore struct {
	inner            BlobStore
	mu               sync.Mutex
	failPutRemaining int
	failPrefix       string
	truncateGetBytes int
}

func (f *faultBlobStore) Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string) error {
	f.mu.Lock()
	if f.failPrefix != "" && strings.HasPrefix(key, f.failPrefix) {
		f.mu.Unlock()
		return errors.New("simulated permanent remote failure")
	}
	if f.failPutRemaining > 0 {
		f.failPutRemaining--
		f.mu.Unlock()
		return errors.New("simulated transient network error")
	}
	f.mu.Unlock()
	return f.inner.Put(ctx, key, r, size, meta)
}

func (f *faultBlobStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := f.inner.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	trunc := f.truncateGetBytes
	f.mu.Unlock()
	if trunc > 0 && !strings.HasSuffix(key, ".meta.json") {
		data, _ := io.ReadAll(io.LimitReader(rc, int64(trunc)))
		_ = rc.Close()
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return rc, nil
}

func (f *faultBlobStore) List(ctx context.Context, prefix string) ([]BlobInfo, error) {
	return f.inner.List(ctx, prefix)
}

func (f *faultBlobStore) Delete(ctx context.Context, key string) error {
	return f.inner.Delete(ctx, key)
}

func TestFaultInjectingBlobStoreRetryTruncatedStreamAndSchedulerIsolation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	remoteDir := t.TempDir()

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

	db1, _ := mgr.CreateDatabase(ctx, "db1", engine.DurabilityNormal)
	db2, _ := mgr.CreateDatabase(ctx, "db2", engine.DurabilityNormal)
	for _, name := range []string{"db1", "db2"} {
		e, _ := mgr.Acquire(ctx, name)
		_, _ = e.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)"})
		_, _ = e.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO items (name) VALUES ('ok')"})
		mgr.Release(e)
	}

	_, _ = store.UpsertBackupPolicy(ctx, control.UpsertBackupPolicyParams{
		DatabaseID: db1.ID, IntervalSeconds: 3600, Remote: true, Enabled: true,
	})
	_, _ = store.UpsertBackupPolicy(ctx, control.UpsertBackupPolicyParams{
		DatabaseID: db2.ID, IntervalSeconds: 3600, Remote: true, Enabled: true,
	})

	baseBlob, _ := NewLocalDirStore(remoteDir, "")
	faultStore := &faultBlobStore{
		inner:            baseBlob,
		failPutRemaining: 1, // First Put fails transiently, retry succeeds
		failPrefix:       "db1/",
	}
	snapSvc := snapshot.NewService(mgr)
	bm := NewManager(ManagerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BlobStore:     faultStore,
		Compression:   CompressionNone,
		RetryBaseWait: time.Millisecond,
	})
	sched := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
		JitterFunc:    func(_ string) float64 { return 0 },
	})

	// Tick: db1 fails permanently, db2 fails once transiently then succeeds!
	sched.Tick(ctx)

	st1, _ := sched.GetStatus(ctx, "db1")
	if st1.LastError == "" {
		t.Fatal("expected db1 backup status to record LastError")
	}
	st2, _ := sched.GetStatus(ctx, "db2")
	if st2.LastError != "" || st2.LastSuccessAt == nil {
		t.Fatalf("expected db2 to succeed despite db1 failure and transient error, got %+v", st2)
	}

	// Truncate stream on restore of db2: must fail checksum before touching live db2!
	faultStore.mu.Lock()
	faultStore.truncateGetBytes = 32
	faultStore.mu.Unlock()

	if _, err := bm.RestoreFromRemote(ctx, "db2", "latest", ""); err == nil {
		t.Fatal("expected RestoreFromRemote with truncated stream to fail")
	}
}

func TestRemoteUploadVerifyOrphanGCAndDisasterRecoveryDrill(t *testing.T) {
	ctx := context.Background()
	serverDir := t.TempDir()
	remoteDir := t.TempDir()

	blobStore, err := NewLocalDirStore(remoteDir, "cluster-1")
	if err != nil {
		t.Fatalf("NewLocalDirStore: %v", err)
	}

	store, err := control.OpenStore(ctx, filepath.Join(serverDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	_, adminToken, err := store.CreateToken(ctx, control.CreateTokenParams{
		Scopes: []auth.Scope{auth.ScopeAdmin},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	mgr, err := manager.NewManager(manager.Config{DataDir: serverDir, Store: store})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, dbName := range []string{"orders", "users"} {
		rec, err := mgr.CreateDatabase(ctx, dbName, engine.DurabilityNormal)
		if err != nil {
			t.Fatalf("CreateDatabase(%s): %v", dbName, err)
		}
		entry, _ := mgr.Acquire(ctx, dbName)
		_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE records (id INTEGER PRIMARY KEY, label TEXT NOT NULL)"})
		_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO records (label) VALUES ('" + dbName + "-row-1'), ('" + dbName + "-row-2')"})
		mgr.Release(entry)

		if _, err := store.UpsertBackupPolicy(ctx, control.UpsertBackupPolicyParams{
			DatabaseID:      rec.ID,
			IntervalSeconds: 600,
			KeepLast:        5,
			KeepDays:        7,
			Remote:          true,
			Enabled:         true,
		}); err != nil {
			t.Fatalf("UpsertBackupPolicy(%s): %v", dbName, err)
		}
	}

	snapSvc := snapshot.NewService(mgr)
	bm := NewManager(ManagerConfig{
		DBManager: mgr,
		Snapshots: snapSvc,
		BlobStore: blobStore,
	})
	sched := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
	})
	sched.Tick(ctx)

	// Verify both local and remote backups (F5)
	for _, remoteFlag := range []bool{false, true} {
		vRes, err := bm.Verify(ctx, VerifyOptions{
			Database: "orders",
			Remote:   remoteFlag,
		})
		if err != nil {
			t.Fatalf("Verify(remote=%v) failed: %v", remoteFlag, err)
		}
		if !vRes.IntegrityOK || !vRes.ForeignKeysOK || vRes.TableRowCounts["records"] != 2 {
			t.Fatalf("unexpected Verify result (remote=%v): %+v", remoteFlag, vRes)
		}
	}

	// Test orphan GC (>24h data object without .meta.json)
	orphanKey := "orders/2026/11/999-orphan.sqlite.gz"
	_ = blobStore.Put(ctx, orphanKey, strings.NewReader("partial"), 7, nil)
	deletedOrphans, err := bm.GarbageCollectOrphans(ctx, "orders", time.Now().UTC().Add(25*time.Hour))
	if err != nil || len(deletedOrphans) != 1 || deletedOrphans[0] != orphanKey {
		t.Fatalf("expected orphan %q to be garbage-collected, got %v (err=%v)", orphanKey, deletedOrphans, err)
	}

	// Section 9.4 Disaster Recovery Drill: close and delete the entire server data directory!
	_ = mgr.CloseAll(ctx)
	_ = store.Close()
	if err := os.RemoveAll(serverDir); err != nil {
		t.Fatalf("RemoveAll serverDir: %v", err)
	}

	freshDataDir := t.TempDir()
	if _, err := RestoreControlFromRemote(ctx, freshDataDir, blobStore, "latest", 0); err != nil {
		t.Fatalf("RestoreControlFromRemote: %v", err)
	}

	recoveredStore, err := control.OpenStore(ctx, filepath.Join(freshDataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore on recovered control.db: %v", err)
	}
	defer recoveredStore.Close()

	if _, err := recoveredStore.AuthenticateToken(ctx, adminToken); err != nil {
		t.Fatalf("expected adminToken from before disaster to authenticate against recovered control.db: %v", err)
	}

	recoveredMgr, err := manager.NewManager(manager.Config{DataDir: freshDataDir, Store: recoveredStore})
	if err != nil {
		t.Fatalf("NewManager on freshDataDir: %v", err)
	}
	defer recoveredMgr.CloseAll(ctx)

	recoveredSnapSvc := snapshot.NewService(recoveredMgr)
	recoveredBM := NewManager(ManagerConfig{
		DBManager: recoveredMgr,
		Snapshots: recoveredSnapSvc,
		BlobStore: blobStore,
	})

	for _, dbName := range []string{"orders", "users"} {
		if _, err := recoveredBM.RestoreFromRemote(ctx, dbName, "latest", ""); err != nil {
			t.Fatalf("RestoreFromRemote(%s) in DR drill: %v", dbName, err)
		}
		entry, err := recoveredMgr.Acquire(ctx, dbName)
		if err != nil {
			t.Fatalf("Acquire(%s) after DR restore: %v", dbName, err)
		}
		out, err := entry.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT COUNT(*) FROM records"})
		recoveredMgr.Release(entry)
		if err != nil {
			t.Fatalf("query %s after DR restore: %v", dbName, err)
		}
		cnt, _ := out.Rows[0][0].ToDriverValue()
		if cnt.(int64) != 2 {
			t.Fatalf("expected 2 rows in recovered %s, got %d", dbName, cnt.(int64))
		}
	}

	if _, err := recoveredBM.RestoreFromRemote(ctx, "orders", "latest", "orders_clone"); err != nil {
		t.Fatalf("RestoreFromRemote --as orders_clone: %v", err)
	}
	if _, err := recoveredBM.Verify(ctx, VerifyOptions{Database: "orders", Remote: true}); err != nil {
		t.Fatalf("Verify after DR drill: %v", err)
	}
}

func TestConcurrentWritesSnapshotInvariantAndBoundedStreamMemory(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	remoteDir := t.TempDir()

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

	if _, err := mgr.CreateDatabase(ctx, "ledger", engine.DurabilityNormal); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	entry, _ := mgr.Acquire(ctx, "ledger")
	_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER NOT NULL)"})
	_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO accounts (id, balance) VALUES (1, 500), (2, 500)"})
	mgr.Release(entry)

	snapSvc := snapshot.NewService(mgr)
	stopWriters := make(chan struct{})
	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		for {
			select {
			case <-stopWriters:
				return
			default:
			}
			e, err := mgr.Acquire(ctx, "ledger")
			if err != nil {
				continue
			}
			_, _ = e.DB.ExecuteBatch(ctx, []engine.Statement{
				{SQL: "UPDATE accounts SET balance = balance - 1 WHERE id = 1"},
				{SQL: "UPDATE accounts SET balance = balance + 1 WHERE id = 2"},
			})
			mgr.Release(e)
		}
	}()

	for i := 0; i < 3; i++ {
		snap, err := snapSvc.Create(ctx, "ledger", "under-load")
		if err != nil {
			close(stopWriters)
			writerWG.Wait()
			t.Fatalf("Snapshot under concurrent writes (%d): %v", i, err)
		}
		cloneName := "ledger_check_" + strconv.Itoa(i)
		if _, err := snapSvc.RestoreWithOptions(ctx, "ledger", snapshot.RestoreOptions{
			SnapshotID: snap.ID,
			AsNewDB:    cloneName,
		}); err != nil {
			close(stopWriters)
			writerWG.Wait()
			t.Fatalf("Restore --as %s: %v", cloneName, err)
		}
		ce, err := mgr.Acquire(ctx, cloneName)
		if err != nil {
			close(stopWriters)
			writerWG.Wait()
			t.Fatalf("Acquire %s: %v", cloneName, err)
		}
		out, err := ce.DB.ExecuteReadOnly(ctx, engine.Statement{SQL: "SELECT SUM(balance) FROM accounts"})
		mgr.Release(ce)
		if err != nil {
			close(stopWriters)
			writerWG.Wait()
			t.Fatalf("Query %s: %v", cloneName, err)
		}
		sumVal, _ := out.Rows[0][0].ToDriverValue()
		if sumVal.(int64) != 1000 {
			close(stopWriters)
			writerWG.Wait()
			t.Fatalf("invariant violated in snapshot %d: expected SUM(balance)=1000, got %d", i, sumVal.(int64))
		}
	}
	close(stopWriters)
	writerWG.Wait()

	// Verify streaming upload uses bounded memory
	largeFile := filepath.Join(dataDir, "large.bin")
	chunk := bytes.Repeat([]byte("litebase-stream-test-block-0123456789"), 1024)
	f, err := os.OpenFile(largeFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("OpenFile large: %v", err)
	}
	for i := 0; i < 512; i++ {
		_, _ = f.Write(chunk)
	}
	_ = f.Close()

	blobStore, _ := NewLocalDirStore(remoteDir, "")
	bm := NewManager(ManagerConfig{
		DBManager: mgr,
		Snapshots: snapSvc,
		BlobStore: blobStore,
	})
	runtime.GC()
	var beforeMem, afterMem runtime.MemStats
	runtime.ReadMemStats(&beforeMem)
	if _, err := bm.streamUploadOnce(ctx, largeFile, "ledger/2026/11/stream.sqlite.gz", nil); err != nil {
		t.Fatalf("streamUploadOnce: %v", err)
	}
	runtime.GC()
	runtime.ReadMemStats(&afterMem)
	var delta uint64
	if afterMem.HeapAlloc > beforeMem.HeapAlloc {
		delta = afterMem.HeapAlloc - beforeMem.HeapAlloc
	}
	if delta > 8*1024*1024 {
		t.Fatalf("expected streaming upload heap delta <= 8MB, got %d bytes", delta)
	}
}

func seedDatabaseWithPolicyForBackupTest(t *testing.T, ctx context.Context, dataDir string, remote bool) {
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

	dbRec, err := mgr.CreateDatabase(ctx, "main", engine.DurabilityFull)
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	entry, err := mgr.Acquire(ctx, "main")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, v TEXT)"})
	_, _ = entry.DB.Execute(ctx, engine.Statement{SQL: "INSERT INTO items (v) VALUES ('backup-me')"})
	mgr.Release(entry)

	if _, err := store.UpsertBackupPolicy(ctx, control.UpsertBackupPolicyParams{
		DatabaseID:      dbRec.ID,
		IntervalSeconds: 3600,
		KeepLast:        5,
		KeepDays:        7,
		Remote:          remote,
		Enabled:         true,
	}); err != nil {
		t.Fatalf("UpsertBackupPolicy: %v", err)
	}
}

func newInMemoryS3Server(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	type objData struct {
		body []byte
		mod  time.Time
	}
	objects := make(map[string]objData)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		bucket := parts[0]
		key := ""
		if len(parts) > 1 {
			key = parts[1]
		}
		if key == "" && r.Method == http.MethodGet {
			if _, isLoc := r.URL.Query()["location"]; isLoc {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
				return
			}
			prefix := r.URL.Query().Get("prefix")
			mu.Lock()
			var keys []string
			for k := range objects {
				if strings.HasPrefix(k, bucket+"/"+prefix) {
					keys = append(keys, k)
				}
			}
			sort.Strings(keys)
			type contentItem struct {
				Key          string `xml:"Key"`
				LastModified string `xml:"LastModified"`
				Size         int    `xml:"Size"`
				ETag         string `xml:"ETag"`
			}
			type listResp struct {
				XMLName     xml.Name      `xml:"ListBucketResult"`
				Name        string        `xml:"Name"`
				Prefix      string        `xml:"Prefix"`
				IsTruncated bool          `xml:"IsTruncated"`
				Contents    []contentItem `xml:"Contents"`
			}
			respObj := listResp{Name: bucket, Prefix: prefix}
			for _, fullK := range keys {
				item := objects[fullK]
				respObj.Contents = append(respObj.Contents, contentItem{
					Key:          strings.TrimPrefix(fullK, bucket+"/"),
					LastModified: item.mod.Format(time.RFC3339),
					Size:         len(item.body),
					ETag:         `"etag"`,
				})
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			_ = xml.NewEncoder(w).Encode(respObj)
			return
		}

		fullKey := bucket + "/" + key
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
				data = decodeAWSChunkedBody(data)
			}
			mu.Lock()
			objects[fullKey] = objData{body: data, mod: time.Now().UTC()}
			mu.Unlock()
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusOK)
		case http.MethodHead, http.MethodGet:
			mu.Lock()
			obj, ok := objects[fullKey]
			mu.Unlock()
			if !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>Not found</Message></Error>`)
				return
			}
			w.Header().Set("Last-Modified", obj.mod.Format(http.TimeFormat))
			w.Header().Set("Content-Length", strconv.Itoa(len(obj.body)))
			w.Header().Set("ETag", `"etag"`)
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(obj.body)
			}
		case http.MethodDelete:
			mu.Lock()
			delete(objects, fullKey)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func decodeAWSChunkedBody(raw []byte) []byte {
	var out bytes.Buffer
	remaining := raw
	for len(remaining) > 0 {
		lineEnd := bytes.Index(remaining, []byte("\r\n"))
		if lineEnd < 0 {
			break
		}
		header := string(remaining[:lineEnd])
		semi := strings.IndexByte(header, ';')
		if semi >= 0 {
			header = header[:semi]
		}
		chunkLen, err := strconv.ParseInt(strings.TrimSpace(header), 16, 64)
		if err != nil || chunkLen == 0 {
			break
		}
		dataStart := lineEnd + 2
		dataEnd := dataStart + int(chunkLen)
		if dataEnd > len(remaining) {
			break
		}
		out.Write(remaining[dataStart:dataEnd])
		if dataEnd+2 <= len(remaining) {
			remaining = remaining[dataEnd+2:]
		} else {
			break
		}
	}
	return out.Bytes()
}

func TestS3UploadFailureNeverLogsSecretCredentials(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	seedDatabaseWithPolicyForBackupTest(t, ctx, dataDir, true)

	failingS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/fail-bucket" {
			if _, isLoc := r.URL.Query()["location"]; isLoc {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(w, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`)
				return
			}
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided.</Message></Error>`)
	}))
	defer failingS3.Close()

	const (
		sentinelAccessKey = "AKIA_SECRET_SENTINEL_ID_999"
		sentinelSecretKey = "SECRET_SENTINEL_KEY_VALUE_DO_NOT_LOG_999"
	)

	t.Setenv(EnvS3Endpoint, failingS3.URL)
	t.Setenv(EnvS3Region, "us-east-1")
	t.Setenv(EnvS3Bucket, "fail-bucket")
	t.Setenv(EnvS3Prefix, "prod")
	t.Setenv(EnvS3AccessKeyID, sentinelAccessKey)
	t.Setenv(EnvS3SecretAccessKey, sentinelSecretKey)

	blobStore, err := LoadBlobStoreFromEnv()
	if err != nil {
		t.Fatalf("LoadBlobStoreFromEnv: %v", err)
	}

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

	snapSvc := snapshot.NewService(mgr)
	bm := NewManager(ManagerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BlobStore:     blobStore,
		RetryBaseWait: time.Millisecond,
	})

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sched := NewScheduler(SchedulerConfig{
		DBManager:     mgr,
		Snapshots:     snapSvc,
		BackupManager: bm,
		Logger:        logger,
	})

	_, runErr := sched.RunNow(ctx, "main")
	if runErr == nil {
		t.Fatal("expected S3 upload failure against failing endpoint")
	}

	status, err := sched.GetStatus(ctx, "main")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}

	outputsToCheck := map[string]string{
		"run error":                runErr.Error(),
		"structured log output":    logBuf.String(),
		"backup_policy.last_error": status.LastError,
	}
	for source, content := range outputsToCheck {
		if strings.Contains(content, sentinelSecretKey) {
			t.Fatalf("%s leaked LITEBASE_S3_SECRET_ACCESS_KEY: %s", source, content)
		}
		if strings.Contains(content, sentinelAccessKey) {
			t.Fatalf("%s leaked LITEBASE_S3_ACCESS_KEY_ID: %s", source, content)
		}
	}
}
