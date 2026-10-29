package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/litebase/litebase/internal/auth"
)

type CreateTokenParams struct {
	ProjectID  string
	DatabaseID string
	Scopes     []auth.Scope
	ExpiresAt  *time.Time
}

func (s *Store) CreateToken(ctx context.Context, params CreateTokenParams) (auth.TokenRecord, string, error) {
	if err := auth.ValidateScopes(params.Scopes); err != nil {
		return auth.TokenRecord{}, "", err
	}
	generated, err := auth.GenerateToken()
	if err != nil {
		return auth.TokenRecord{}, "", err
	}
	record, err := s.SaveGeneratedToken(ctx, generated, params)
	if err != nil {
		return auth.TokenRecord{}, "", err
	}
	return record, generated.Plaintext, nil
}

func (s *Store) SaveGeneratedToken(
	ctx context.Context,
	generated auth.GeneratedToken,
	params CreateTokenParams,
) (auth.TokenRecord, error) {
	if err := auth.ValidateScopes(params.Scopes); err != nil {
		return auth.TokenRecord{}, err
	}
	projectID := params.ProjectID
	if projectID == "" {
		projectID = DefaultProjectID
	}
	tokenID, err := NewSortableID("tok")
	if err != nil {
		return auth.TokenRecord{}, err
	}
	createdAt := time.Now().UTC()
	var nullableDBID any
	if params.DatabaseID != "" {
		nullableDBID = params.DatabaseID
	}
	var nullableExpires any
	if params.ExpiresAt != nil {
		nullableExpires = params.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	scopesCSV := joinScopes(params.Scopes)

	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO tokens (id, project_id, database_id, prefix, hash, scopes, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tokenID,
		projectID,
		nullableDBID,
		generated.Prefix,
		generated.Hash,
		scopesCSV,
		nullableExpires,
		createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return auth.TokenRecord{}, fmt.Errorf("%w: token hash already registered", ErrAlreadyExists)
		}
		return auth.TokenRecord{}, fmt.Errorf("insert token: %w", err)
	}
	return auth.TokenRecord{
		ID:         tokenID,
		ProjectID:  projectID,
		DatabaseID: params.DatabaseID,
		Prefix:     generated.Prefix,
		Hash:       generated.Hash,
		Scopes:     params.Scopes,
		ExpiresAt:  params.ExpiresAt,
		CreatedAt:  createdAt,
	}, nil
}

func (s *Store) AuthenticateToken(ctx context.Context, plaintext string) (auth.TokenRecord, error) {
	tokenHash := auth.HashToken(plaintext)
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, project_id, COALESCE(database_id, ''), prefix, hash, scopes,
		        COALESCE(expires_at, ''), COALESCE(last_used_at, ''), created_at
		 FROM tokens
		 WHERE hash = ?`,
		tokenHash,
	)
	record, err := scanTokenRow(row)
	if err != nil {
		return auth.TokenRecord{}, err
	}
	if !auth.VerifyTokenHash(plaintext, record.Hash) {
		return auth.TokenRecord{}, ErrNotFound
	}
	if record.IsExpired(time.Now().UTC()) {
		return auth.TokenRecord{}, auth.ErrTokenExpired
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, nowStr, record.ID)
	return record, nil
}

func (s *Store) RevokeToken(ctx context.Context, tokenID string) error {
	outcome, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, tokenID)
	if err != nil {
		return fmt.Errorf("revoke token %q: %w", tokenID, err)
	}
	affected, _ := outcome.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("%w: token %q", ErrNotFound, tokenID)
	}
	return nil
}

func scanTokenRow(scanner rowScanner) (auth.TokenRecord, error) {
	var rec auth.TokenRecord
	var scopesCSV string
	var expiresStr string
	var lastUsedStr string
	var createdStr string

	err := scanner.Scan(
		&rec.ID,
		&rec.ProjectID,
		&rec.DatabaseID,
		&rec.Prefix,
		&rec.Hash,
		&scopesCSV,
		&expiresStr,
		&lastUsedStr,
		&createdStr,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.TokenRecord{}, ErrNotFound
		}
		return auth.TokenRecord{}, err
	}
	rec.Scopes = parseScopesCSV(scopesCSV)
	rec.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	if expiresStr != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, expiresStr); err == nil {
			rec.ExpiresAt = &parsed
		}
	}
	if lastUsedStr != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, lastUsedStr); err == nil {
			rec.LastUsedAt = &parsed
		}
	}
	return rec, nil
}

func joinScopes(scopes []auth.Scope) string {
	parts := make([]string, len(scopes))
	for i, sc := range scopes {
		parts[i] = string(sc)
	}
	return strings.Join(parts, ",")
}

func parseScopesCSV(scopesCSV string) []auth.Scope {
	if strings.TrimSpace(scopesCSV) == "" {
		return nil
	}
	parts := strings.Split(scopesCSV, ",")
	scopes := make([]auth.Scope, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			scopes = append(scopes, auth.Scope(trimmed))
		}
	}
	return scopes
}
