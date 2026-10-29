package engine

import (
	"context"
	"path/filepath"
	"testing"
)

func BenchmarkWritesNoGroupCommit(b *testing.B) {
	runWriteBenchmark(b, Options{
		Durability:    DurabilityNormal,
		MaxBatchSize:  1,
		MaxQueueDepth: 8192,
	})
}

func BenchmarkWritesWithGroupCommit(b *testing.B) {
	runWriteBenchmark(b, Options{
		Durability:    DurabilityNormal,
		MaxBatchSize:  64,
		MaxQueueDepth: 8192,
	})
}

func runWriteBenchmark(b *testing.B, opts Options) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench.sqlite")
	db, err := OpenDatabase(ctx, NewSQLiteEngine(), dbPath, opts)
	if err != nil {
		b.Fatalf("OpenDatabase: %v", err)
	}
	defer db.Close(ctx)

	if _, err := db.Execute(ctx, Statement{
		SQL: "CREATE TABLE events (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL)",
	}); err != nil {
		b.Fatalf("create table: %v", err)
	}

	sampleBody, _ := NewTextValue(`{"event":"click","user":42}`)
	stmt := Statement{
		SQL:    "INSERT INTO events (body) VALUES (?)",
		Params: []TypedValue{sampleBody},
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := db.Execute(ctx, stmt); err != nil {
				b.Errorf("write failed: %v", err)
			}
		}
	})
}
