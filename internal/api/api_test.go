package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/litebase/litebase/internal/auth"
	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/manager"
)

type testEnv struct {
	server     *httptest.Server
	adminToken string
	store      *control.Store
	mgr        *manager.Manager
}

func setupTestServer(t *testing.T, srvCfg ServerConfig, engOpts engine.Options) *testEnv {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	store, err := control.OpenStore(ctx, filepath.Join(dataDir, "control.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	mgr, err := manager.NewManager(manager.Config{
		DataDir:        dataDir,
		Store:          store,
		DefaultOptions: engOpts,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, adminToken, err := store.CreateToken(ctx, control.CreateTokenParams{
		Scopes: []auth.Scope{auth.ScopeAdmin},
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	apiServer := NewServer(mgr, srvCfg)
	httpSrv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		httpSrv.Close()
		_ = apiServer.Shutdown(ctx)
		_ = store.Close()
	})
	return &testEnv{
		server:     httpSrv,
		adminToken: adminToken,
		store:      store,
		mgr:        mgr,
	}
}

func TestAPIWorkflowAndSecurityGates(t *testing.T) {
	env := setupTestServer(t, ServerConfig{
		MaxBodyBytes:       2048,
		MaxBatchStatements: 4,
	}, engine.Options{
		MaxRows: 3,
	})

	resp := doJSONRequest(t, env.server.URL, http.MethodGet, "/healthz", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /healthz, got %d", resp.StatusCode)
	}
	assertSecurityHeaders(t, resp.Header)

	resp = doJSONRequest(t, env.server.URL, http.MethodTrace, "/healthz", "", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on TRACE method, got %d", resp.StatusCode)
	}

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases", env.adminToken, map[string]any{
		"name":       "prod",
		"durability": "normal",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating database, got %d", resp.StatusCode)
	}

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases", env.adminToken, map[string]any{
		"name": "../traversal",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid database name, got %d", resp.StatusCode)
	}

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/batch", env.adminToken, map[string]any{
		"statements": []map[string]any{
			{"sql": "CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INTEGER NOT NULL)"},
			{"sql": "INSERT INTO users (name, age) VALUES ('Ada', 36)"},
			{"sql": "INSERT INTO users (name, age) VALUES ('Grace', 45)"},
			{"sql": "INSERT INTO users (name, age) VALUES ('Linus', 54)"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on batch, got %d", resp.StatusCode)
	}

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", env.adminToken, map[string]any{
		"sql":    "INSERT INTO users (name, age) VALUES (?, ?)",
		"params": []map[string]any{{"type": "text", "value": "Ken"}, {"type": "integer", "value": "80"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on parameterized insert, got %d", resp.StatusCode)
	}

	var queryOutcome engine.QueryResult
	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", env.adminToken, map[string]any{
		"sql": "SELECT id, name FROM users ORDER BY id ASC",
	})
	decodeBody(t, resp, &queryOutcome)
	if !queryOutcome.Truncated || len(queryOutcome.Rows) != 3 {
		t.Fatalf("expected truncated=true with 3 rows (MaxRows=3), got truncated=%v len=%d", queryOutcome.Truncated, len(queryOutcome.Rows))
	}

	verifyScopedTokensAndSecurity(t, env)
}

