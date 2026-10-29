package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/litebase/litebase/internal/auth"
	"github.com/litebase/litebase/internal/control"
)

type contextKey string

const (
	principalContextKey contextKey = "litebase_principal"
	requestIDContextKey contextKey = "litebase_request_id"
)

// TODO(security): Add OAuth 2.0 / OIDC provider integration, MFA enforcement,
// and external KMS secret backing when user accounts and org dashboard ship in v0.4.

func principalFromContext(ctx context.Context) (auth.TokenRecord, bool) {
	rec, ok := ctx.Value(principalContextKey).(auth.TokenRecord)
	return rec, ok
}

func applySecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := newRequestID()
		w.Header().Set("X-Request-Id", reqID)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cache-Control", "no-store")

		if !isAllowedHTTPMethod(r.Method) {
			writeProblem(w, http.StatusMethodNotAllowed, CodeInvalidRequest, "HTTP method is not allowed")
			return
		}
		ctx := context.WithValue(r.Context(), requestIDContextKey, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isAllowedHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead:
		return true
	default:
		return false
	}
}

func (s *Server) withObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.metrics.recordRequest(r.Method, recorder.status)

		if s.logger != nil {
			reqID, _ := r.Context().Value(requestIDContextKey).(string)
			s.logger.Info("http_request",
				slog.String("request_id", reqID),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Duration("duration", time.Since(start)),
			)
		}
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plaintext, err := auth.ExtractBearerToken(r.Header.Get("Authorization"))
		if err != nil {
			writeMappedError(w, err)
			return
		}
		tokenRec, err := s.store.AuthenticateToken(r.Context(), plaintext)
		if err != nil {
			if errors.Is(err, control.ErrNotFound) {
				writeProblem(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or revoked API token")
				return
			}
			writeMappedError(w, err)
			return
		}
		if !s.limiter.allow(tokenRec.ID) {
			writeProblem(w, http.StatusTooManyRequests, CodeRateLimited, "per-token request rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), principalContextKey, tokenRec)
		next(w, r.WithContext(ctx))
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

type tokenRateLimiter struct {
	ratePerSec float64
	burst      float64
	mu         sync.Mutex
	buckets    map[string]*rateBucket
}

type rateBucket struct {
	tokens   float64
	lastTick time.Time
}

func newTokenRateLimiter(ratePerSec float64, burst float64) *tokenRateLimiter {
	return &tokenRateLimiter{
		ratePerSec: ratePerSec,
		burst:      burst,
		buckets:    make(map[string]*rateBucket),
	}
}

func (trl *tokenRateLimiter) allow(key string) bool {
	if trl.ratePerSec <= 0 {
		return true
	}
	trl.mu.Lock()
	defer trl.mu.Unlock()

	now := time.Now()
	bucket, exists := trl.buckets[key]
	if !exists {
		trl.buckets[key] = &rateBucket{tokens: trl.burst - 1, lastTick: now}
		return true
	}
	elapsed := now.Sub(bucket.lastTick).Seconds()
	bucket.tokens += elapsed * trl.ratePerSec
	if bucket.tokens > trl.burst {
		bucket.tokens = trl.burst
	}
	bucket.lastTick = now
	if bucket.tokens < 1.0 {
		return false
	}
	bucket.tokens -= 1.0
	return true
}

func newRequestID() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}
