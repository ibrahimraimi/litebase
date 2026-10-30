package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrent10kWritesAndConcurrentReaders(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "concurrent.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		Durability:    DurabilityNormal,
		MaxQueueDepth: 12000,
		MaxBatchSize:  128,
		BatchWait:     250 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close(ctx)

	setupSQL := []Statement{
		{SQL: "CREATE TABLE counters (id INTEGER PRIMARY KEY, count INTEGER NOT NULL)"},
		{SQL: "INSERT INTO counters (id, count) VALUES (1, 0)"},
	}
	if _, err := db.ExecuteBatch(ctx, setupSQL); err != nil {
		t.Fatalf("setup schema failed: %v", err)
	}

	const (
		goroutineCount    = 100
		writesPerWorker   = 100
		expectedIncrement = goroutineCount * writesPerWorker
	)

	stopReaders := make(chan struct{})
	var readerErr atomic.Value
	var readerWG sync.WaitGroup
	for r := 0; r < 8; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			runConcurrentReaderCheck(ctx, db, stopReaders, &readerErr)
		}()
	}

	var writerWG sync.WaitGroup
	var succeededWrites atomic.Int64
	var busyErrors atomic.Int64
	var unexpectedErr atomic.Value

	for g := 0; g < goroutineCount; g++ {
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			for i := 0; i < writesPerWorker; i++ {
				_, err := db.Execute(ctx, Statement{
					SQL: "UPDATE counters SET count = count + 1 WHERE id = 1",
				})
				if err != nil {
					msg := strings.ToLower(err.Error())
					if strings.Contains(msg, "busy") || strings.Contains(msg, "locked") {
						busyErrors.Add(1)
					}
					if !errors.Is(err, ErrQueueFull) {
						unexpectedErr.Store(err)
					}
					continue
				}
				succeededWrites.Add(1)
			}
		}()
	}

	writerWG.Wait()
	close(stopReaders)
	readerWG.Wait()

	if busyCount := busyErrors.Load(); busyCount != 0 {
		t.Fatalf("expected 0 SQLITE_BUSY/locked errors, got %d", busyCount)
	}
	if storedErr := unexpectedErr.Load(); storedErr != nil {
		t.Fatalf("unexpected write error: %v", storedErr)
	}
	if storedReaderErr := readerErr.Load(); storedReaderErr != nil {
		t.Fatalf("concurrent reader error: %v", storedReaderErr)
	}
	if succeededWrites.Load() != expectedIncrement {
		t.Fatalf("expected %d succeeded writes, got %d", expectedIncrement, succeededWrites.Load())
	}

	finalCounter := readCounterValue(t, ctx, db)
	if finalCounter != expectedIncrement {
		t.Fatalf("counter invariant violated: expected %d, got %d", expectedIncrement, finalCounter)
	}
}

func runConcurrentReaderCheck(
	ctx context.Context,
	db *Database,
	stopCh <-chan struct{},
	errSlot *atomic.Value,
) {
	for {
		select {
		case <-stopCh:
			return
		default:
			readStart := time.Now()
			outcome, err := db.ExecuteReadOnly(ctx, Statement{
				SQL: "SELECT count FROM counters WHERE id = 1",
			})
			if err != nil {
				errSlot.CompareAndSwap(nil, err)
				return
			}
			if time.Since(readStart) > 2*time.Second {
				errSlot.CompareAndSwap(nil, errors.New("reader blocked longer than 2s threshold"))
				return
			}
			if len(outcome.Rows) != 1 || outcome.Rows[0][0].Value == nil {
				errSlot.CompareAndSwap(nil, errors.New("reader did not observe committed counter row"))
				return
			}
		}
	}
}

func readCounterValue(t *testing.T, ctx context.Context, db *Database) int64 {
	t.Helper()
	outcome, err := db.ExecuteReadOnly(ctx, Statement{
		SQL: "SELECT count FROM counters WHERE id = 1",
	})
	if err != nil {
		t.Fatalf("read counter failed: %v", err)
	}
	val, err := outcome.Rows[0][0].ToDriverValue()
	if err != nil {
		t.Fatalf("decode counter value: %v", err)
	}
	return val.(int64)
}

