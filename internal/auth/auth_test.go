package auth

import (
	"errors"
	"testing"
	"time"
)

func TestGenerateAndVerifyToken(t *testing.T) {
	generated, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if len(generated.Plaintext) != len(TokenPrefixHeader)+64 {
		t.Fatalf("unexpected token length %d", len(generated.Plaintext))
	}
	if !VerifyTokenHash(generated.Plaintext, generated.Hash) {
		t.Fatal("expected VerifyTokenHash to return true for matching token")
	}
	if VerifyTokenHash(generated.Plaintext+"x", generated.Hash) {
		t.Fatal("expected VerifyTokenHash to reject modified token")
	}

	extracted, err := ExtractBearerToken("Bearer " + generated.Plaintext)
	if err != nil || extracted != generated.Plaintext {
		t.Fatalf("ExtractBearerToken failed: got %q, err %v", extracted, err)
	}
	if _, err := ExtractBearerToken("Basic abc"); !errors.Is(err, ErrMissingToken) {
		t.Fatalf("expected ErrMissingToken for Basic scheme, got %v", err)
	}
	if _, err := ExtractBearerToken("Bearer invalid_prefix"); !errors.Is(err, ErrMalformedToken) {
		t.Fatalf("expected ErrMalformedToken, got %v", err)
	}
}

func TestTokenScopesAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-1 * time.Minute)
	future := now.Add(10 * time.Minute)

	readToken := TokenRecord{
		DatabaseID: "db-1",
		Scopes:     []Scope{ScopeRead},
		ExpiresAt:  &future,
	}
	if readToken.IsExpired(now) {
		t.Fatal("expected future token not to be expired")
	}
	if !readToken.CanReadDatabase("db-1") {
		t.Fatal("expected read token to read bound db-1")
	}
	if readToken.CanReadDatabase("db-2") {
		t.Fatal("expected read token to be denied on unbound db-2")
	}
	if readToken.CanWriteDatabase("db-1") {
		t.Fatal("expected read token to be denied write access")
	}

	expiredToken := TokenRecord{
		Scopes:    []Scope{ScopeAdmin},
		ExpiresAt: &past,
	}
	if !expiredToken.IsExpired(now) {
		t.Fatal("expected past token to be expired")
	}
}
