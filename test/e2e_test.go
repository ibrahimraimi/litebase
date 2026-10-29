package test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/litebase/litebase/internal/api"
	"github.com/litebase/litebase/internal/auth"
	"github.com/litebase/litebase/internal/cli"
	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/manager"
)

func TestEndToEndCLIAndServerWorkflow(t *testing.T) {
	ctx := context.Background()
	serverDir := t.TempDir()
	projectDir := t.TempDir()

	store, err := control.OpenStore(ctx, filepath.Join(serverDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	mgr, err := manager.NewManager(manager.Config{
		DataDir: serverDir,
		Store:   store,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.CloseAll(ctx)

	_, adminToken, err := store.CreateToken(ctx, control.CreateTokenParams{
		Scopes: []auth.Scope{auth.ScopeAdmin},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	apiSrv := api.NewServer(mgr, api.ServerConfig{})
	httpSrv := httptest.NewServer(apiSrv.Handler())
	defer httpSrv.Close()

	runCLI(t, projectDir, adminToken, "init", "--project", "demoapp")
	runCLI(t, projectDir, adminToken, "link", httpSrv.URL, "--database", "main")
	runCLI(t, projectDir, adminToken, "db", "create", "main")
	runCLI(t, projectDir, adminToken, "migrate", "new", "create_users")

	migrationPath := filepath.Join(projectDir, "migrations", "0001_create_users.sql")
	migrationSQL := `CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE);
INSERT INTO users (email) VALUES ('ada@example.com');`
	if err := os.WriteFile(migrationPath, []byte(migrationSQL), 0o644); err != nil {
		t.Fatalf("write migration file: %v", err)
	}

	dryRunOut := runCLI(t, projectDir, adminToken, "deploy", "--dry-run")
	if !strings.Contains(dryRunOut, "0001") {
		t.Fatalf("expected dry-run output to mention 0001, got: %s", dryRunOut)
	}

	deployOut := runCLI(t, projectDir, adminToken, "deploy")
	if !strings.Contains(deployOut, "Applied 1 migration(s)") {
		t.Fatalf("unexpected deploy output: %s", deployOut)
	}

	statusOut := runCLI(t, projectDir, adminToken, "migrate", "status")
	if !strings.Contains(statusOut, "APPLIED") {
		t.Fatalf("expected APPLIED in migrate status output, got: %s", statusOut)
	}

	queryOut := runCLI(t, projectDir, adminToken, "query", "--remote", "SELECT email FROM users")
	if !strings.Contains(queryOut, "ada@example.com") {
		t.Fatalf("expected ada@example.com in query output, got: %s", queryOut)
	}

	verifySnapshotsAndBranchesCLI(t, projectDir, adminToken)
}

func verifySnapshotsAndBranchesCLI(t *testing.T, projectDir string, adminToken string) {
	t.Helper()
	snapOut := runCLI(t, projectDir, adminToken, "snapshot", "create", "--label", "baseline")
	if !strings.Contains(snapOut, "Created snapshot") {
		t.Fatalf("unexpected snapshot create output: %s", snapOut)
	}

	runCLI(t, projectDir, adminToken, "branch", "create", "feat-billing")
	runCLI(t, projectDir, adminToken, "branch", "checkout", "feat-billing")
	runCLI(t, projectDir, adminToken, "query", "--remote", "CREATE TABLE invoices (id INTEGER PRIMARY KEY, total_cents INTEGER NOT NULL)")

	runCLI(t, projectDir, adminToken, "branch", "checkout", "main")
	diffOut := runCLI(t, projectDir, adminToken, "branch", "diff", "feat-billing")
	if !strings.Contains(diffOut, "CREATE TABLE invoices") {
		t.Fatalf("expected CREATE TABLE invoices in branch diff output, got: %s", diffOut)
	}

	mergeOut := runCLI(t, projectDir, adminToken, "branch", "merge", "feat-billing")
	if !strings.Contains(mergeOut, "Merged 1 schema statement(s)") {
		t.Fatalf("unexpected branch merge output: %s", mergeOut)
	}

	runCLI(t, projectDir, adminToken, "query", "--remote", "INSERT INTO invoices (total_cents) VALUES (1999)")
	invOut := runCLI(t, projectDir, adminToken, "query", "--remote", "SELECT total_cents FROM invoices")
	if !strings.Contains(invOut, "1999") {
		t.Fatalf("expected 1999 in merged invoices table on main, got: %s", invOut)
	}

	verifyPullAndPushCLI(t, projectDir, adminToken)
}

func verifyPullAndPushCLI(t *testing.T, projectDir string, adminToken string) {
	t.Helper()
	pullOut := runCLI(t, projectDir, adminToken, "pull")
	if !strings.Contains(pullOut, "Pulled remote database") {
		t.Fatalf("unexpected pull output: %s", pullOut)
	}
	localQueryOut := runCLI(t, projectDir, adminToken, "query", "SELECT email FROM users")
	if !strings.Contains(localQueryOut, "ada@example.com") {
		t.Fatalf("expected pulled local DB to contain ada@example.com, got: %s", localQueryOut)
	}

	runCLI(t, projectDir, adminToken, "query", "INSERT INTO users (email) VALUES ('local-push@example.com')")

	if err := executeCLI(projectDir, adminToken, "push"); err == nil {
		t.Fatal("expected 'litebase push' without --force to fail")
	}

	pushOut := runCLI(t, projectDir, adminToken, "push", "--force")
	if !strings.Contains(pushOut, "Pushed") {
		t.Fatalf("unexpected push --force output: %s", pushOut)
	}
	remoteAfterPush := runCLI(t, projectDir, adminToken, "query", "--remote", "SELECT email FROM users WHERE email = 'local-push@example.com'")
	if !strings.Contains(remoteAfterPush, "local-push@example.com") {
		t.Fatalf("expected remote DB to contain row from pushed local DB, got: %s", remoteAfterPush)
	}
}

func runCLI(t *testing.T, workDir string, token string, args ...string) string {
	t.Helper()
	out, err := executeCLIWithOutput(workDir, token, args...)
	if err != nil {
		t.Fatalf("CLI %v failed: %v", args, err)
	}
	return out
}

func executeCLI(workDir string, token string, args ...string) error {
	_, err := executeCLIWithOutput(workDir, token, args...)
	return err
}

func executeCLIWithOutput(workDir string, token string, args ...string) (string, error) {
	cmd := cli.NewRootCommand()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	fullArgs := append([]string{"--work-dir", workDir, "--token", token}, args...)
	cmd.SetArgs(fullArgs)
	if err := cmd.Execute(); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}
