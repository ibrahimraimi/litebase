package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ibrahimraimi/litebase/internal/auth"
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
	now := time.Now().UTC()
	tokenHash := auth.HashToken(plaintext)
	if cached, ok := s.lookupCachedToken(tokenHash, now); ok {
		if !auth.VerifyTokenHash(plaintext, cached.Hash) {
			return auth.TokenRecord{}, ErrNotFound
		}
		if cached.IsExpired(now) {
			return auth.TokenRecord{}, auth.ErrTokenExpired
		}
		s.recordTokenUsed(ctx, cached.ID, now)
		return cached, nil
	}

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
	if record.IsExpired(now) {
		return auth.TokenRecord{}, auth.ErrTokenExpired
	}
	s.storeCachedToken(tokenHash, record, now)
	s.recordTokenUsed(ctx, record.ID, now)
	return record, nil
}

func (s *Store) lookupCachedToken(tokenHash string, now time.Time) (auth.TokenRecord, bool) {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	entry, ok := s.tokenCache[tokenHash]
	if !ok || !now.Before(entry.expiresAt) {
		return auth.TokenRecord{}, false
	}
	return entry.record, true
}

func (s *Store) storeCachedToken(tokenHash string, record auth.TokenRecord, now time.Time) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.tokenCache[tokenHash] = cachedTokenEntry{
		record:    record,
		expiresAt: now.Add(defaultTokenCacheTTL),
	}
}

func (s *Store) recordTokenUsed(ctx context.Context, tokenID string, now time.Time) {
	var toFlush map[string]time.Time
	s.cacheMu.Lock()
	s.lastUsed[tokenID] = now
	if now.Sub(s.lastFlushAt) >= defaultLastUsedFlushInterval {
		toFlush = s.lastUsed
		s.lastUsed = make(map[string]time.Time)
		s.lastFlushAt = now
	}
	s.cacheMu.Unlock()

	if len(toFlush) > 0 {
		s.flushLastUsedMap(ctx, toFlush)
	}
}

func (s *Store) flushPendingLastUsed(ctx context.Context) {
	s.cacheMu.Lock()
	toFlush := s.lastUsed
	s.lastUsed = make(map[string]time.Time)
	s.lastFlushAt = time.Now().UTC()
	s.cacheMu.Unlock()

	if len(toFlush) > 0 {
		s.flushLastUsedMap(ctx, toFlush)
	}
}

func (s *Store) flushLastUsedMap(ctx context.Context, updates map[string]time.Time) {
	for tokenID, usedAt := range updates {
		_, _ = s.db.ExecContext(
			ctx,
			`UPDATE tokens SET last_used_at = ? WHERE id = ?`,
			usedAt.UTC().Format(time.RFC3339Nano),
			tokenID,
		)
	}
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
	s.invalidateTokenCacheByID(tokenID)
	return nil
}

func (s *Store) invalidateTokenCacheByID(tokenID string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	delete(s.lastUsed, tokenID)
	for hash, entry := range s.tokenCache {
		if entry.record.ID == tokenID {
			delete(s.tokenCache, hash)
		}
	}
}

func (s *Store) HasAdminToken(ctx context.Context) (bool, error) {
	tokens, err := s.ListTokens(ctx, DefaultProjectID)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, tok := range tokens {
		if !tok.IsExpired(now) && tok.CanAdmin() {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) ListTokens(ctx context.Context, projectID string) ([]auth.TokenRecord, error) {
	if projectID == "" {
		projectID = DefaultProjectID
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, project_id, COALESCE(database_id, ''), prefix, hash, scopes,
		        COALESCE(expires_at, ''), COALESCE(last_used_at, ''), created_at
		 FROM tokens
		 WHERE project_id = ?
		 ORDER BY created_at ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	var records []auth.TokenRecord
	for rows.Next() {
		rec, err := scanTokenRow(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
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
