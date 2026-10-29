package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Scope string

const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"

	TokenPrefixHeader = "lbk_"
	tokenEntropyBytes = 32
	visiblePrefixLen  = 12
)

var (
	ErrMissingToken   = errors.New("missing bearer token")
	ErrMalformedToken = errors.New("malformed token: expected prefix lbk_")
	ErrInvalidScope   = errors.New("invalid token scope: use read, write, or admin")
	ErrTokenExpired   = errors.New("token has expired")
	ErrForbiddenScope = errors.New("token does not have required scope for this operation")
)

type GeneratedToken struct {
	Plaintext string
	Prefix    string
	Hash      string
}

type TokenRecord struct {
	ID         string     `json:"id"`
	ProjectID  string     `json:"project_id"`
	DatabaseID string     `json:"database_id,omitempty"`
	Prefix     string     `json:"prefix"`
	Hash       string     `json:"-"`
	Scopes     []Scope    `json:"scopes"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

func GenerateToken() (GeneratedToken, error) {
	randomBytes := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(randomBytes); err != nil {
		return GeneratedToken{}, fmt.Errorf("read random entropy for token: %w", err)
	}
	plaintext := TokenPrefixHeader + hex.EncodeToString(randomBytes)
	return GeneratedToken{
		Plaintext: plaintext,
		Prefix:    plaintext[:visiblePrefixLen],
		Hash:      HashToken(plaintext),
	}, nil
}

func HashToken(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(digest[:])
}

func VerifyTokenHash(plaintext string, expectedHash string) bool {
	computed := HashToken(plaintext)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(expectedHash)) == 1
}

func ExtractBearerToken(authorizationHeader string) (string, error) {
	trimmed := strings.TrimSpace(authorizationHeader)
	if trimmed == "" {
		return "", ErrMissingToken
	}
	const bearerScheme = "bearer "
	if len(trimmed) < len(bearerScheme) || !strings.EqualFold(trimmed[:len(bearerScheme)], bearerScheme) {
		return "", fmt.Errorf("%w: Authorization header must use Bearer scheme", ErrMissingToken)
	}
	token := strings.TrimSpace(trimmed[len(bearerScheme):])
	if !strings.HasPrefix(token, TokenPrefixHeader) || len(token) <= visiblePrefixLen {
		return "", ErrMalformedToken
	}
	return token, nil
}

func ValidateScopes(scopes []Scope) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%w: at least one scope is required", ErrInvalidScope)
	}
	for _, scope := range scopes {
		switch scope {
		case ScopeRead, ScopeWrite, ScopeAdmin:
		default:
			return fmt.Errorf("%w: %q", ErrInvalidScope, scope)
		}
	}
	return nil
}

func (tr TokenRecord) IsExpired(referenceTime time.Time) bool {
	if tr.ExpiresAt == nil {
		return false
	}
	return !referenceTime.Before(*tr.ExpiresAt)
}

func (tr TokenRecord) CanAdmin() bool {
	return tr.hasScope(ScopeAdmin)
}

func (tr TokenRecord) CanWriteDatabase(databaseID string) bool {
	if !tr.matchesDatabase(databaseID) {
		return false
	}
	return tr.hasScope(ScopeAdmin) || tr.hasScope(ScopeWrite)
}

func (tr TokenRecord) CanReadDatabase(databaseID string) bool {
	if !tr.matchesDatabase(databaseID) {
		return false
	}
	return tr.hasScope(ScopeAdmin) || tr.hasScope(ScopeWrite) || tr.hasScope(ScopeRead)
}

func (tr TokenRecord) matchesDatabase(targetDatabaseID string) bool {
	if tr.DatabaseID == "" {
		return true
	}
	return tr.DatabaseID == targetDatabaseID
}

func (tr TokenRecord) hasScope(target Scope) bool {
	for _, granted := range tr.Scopes {
		if granted == target {
			return true
		}
	}
	return false
}
