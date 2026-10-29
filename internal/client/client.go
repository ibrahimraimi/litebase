package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/litebase/litebase/internal/api"
	"github.com/litebase/litebase/internal/auth"
	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/migrate"
)

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type APIError struct {
	Problem api.ProblemDetail
}

func (e *APIError) Error() string {
	return fmt.Sprintf("litebase api error (%d %s): %s", e.Problem.Status, e.Problem.Code, e.Problem.Detail)
}

func New(baseURL string, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type requestOptions struct {
	method         string
	path           string
	requestBody    any
	responseTarget any
}

func (c *Client) CreateDatabase(
	ctx context.Context,
	name string,
	durability engine.DurabilityMode,
) (control.DatabaseRecord, error) {
	var rec control.DatabaseRecord
	err := c.doJSON(ctx, requestOptions{
		method: http.MethodPost,
		path:   "/v1/databases",
		requestBody: map[string]any{
			"name":       name,
			"durability": durability,
		},
		responseTarget: &rec,
	})
	return rec, err
}

func (c *Client) ListDatabases(ctx context.Context) ([]control.DatabaseRecord, error) {
	var wrapper struct {
		Databases []control.DatabaseRecord `json:"databases"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodGet,
		path:           "/v1/databases",
		responseTarget: &wrapper,
	})
	return wrapper.Databases, err
}

func (c *Client) DeleteDatabase(ctx context.Context, name string) error {
	return c.doJSON(ctx, requestOptions{
		method: http.MethodDelete,
		path:   "/v1/databases/" + url.PathEscape(name),
	})
}

func (c *Client) Query(
	ctx context.Context,
	dbName string,
	stmt engine.Statement,
) (engine.QueryResult, error) {
	var outcome engine.QueryResult
	err := c.doJSON(ctx, requestOptions{
		method: http.MethodPost,
		path:   fmt.Sprintf("/v1/databases/%s/query", url.PathEscape(dbName)),
		requestBody: map[string]any{
			"sql":    stmt.SQL,
			"params": stmt.Params,
		},
		responseTarget: &outcome,
	})
	return outcome, err
}

func (c *Client) Batch(
	ctx context.Context,
	dbName string,
	statements []engine.Statement,
) ([]engine.QueryResult, error) {
	var wrapper struct {
		Results []engine.QueryResult `json:"results"`
	}
	err := c.doJSON(ctx, requestOptions{
		method: http.MethodPost,
		path:   fmt.Sprintf("/v1/databases/%s/batch", url.PathEscape(dbName)),
		requestBody: map[string]any{
			"statements": statements,
		},
		responseTarget: &wrapper,
	})
	return wrapper.Results, err
}

func (c *Client) ListAppliedMigrations(
	ctx context.Context,
	dbName string,
) ([]control.AppliedMigrationRecord, error) {
	var wrapper struct {
		Migrations []control.AppliedMigrationRecord `json:"migrations"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodGet,
		path:           fmt.Sprintf("/v1/databases/%s/migrations", url.PathEscape(dbName)),
		responseTarget: &wrapper,
	})
	return wrapper.Migrations, err
}

func (c *Client) ApplyMigrations(
	ctx context.Context,
	dbName string,
	migrations []migrate.MigrationInput,
) (migrate.ApplyResult, error) {
	return c.sendMigrationsRequest(ctx, dbName, map[string]any{
		"migrations": migrations,
		"dry_run":    false,
	})
}

func (c *Client) DryRunMigrations(
	ctx context.Context,
	dbName string,
	migrations []migrate.MigrationInput,
) (migrate.ApplyResult, error) {
	return c.sendMigrationsRequest(ctx, dbName, map[string]any{
		"migrations": migrations,
		"dry_run":    true,
	})
}

func (c *Client) sendMigrationsRequest(
	ctx context.Context,
	dbName string,
	payload map[string]any,
) (migrate.ApplyResult, error) {
	var applyOut migrate.ApplyResult
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodPost,
		path:           fmt.Sprintf("/v1/databases/%s/migrations/apply", url.PathEscape(dbName)),
		requestBody:    payload,
		responseTarget: &applyOut,
	})
	return applyOut, err
}

func (c *Client) GetSchema(ctx context.Context, dbName string) ([]migrate.SchemaObject, error) {
	var wrapper struct {
		Objects []migrate.SchemaObject `json:"objects"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodGet,
		path:           fmt.Sprintf("/v1/databases/%s/schema", url.PathEscape(dbName)),
		responseTarget: &wrapper,
	})
	return wrapper.Objects, err
}

func (c *Client) CreateSnapshot(
	ctx context.Context,
	dbName string,
	label string,
) (control.SnapshotRecord, error) {
	var rec control.SnapshotRecord
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodPost,
		path:           fmt.Sprintf("/v1/databases/%s/snapshots", url.PathEscape(dbName)),
		requestBody:    map[string]string{"label": label},
		responseTarget: &rec,
	})
	return rec, err
}

func (c *Client) ListSnapshots(ctx context.Context, dbName string) ([]control.SnapshotRecord, error) {
	var wrapper struct {
		Snapshots []control.SnapshotRecord `json:"snapshots"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodGet,
		path:           fmt.Sprintf("/v1/databases/%s/snapshots", url.PathEscape(dbName)),
		responseTarget: &wrapper,
	})
	return wrapper.Snapshots, err
}