func verifyScopedTokensAndSecurity(t *testing.T, env *testEnv) {
	t.Helper()
	var tokResp createTokenResponse
	resp := doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/tokens", env.adminToken, map[string]any{
		"database": "prod",
		"scopes":   []string{"read"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating read token, got %d", resp.StatusCode)
	}
	decodeBody(t, resp, &tokResp)

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", tokResp.Token, map[string]any{
		"sql": "SELECT name FROM users WHERE age > 30",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected read token to succeed on SELECT, got %d", resp.StatusCode)
	}

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", tokResp.Token, map[string]any{
		"sql": "DELETE FROM users",
	})
	assertProblemCode(t, resp, http.StatusForbidden, CodeForbidden)

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", env.adminToken, map[string]any{
		"sql": "ATTACH DATABASE ':memory:' AS extra",
	})
	assertProblemCode(t, resp, http.StatusForbidden, CodeForbidden)

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", env.adminToken, map[string]any{
		"sql": "SELECT load_extension('evil')",
	})
	assertProblemCode(t, resp, http.StatusForbidden, CodeForbidden)

	oversizedSQL := "SELECT '" + strings.Repeat("a", 4096) + "'"
	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", env.adminToken, map[string]any{
		"sql": oversizedSQL,
	})
	assertProblemCode(t, resp, http.StatusRequestEntityTooLarge, CodePayloadTooLarge)

	resp = doJSONRequest(t, env.server.URL, http.MethodDelete, "/v1/tokens/"+tokResp.ID, env.adminToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 revoking token, got %d", resp.StatusCode)
	}

	resp = doJSONRequest(t, env.server.URL, http.MethodPost, "/v1/databases/prod/query", tokResp.Token, map[string]any{
		"sql": "SELECT 1",
	})
	assertProblemCode(t, resp, http.StatusUnauthorized, CodeUnauthorized)
}

func TestOpenAPIContractSpecPresence(t *testing.T) {
	specBytes, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}
	specText := string(specBytes)
	requiredPaths := []string{
		"/healthz:",
		"/readyz:",
		"/metrics:",
		"/v1/databases:",
		"/v1/databases/{db}:",
		"/v1/databases/{db}/query:",
		"/v1/databases/{db}/batch:",
		"/v1/databases/{db}/migrations/apply:",
		"/v1/databases/{db}/schema:",
		"/v1/databases/{db}/snapshots:",
		"/v1/databases/{db}/restore:",
		"/v1/databases/{db}/branches:",
		"/v1/tokens:",
	}
	for _, pathKey := range requiredPaths {
		if !strings.Contains(specText, pathKey) {
			t.Fatalf("openapi.yaml is missing required endpoint %q", pathKey)
		}
	}
}

func TestRateLimitingAndMetrics(t *testing.T) {
	env := setupTestServer(t, ServerConfig{
		RateLimitPerSec: 1,
		RateLimitBurst:  2,
	}, engine.Options{})

	for i := 0; i < 2; i++ {
		resp := doJSONRequest(t, env.server.URL, http.MethodGet, "/v1/databases", env.adminToken, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected request %d within burst to succeed, got %d", i, resp.StatusCode)
		}
	}
	resp := doJSONRequest(t, env.server.URL, http.MethodGet, "/v1/databases", env.adminToken, nil)
	assertProblemCode(t, resp, http.StatusTooManyRequests, CodeRateLimited)
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("expected Retry-After: 1 header on 429, got %q", resp.Header.Get("Retry-After"))
	}

	metricsResp := doJSONRequest(t, env.server.URL, http.MethodGet, "/metrics", "", nil)
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /metrics, got %d", metricsResp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if !strings.Contains(string(bodyBytes), "litebase_http_requests_total") {
		t.Fatalf("expected prometheus counter in /metrics output, got: %s", string(bodyBytes))
	}
}

func doJSONRequest(
	t *testing.T,
	baseURL string,
	method string,
	path string,
	token string,
	payload any,
) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, baseURL+path, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("execute HTTP request %s %s: %v", method, path, err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		t.Fatalf("decode response body (status %d): %v", resp.StatusCode, err)
	}
}

func assertProblemCode(t *testing.T, resp *http.Response, expectedStatus int, expectedCode string) {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != expectedStatus {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status %d, got %d (%s)", expectedStatus, resp.StatusCode, string(body))
	}
	var problem ProblemDetail
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem JSON: %v", err)
	}
	if problem.Code != expectedCode {
		t.Fatalf("expected problem code %q, got %q (detail: %s)", expectedCode, problem.Code, problem.Detail)
	}
}

func assertSecurityHeaders(t *testing.T, headers http.Header) {
	t.Helper()
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing X-Content-Type-Options: nosniff header")
	}
	if headers.Get("X-Frame-Options") != "DENY" {
		t.Fatal("missing X-Frame-Options: DENY header")
	}
	if !strings.Contains(headers.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("missing strict Content-Security-Policy header")
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatal("missing Cache-Control: no-store header")
	}
	if headers.Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id header")
	}
}
