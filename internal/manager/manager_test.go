package manager

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
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
