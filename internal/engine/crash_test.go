package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	crashHelperEnvKey  = "LITEBASE_CRASH_HELPER"
	crashDBPathEnvKey  = "LITEBASE_CRASH_DB_PATH"
	crashSideLogEnvKey = "LITEBASE_CRASH_SIDELOG_PATH"
)

func TestCrashDurabilityKill9(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "crash.sqlite")
	sideLogPath := filepath.Join(tempDir, "acknowledged.log")

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	for iteration := 0; iteration < 3; iteration++ {
		runSingleCrashIteration(t, testBinary, dbPath, sideLogPath)
	}
}

func runSingleCrashIteration(t *testing.T, testBinary string, dbPath string, sideLogPath string) {
	t.Helper()
	cmd := exec.Command(testBinary, "-test.run=^TestCrashSubprocessWorkload$")
	cmd.Env = append(os.Environ(),
		crashHelperEnvKey+"=1",
		crashDBPathEnvKey+"="+dbPath,
		crashSideLogEnvKey+"="+sideLogPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start crash helper process: %v", err)
	}

	waitForAcknowledgedWrites(t, sideLogPath, 25)
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_, _ = cmd.Process.Wait()

	verifyRecoveredDatabase(t, dbPath, sideLogPath)
}

func waitForAcknowledgedWrites(t *testing.T, sideLogPath string, minLines int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ackIDs, err := readSideLogIDs(sideLogPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read side log %s: %v", sideLogPath, err)
		}
		if len(ackIDs) >= minLines {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for at least %d acknowledged writes in %s", minLines, sideLogPath)
}

func verifyRecoveredDatabase(t *testing.T, dbPath string, sideLogPath string) {
	t.Helper()
	ctx := context.Background()
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		Durability: DurabilityFull,
	})
	if err != nil {
		t.Fatalf("reopen database after SIGKILL: %v", err)
	}
	defer db.Close(ctx)

	if err := db.IntegrityCheck(ctx); err != nil {
		t.Fatalf("post-crash integrity_check failed: %v", err)
	}

	ackIDs, err := readSideLogIDs(sideLogPath)
	if err != nil {
		t.Fatalf("read side log %s: %v", sideLogPath, err)
	}
	if len(ackIDs) == 0 {
		t.Fatal("expected acknowledged writes in side log")
	}

	for _, id := range ackIDs {
		outcome, err := db.ExecuteReadOnly(ctx, Statement{
			SQL:    "SELECT seq FROM ledger WHERE seq = ?",
			Params: []TypedValue{NewIntValue(id)},
		})
		if err != nil {
			t.Fatalf("query acknowledged seq %d: %v", id, err)
		}
		if len(outcome.Rows) != 1 {
			t.Fatalf("acknowledged write seq=%d missing after SIGKILL recovery", id)
		}
	}
}

func readSideLogIDs(sideLogPath string) ([]int64, error) {
	file, err := os.Open(sideLogPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ids []int64
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parsed, err := strconv.ParseInt(line, 10, 64)
		if err == nil {
			ids = append(ids, parsed)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan side log %s: %w", sideLogPath, err)
	}
	return ids, nil
}

func TestCrashSubprocessWorkload(t *testing.T) {
	if os.Getenv(crashHelperEnvKey) != "1" {
		return
	}
	dbPath := os.Getenv(crashDBPathEnvKey)
	sideLogPath := os.Getenv(crashSideLogEnvKey)

	ctx := context.Background()
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, Options{
		Durability:   DurabilityFull,
		MaxBatchSize: 8,
		BatchWait:    100 * time.Microsecond,
	})
	if err != nil {
		os.Exit(2)
	}

	if _, err := db.Execute(ctx, Statement{
		SQL: "CREATE TABLE IF NOT EXISTS ledger (seq INTEGER PRIMARY KEY, payload TEXT NOT NULL)",
	}); err != nil {
		os.Exit(3)
	}

	logFile, err := os.OpenFile(sideLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(4)
	}
	var logMu sync.Mutex

	baseOffset := time.Now().UnixNano()
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for step := 1; step <= 100000; step++ {
				seq := baseOffset + int64(workerID*1000000+step)
				payload, _ := NewTextValue(fmt.Sprintf("worker-%d-step-%d", workerID, step))
				_, err := db.Execute(ctx, Statement{
					SQL:    "INSERT INTO ledger (seq, payload) VALUES (?, ?)",
					Params: []TypedValue{NewIntValue(seq), payload},
				})
				if err != nil {
					continue
				}
				logMu.Lock()
				_, _ = fmt.Fprintf(logFile, "%d\n", seq)
				_ = logFile.Sync()
				logMu.Unlock()
			}
		}(worker)
	}
	wg.Wait()
}