func (c *Client) RestoreSnapshot(
	ctx context.Context,
	dbName string,
	snapshotID string,
) (control.SnapshotRecord, error) {
	var wrapper struct {
		Restored control.SnapshotRecord `json:"restored_snapshot"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodPost,
		path:           fmt.Sprintf("/v1/databases/%s/restore", url.PathEscape(dbName)),
		requestBody:    map[string]string{"snapshot_id": snapshotID},
		responseTarget: &wrapper,
	})
	return wrapper.Restored, err
}

func (c *Client) CreateBranch(
	ctx context.Context,
	parentDBName string,
	branchName string,
) (control.BranchRecord, error) {
	var wrapper struct {
		Branch control.BranchRecord `json:"branch"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodPost,
		path:           fmt.Sprintf("/v1/databases/%s/branches", url.PathEscape(parentDBName)),
		requestBody:    map[string]string{"name": branchName},
		responseTarget: &wrapper,
	})
	return wrapper.Branch, err
}

func (c *Client) ListBranches(ctx context.Context, parentDBName string) ([]control.BranchRecord, error) {
	var wrapper struct {
		Branches []control.BranchRecord `json:"branches"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodGet,
		path:           fmt.Sprintf("/v1/databases/%s/branches", url.PathEscape(parentDBName)),
		responseTarget: &wrapper,
	})
	return wrapper.Branches, err
}

func (c *Client) DeleteBranch(ctx context.Context, parentDBName string, branchName string) error {
	return c.doJSON(ctx, requestOptions{
		method: http.MethodDelete,
		path:   fmt.Sprintf("/v1/databases/%s/branches/%s", url.PathEscape(parentDBName), url.PathEscape(branchName)),
	})
}

func (c *Client) DiffBranch(ctx context.Context, parentDBName string, branchName string) ([]string, error) {
	var wrapper struct {
		Statements []string `json:"statements"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodGet,
		path:           fmt.Sprintf("/v1/databases/%s/branches/%s/diff", url.PathEscape(parentDBName), url.PathEscape(branchName)),
		responseTarget: &wrapper,
	})
	return wrapper.Statements, err
}

func (c *Client) MergeBranch(ctx context.Context, parentDBName string, branchName string) ([]string, error) {
	var wrapper struct {
		MergedStatements []string `json:"merged_statements"`
	}
	err := c.doJSON(ctx, requestOptions{
		method:         http.MethodPost,
		path:           fmt.Sprintf("/v1/databases/%s/branches/%s/merge", url.PathEscape(parentDBName), url.PathEscape(branchName)),
		responseTarget: &wrapper,
	})
	return wrapper.MergedStatements, err
}

type CreatedToken struct {
	ID       string       `json:"id"`
	Token    string       `json:"token"`
	Prefix   string       `json:"prefix"`
	Database string       `json:"database"`
	Scopes   []auth.Scope `json:"scopes"`
}

func (c *Client) CreateToken(
	ctx context.Context,
	database string,
	scopes []auth.Scope,
	expiresInSec int64,
) (CreatedToken, error) {
	var created CreatedToken
	err := c.doJSON(ctx, requestOptions{
		method: http.MethodPost,
		path:   "/v1/tokens",
		requestBody: map[string]any{
			"database":           database,
			"scopes":             scopes,
			"expires_in_seconds": expiresInSec,
		},
		responseTarget: &created,
	})
	return created, err
}

func (c *Client) DownloadDatabase(ctx context.Context, dbName string, destinationPath string) (int64, error) {
	path := fmt.Sprintf("/v1/databases/%s/download", url.PathEscape(dbName))
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request GET %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return 0, decodeAPIError(resp)
	}
	return writeStreamAtomically(destinationPath, resp.Body)
}

func (c *Client) UploadDatabase(
	ctx context.Context,
	dbName string,
	sourcePath string,
) (control.DatabaseRecord, error) {
	srcFile, err := os.Open(sourcePath)
	if err != nil {
		return control.DatabaseRecord{}, fmt.Errorf("open source database file %q: %w", sourcePath, err)
	}
	defer srcFile.Close()

	path := fmt.Sprintf("/v1/databases/%s/upload?force=true", url.PathEscape(dbName))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, srcFile)
	if err != nil {
		return control.DatabaseRecord{}, err
	}
	req.Header.Set("Content-Type", "application/vnd.sqlite3")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return control.DatabaseRecord{}, fmt.Errorf("request PUT %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return control.DatabaseRecord{}, decodeAPIError(resp)
	}
	var updated control.DatabaseRecord
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		return control.DatabaseRecord{}, fmt.Errorf("decode upload response: %w", err)
	}
	return updated, nil
}

func writeStreamAtomically(destinationPath string, reader io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		return 0, fmt.Errorf("create destination directory: %w", err)
	}
	stagingPath := destinationPath + ".pull-staging"
	outFile, err := os.OpenFile(stagingPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(outFile, reader)
	syncErr := outFile.Sync()
	closeErr := outFile.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(stagingPath)
		return 0, fmt.Errorf("write downloaded database: %w", firstNonNilError(copyErr, syncErr, closeErr))
	}
	_ = os.Remove(destinationPath + "-wal")
	_ = os.Remove(destinationPath + "-shm")
	if err := os.Rename(stagingPath, destinationPath); err != nil {
		_ = os.Remove(stagingPath)
		return 0, err
	}
	return written, nil
}

func firstNonNilError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, opts requestOptions) error {
	req, err := c.newRequest(ctx, opts.method, opts.path, opts.requestBody)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s failed: %w", opts.method, opts.path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	if opts.responseTarget == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(opts.responseTarget); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", opts.method, opts.path, err)
	}
	return nil
}

func (c *Client) newRequest(
	ctx context.Context,
	method string,
	path string,
	requestBody any,
) (*http.Request, error) {
	var bodyReader io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

func decodeAPIError(resp *http.Response) error {
	var problem api.ProblemDetail
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil || problem.Code == "" {
		return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	return &APIError{Problem: problem}
}
