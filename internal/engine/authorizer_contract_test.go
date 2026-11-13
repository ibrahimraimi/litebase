package engine

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func containsAuthorizerEvent(events []authorizerEvent, want authorizerEvent) bool {
	for _, ev := range events {
		if ev == want {
			return true
		}
	}
	return false
}

// TestAuthorizerCallbackPinnedContract pins the exact (action, arg1, arg2, arg3)
// tuples delivered by mattn/go-sqlite3's RegisterAuthorizer across core SQL statements,
// DDL, views, and triggers so any driver/SQLite upgrade that changes callback payloads fails CI.
func TestAuthorizerCallbackPinnedContract(t *testing.T) {
	var (
		mu     sync.Mutex
		events []authorizerEvent
	)
	drv := &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			conn.RegisterAuthorizer(func(action int, arg1, arg2, arg3 string) int {
				mu.Lock()
				events = append(events, authorizerEvent{
					action: action,
					arg1:   arg1,
					arg2:   arg2,
					arg3:   arg3,
				})
				mu.Unlock()
				return sqlite3.SQLITE_OK
			})
			return nil
		},
	}

	dbPath := filepath.Join(t.TempDir(), "pinned_contract.sqlite")
	pool := sql.OpenDB(&dsnConnector{dsn: buildWriterDSN(dbPath), sqliteDriver: drv})
	pool.SetMaxOpenConns(1)
	defer pool.Close()

	ctx := context.Background()

	runStep := func(name, sqlStmt string, want authorizerEvent, extraChecks ...func([]authorizerEvent)) {
		t.Helper()
		mu.Lock()
		events = nil
		mu.Unlock()

		rows, err := pool.QueryContext(ctx, sqlStmt)
		if err != nil {
			t.Fatalf("%s (%q): %v", name, sqlStmt, err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatalf("%s rows iteration: %v", name, err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("%s close rows: %v", name, err)
		}

		mu.Lock()
		captured := append([]authorizerEvent(nil), events...)
		mu.Unlock()

		if !containsAuthorizerEvent(captured, want) {
			t.Fatalf("%s: expected authorizer event %+v, got: %+v", name, want, captured)
		}
		for _, check := range extraChecks {
			check(captured)
		}
	}

	runStep("CREATE TABLE items", `CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT)`,
		authorizerEvent{action: sqlite3.SQLITE_CREATE_TABLE, arg1: "items", arg2: "", arg3: "main"})

	runStep("CREATE TABLE audit_log", `CREATE TABLE audit_log (id INTEGER PRIMARY KEY, item_id INTEGER)`,
		authorizerEvent{action: sqlite3.SQLITE_CREATE_TABLE, arg1: "audit_log", arg2: "", arg3: "main"})

	runStep("CREATE VIEW", `CREATE VIEW v_items AS SELECT id, label FROM items`,
		authorizerEvent{action: sqlite3.SQLITE_CREATE_VIEW, arg1: "v_items", arg2: "", arg3: "main"})

	runStep("CREATE TRIGGER", `CREATE TRIGGER trg_items_audit AFTER INSERT ON items BEGIN INSERT INTO audit_log (item_id) VALUES (NEW.id); END`,
		authorizerEvent{action: sqlite3.SQLITE_CREATE_TRIGGER, arg1: "trg_items_audit", arg2: "items", arg3: "main"})

	// INSERT + trigger write into audit_log (both report arg3="main", never the trigger name).
	runStep("INSERT with trigger", `INSERT INTO items (label) VALUES ('alpha')`,
		authorizerEvent{action: sqlite3.SQLITE_INSERT, arg1: "items", arg2: "", arg3: "main"},
		func(captured []authorizerEvent) {
			t.Helper()
			triggerWrite := authorizerEvent{action: sqlite3.SQLITE_INSERT, arg1: "audit_log", arg2: "", arg3: "main"}
			if !containsAuthorizerEvent(captured, triggerWrite) {
				t.Fatalf("trigger write: expected %+v, got: %+v", triggerWrite, captured)
			}
		},
	)

	// SELECT direct table.
	runStep("SELECT", `SELECT label FROM items WHERE id = 1`,
		authorizerEvent{action: sqlite3.SQLITE_READ, arg1: "items", arg2: "label", arg3: "main"},
		func(captured []authorizerEvent) {
			t.Helper()
			selectOp := authorizerEvent{action: sqlite3.SQLITE_SELECT, arg1: "", arg2: "", arg3: ""}
			if !containsAuthorizerEvent(captured, selectOp) {
				t.Fatalf("SELECT op: expected %+v, got: %+v", selectOp, captured)
			}
		},
	)

	// View read (arg1/arg2/arg3 report underlying table/column and "main", never the view name in arg3).
	runStep("VIEW read", `SELECT label FROM v_items`,
		authorizerEvent{action: sqlite3.SQLITE_READ, arg1: "items", arg2: "label", arg3: "main"},
		func(captured []authorizerEvent) {
			t.Helper()
			for _, ev := range captured {
				if ev.arg3 == "v_items" {
					t.Fatalf("view read: expected arg3=\"main\", got %q in %+v", ev.arg3, ev)
				}
			}
		},
	)

	// UPDATE.
	runStep("UPDATE", `UPDATE items SET label = 'beta' WHERE id = 1`,
		authorizerEvent{action: sqlite3.SQLITE_UPDATE, arg1: "items", arg2: "label", arg3: "main"})

	// DELETE.
	runStep("DELETE", `DELETE FROM items WHERE id = 1`,
		authorizerEvent{action: sqlite3.SQLITE_DELETE, arg1: "items", arg2: "", arg3: "main"})

	// ALTER TABLE delivers database name in arg1 and table name in arg2.
	runStep("CREATE TABLE scratch", `CREATE TABLE scratch (id INTEGER PRIMARY KEY)`,
		authorizerEvent{action: sqlite3.SQLITE_CREATE_TABLE, arg1: "scratch", arg2: "", arg3: "main"})

	runStep("ALTER TABLE", `ALTER TABLE scratch ADD COLUMN note TEXT`,
		authorizerEvent{action: sqlite3.SQLITE_ALTER_TABLE, arg1: "main", arg2: "scratch", arg3: ""})

	// DROP TABLE.
	runStep("DROP TABLE", `DROP TABLE scratch`,
		authorizerEvent{action: sqlite3.SQLITE_DROP_TABLE, arg1: "scratch", arg2: "", arg3: "main"})
}

// TestDriverStatementCacheDisabled enforces the ADR 0019 security invariant:
// authorization runs at sqlite3_prepare time, so driver-level statement caching
// (_stmt_cache_size) must remain absent from all Litebase DSNs and disabled in runtime behavior.
func TestDriverStatementCacheDisabled(t *testing.T) {
	t.Run("dsn builders and production code never enable _stmt_cache_size", func(t *testing.T) {
		samplePath := filepath.Join(t.TempDir(), "dsn_check.sqlite")
		for _, dsn := range []string{
			buildWriterDSN(samplePath),
			buildReaderDSN(samplePath),
		} {
			if strings.Contains(dsn, "_stmt_cache_size") {
				t.Fatalf("DSN %q must not contain _stmt_cache_size", dsn)
			}
		}

		repoRoot := filepath.Clean(filepath.Join("..", ".."))
		for _, dir := range []string{"cmd", "internal"} {
			root := filepath.Join(repoRoot, dir)
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				content, readErr := os.ReadFile(path)
				if readErr != nil {
					return readErr
				}
				if strings.Contains(string(content), "_stmt_cache_size") {
					t.Errorf("production Go file %s must not enable _stmt_cache_size", path)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", root, err)
			}
		}
	})

	t.Run("same SQL on same connection is re-authorized on every execution", func(t *testing.T) {
		var (
			allowSQL     atomic.Bool
			connectCount atomic.Int32
		)
		allowSQL.Store(true)

		drv := &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				connectCount.Add(1)
				conn.RegisterAuthorizer(func(action int, arg1, _, _ string) int {
					if allowSQL.Load() {
						return sqlite3.SQLITE_OK
					}
					if action == sqlite3.SQLITE_SELECT || action == sqlite3.SQLITE_READ ||
						action == sqlite3.SQLITE_INSERT || action == sqlite3.SQLITE_UPDATE ||
						action == sqlite3.SQLITE_DELETE {
						if arg1 == "" || arg1 == "items" {
							return sqlite3.SQLITE_DENY
						}
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			},
		}

		dbPath := filepath.Join(t.TempDir(), "stmt_cache_guard.sqlite")
		pool := sql.OpenDB(&dsnConnector{dsn: buildWriterDSN(dbPath), sqliteDriver: drv})
		pool.SetMaxOpenConns(1)
		pool.SetMaxIdleConns(1)
		pool.SetConnMaxLifetime(0)
		defer pool.Close()

		ctx := context.Background()
		if _, err := pool.ExecContext(ctx, `CREATE TABLE items (id INTEGER PRIMARY KEY, label TEXT)`); err != nil {
			t.Fatalf("create table: %v", err)
		}

		const insertSQL = `INSERT INTO items (label) VALUES (?)`
		const selectSQL = `SELECT label FROM items WHERE id = ?`

		if _, err := pool.ExecContext(ctx, insertSQL, "first"); err != nil {
			t.Fatalf("first insert should be allowed: %v", err)
		}

		var got string
		if err := pool.QueryRowContext(ctx, selectSQL, 1).Scan(&got); err != nil {
			t.Fatalf("first select should be allowed: %v", err)
		}
		if got != "first" {
			t.Fatalf("expected label 'first', got %q", got)
		}

		// Deny on the exact same open connection and re-run the exact same SQL strings.
		allowSQL.Store(false)

		if _, err := pool.ExecContext(ctx, insertSQL, "second"); err == nil {
			t.Fatal("second insert on same connection must be denied by authorizer (statement cache bypass check)")
		} else {
			var sqliteErr sqlite3.Error
			if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite3.ErrAuth {
				t.Fatalf("expected sqlite3.ErrAuth on denied insert, got %v", err)
			}
		}

		var deniedLabel string
		if err := pool.QueryRowContext(ctx, selectSQL, 1).Scan(&deniedLabel); err == nil {
			t.Fatalf("second select on same connection must be denied by authorizer, got row %q", deniedLabel)
		} else {
			var sqliteErr sqlite3.Error
			if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite3.ErrAuth {
				t.Fatalf("expected sqlite3.ErrAuth on denied select, got %v", err)
			}
		}

		if count := connectCount.Load(); count != 1 {
			t.Fatalf("expected single reused connection (connectCount == 1), got %d", count)
		}
	})
}