func TestGroupCommitSavepointIsolation(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "savepoint.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		MaxBatchSize: 16,
		BatchWait:    5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close(ctx)

	if _, err := db.Execute(ctx, Statement{
		SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE)",
	}); err != nil {
		t.Fatalf("create table failed: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 3)

	wg.Add(3)
	go func() {
		defer wg.Done()
		_, err := db.Execute(ctx, Statement{
			SQL:    "INSERT INTO items (name) VALUES (?)",
			Params: []TypedValue{mustTextValue(t, "alpha")},
		})
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := db.Execute(ctx, Statement{
			SQL: "INSERT INTO non_existent_table (name) VALUES ('bad')",
		})
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := db.Execute(ctx, Statement{
			SQL:    "INSERT INTO items (name) VALUES (?)",
			Params: []TypedValue{mustTextValue(t, "beta")},
		})
		errCh <- err
	}()
	wg.Wait()
	close(errCh)

	successCount := 0
	failureCount := 0
	for jobErr := range errCh {
		if jobErr == nil {
			successCount++
		} else {
			failureCount++
		}
	}
	if successCount != 2 || failureCount != 1 {
		t.Fatalf("expected 2 successes and 1 failure, got %d successes and %d failures", successCount, failureCount)
	}

	outcome, err := db.ExecuteReadOnly(ctx, Statement{SQL: "SELECT COUNT(*) FROM items"})
	if err != nil {
		t.Fatalf("select count failed: %v", err)
	}
	countVal, _ := outcome.Rows[0][0].ToDriverValue()
	if countVal.(int64) != 2 {
		t.Fatalf("expected 2 committed rows, got %v", countVal)
	}
}

func TestCommitFailureFailsEntireBatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "commit_fail.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		MaxBatchSize: 8,
		BatchWait:    2 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close(ctx)

	if _, err := db.Execute(ctx, Statement{
		SQL: "CREATE TABLE events (id INTEGER PRIMARY KEY, label TEXT)",
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	injectedErr := errors.New("simulated disk commit failure")
	db.writer.commitHook = func() error {
		return injectedErr
	}

	_, err = db.Execute(ctx, Statement{
		SQL:    "INSERT INTO events (label) VALUES (?)",
		Params: []TypedValue{mustTextValue(t, "should-rollback")},
	})
	if !errors.Is(err, injectedErr) {
		t.Fatalf("expected injected commit error, got: %v", err)
	}

	db.writer.commitHook = nil
	outcome, err := db.ExecuteReadOnly(ctx, Statement{SQL: "SELECT COUNT(*) FROM events"})
	if err != nil {
		t.Fatalf("select count: %v", err)
	}
	countVal, _ := outcome.Rows[0][0].ToDriverValue()
	if countVal.(int64) != 0 {
		t.Fatalf("expected 0 rows after rollback on commit failure, got %v", countVal)
	}
}

func TestWriterQueueBackpressure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "backpressure.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		MaxQueueDepth: 2,
		MaxBatchSize:  1,
		BatchWait:     1 * time.Microsecond,
	})
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close(ctx)

	blockCh := make(chan struct{})
	maintStarted := make(chan struct{})
	go func() {
		_ = db.writer.RunMaintenance(ctx, func(_ context.Context, _ *sql.Conn) error {
			close(maintStarted)
			<-blockCh
			return nil
		})
	}()
	<-maintStarted

	var queueFullSeen atomic.Bool
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.Execute(ctx, Statement{SQL: "CREATE TABLE IF NOT EXISTS t (id INT)"})
			if errors.Is(err, ErrQueueFull) {
				queueFullSeen.Store(true)
			}
		}()
	}
	time.Sleep(15 * time.Millisecond)
	close(blockCh)
	wg.Wait()

	if !queueFullSeen.Load() {
		t.Fatal("expected ErrQueueFull when writer queue capacity was exceeded")
	}
}

func TestSecurityAuthorizerAndStatementRestrictions(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "security.sqlite")
	otherPath := filepath.Join(t.TempDir(), "attached.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{})
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close(ctx)

	if _, err := db.Execute(ctx, Statement{SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)"}); err != nil {
		t.Fatalf("create table failed: %v", err)
	}

	forbiddenQueries := []string{
		"ATTACH DATABASE '" + otherPath + "' AS other",
		"DETACH DATABASE main",
		"SELECT load_extension('/tmp/evil.so')",
		"VACUUM INTO '" + otherPath + "'",
		"PRAGMA writable_schema = ON",
		"PRAGMA journal_mode = DELETE",
		"PRAGMA synchronous = OFF",
		"CREATE TABLE _litebase_internal (id INT)",
	}
	for _, sqlText := range forbiddenQueries {
		if _, err := db.Execute(ctx, Statement{SQL: sqlText}); !errors.Is(err, ErrAuthorizationDenied) {
			t.Fatalf("expected %q to fail with ErrAuthorizationDenied, got: %v", sqlText, err)
		}
	}

	if _, err := db.ExecuteReadOnly(ctx, Statement{SQL: "INSERT INTO items (name) VALUES ('denied')"}); !errors.Is(err, ErrReadOnlyViolation) {
		t.Fatalf("expected write on ExecuteReadOnly to fail with ErrReadOnlyViolation, got: %v", err)
	}

	if _, err := db.Execute(ctx, Statement{SQL: "BEGIN IMMEDIATE"}); !errors.Is(err, ErrInteractiveTxNotSupported) {
		t.Fatalf("expected BEGIN IMMEDIATE to fail with ErrInteractiveTxNotSupported, got: %v", err)
	}

	if _, err := db.Execute(ctx, Statement{SQL: "SELECT 1; DROP TABLE items"}); !errors.Is(err, ErrMultiStatementNotAllowed) {
		t.Fatalf("expected multi-statement to fail with ErrMultiStatementNotAllowed, got: %v", err)
	}
}

