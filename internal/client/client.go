package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