func mustTextValue(t *testing.T, text string) TypedValue {
	t.Helper()
	val, err := NewTextValue(text)
	if err != nil {
		t.Fatalf("NewTextValue(%q): %v", text, err)
	}
	return val
}

func TestTriggersAndBracketIdentifiers(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "triggers.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{})
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close(ctx)

	setupStmts := []Statement{
		{SQL: "CREATE TABLE [a;b] (id INTEGER PRIMARY KEY, [val;1] TEXT, updated_count INTEGER DEFAULT 0)"},
		{SQL: `CREATE TRIGGER trg_after_insert AFTER INSERT ON [a;b]
BEGIN
	UPDATE [a;b] SET updated_count = updated_count + 1 WHERE id = NEW.id;
END;`},
	}
	if _, err := db.ExecuteBatch(ctx, setupStmts); err != nil {
		t.Fatalf("ExecuteBatch with trigger and bracketed identifier failed: %v", err)
	}

	if _, err := db.Execute(ctx, Statement{
		SQL: "INSERT INTO [a;b] ([val;1]) VALUES ('hello')",
	}); err != nil {
		t.Fatalf("insert into [a;b] failed: %v", err)
	}

	outcome, err := db.ExecuteReadOnly(ctx, Statement{
		SQL: "SELECT [val;1], updated_count FROM [a;b] WHERE id = 1",
	})
	if err != nil {
		t.Fatalf("select from [a;b] failed: %v", err)
	}
	if len(outcome.Rows) != 1 || *outcome.Rows[0][1].Value != "1" {
		t.Fatalf("expected trigger to increment updated_count to 1, got: %+v", outcome.Rows)
	}
}

func TestSQLiteLimitsResponseByteCapAndMaxPageCount(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "limits.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		MaxResponseBytes: 512,
		MaxDBSizeBytes:   64 * 1024,
	})
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close(ctx)

	if _, err := db.ExecuteReadOnly(ctx, Statement{SQL: "SELECT zeroblob(1 << 26)"}); err == nil {
		t.Fatal("expected SELECT zeroblob(1 << 26) to fail due to SQLITE_LIMIT_LENGTH")
	}

	if _, err := db.Execute(ctx, Statement{SQL: "CREATE TABLE blobs (id INTEGER PRIMARY KEY, payload TEXT)"}); err != nil {
		t.Fatalf("create table blobs: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := db.Execute(ctx, Statement{SQL: "INSERT INTO blobs (payload) VALUES ( printf('%0100d', 1) )"}); err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	outcome, err := db.ExecuteReadOnly(ctx, Statement{SQL: "SELECT id, payload FROM blobs"})
	if err != nil {
		t.Fatalf("select blobs: %v", err)
	}
	if !outcome.Truncated || len(outcome.Rows) >= 10 {
		t.Fatalf("expected response byte cap to truncate rows (Truncated=%v, len=%d)", outcome.Truncated, len(outcome.Rows))
	}

	oversizedBatch := make([]Statement, 0, 20)
	for i := 0; i < 20; i++ {
		oversizedBatch = append(oversizedBatch, Statement{
			SQL: "INSERT INTO blobs (payload) VALUES ( hex(zeroblob(4096)) )",
		})
	}
	if _, err := db.ExecuteBatch(ctx, oversizedBatch); err == nil {
		t.Fatal("expected oversized batch to fail due to PRAGMA max_page_count")
	}

	countOut, err := db.ExecuteReadOnly(ctx, Statement{SQL: "SELECT COUNT(*) FROM blobs"})
	if err != nil {
		t.Fatalf("count blobs after failed batch: %v", err)
	}
	val, _ := countOut.Rows[0][0].ToDriverValue()
	if val.(int64) != 10 {
		t.Fatalf("expected failed oversized batch to roll back completely, got %d rows", val.(int64))
	}
}

func TestVacuumIntoConcurrentWithWrites(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "live.sqlite")
	snapPath := filepath.Join(t.TempDir(), "snap.sqlite")

	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{})
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close(ctx)

	if _, err := db.Execute(ctx, Statement{SQL: "CREATE TABLE items (id INTEGER PRIMARY KEY, body TEXT)"}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.Execute(ctx, Statement{SQL: "INSERT INTO items (body) VALUES (hex(zeroblob(1024)))"}); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

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
				_, _ = db.Execute(ctx, Statement{SQL: "INSERT INTO items (body) VALUES ('concurrent-write')"})
			}
		}
	}()

	if err := db.VacuumInto(ctx, snapPath); err != nil {
		close(stopCh)
		wg.Wait()
		t.Fatalf("VacuumInto during concurrent writes failed: %v", err)
	}
	close(stopCh)
	wg.Wait()

	snapDB, err := OpenDatabase(ctx, NewSQLiteEngine(), snapPath, Options{})
	if err != nil {
		t.Fatalf("OpenDatabase snapshot: %v", err)
	}
	defer snapDB.Close(ctx)
	if err := snapDB.IntegrityCheck(ctx); err != nil {
		t.Fatalf("snapshot IntegrityCheck failed: %v", err)
	}
}
