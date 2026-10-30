package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ibrahimraimi/litebase/internal/branch"
	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/migrate"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
)

const (
	defaultMaxBodyBytes       = 4 * 1024 * 1024
	defaultMaxBatchStatements = 256
	defaultQueryTimeout       = 10 * time.Second
	defaultRateLimitPerSec    = 5000.0
	defaultRateLimitBurst     = 5000.0
)

type ServerConfig struct {
	MaxBodyBytes       int64
	MaxBatchStatements int
	QueryTimeout       time.Duration
	RateLimitPerSec    float64
	RateLimitBurst     float64
	Logger             *slog.Logger
}

func (cfg ServerConfig) withDefaults() ServerConfig {
	resolved := cfg
	if resolved.MaxBodyBytes <= 0 {
		resolved.MaxBodyBytes = defaultMaxBodyBytes
	}
	if resolved.MaxBatchStatements <= 0 {
		resolved.MaxBatchStatements = defaultMaxBatchStatements
	}
	if resolved.QueryTimeout <= 0 {
		resolved.QueryTimeout = defaultQueryTimeout
	}
	if resolved.RateLimitPerSec <= 0 {
		resolved.RateLimitPerSec = defaultRateLimitPerSec
	}
	if resolved.RateLimitBurst <= 0 {
		resolved.RateLimitBurst = defaultRateLimitBurst
	}
	return resolved
}

type Server struct {
	cfg       ServerConfig
	store     *control.Store
	mgr       *manager.Manager
	migrator  *migrate.Runner
	snapshots *snapshot.Service
	branches  *branch.Service
	limiter   *tokenRateLimiter
	metrics   *serverMetrics
	logger    *slog.Logger
	handler   http.Handler
}

func NewServer(mgr *manager.Manager, cfg ServerConfig) *Server {
	resolvedCfg := cfg.withDefaults()
	snapSvc := snapshot.NewService(mgr)
	srv := &Server{
		cfg:       resolvedCfg,
		store:     mgr.Store(),
		mgr:       mgr,
		migrator:  migrate.NewRunner(mgr),
		snapshots: snapSvc,
		branches:  branch.NewService(mgr, snapSvc),
		limiter:   newTokenRateLimiter(resolvedCfg.RateLimitPerSec, resolvedCfg.RateLimitBurst),
		metrics:   newServerMetrics(),
		logger:    resolvedCfg.Logger,
	}
	srv.handler = srv.buildHandler()
	return srv
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.mgr.CloseAll(ctx)
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.requireAuth(s.handleMetrics))

	mux.HandleFunc("POST /v1/databases", s.requireAuth(s.handleCreateDatabase))
	mux.HandleFunc("GET /v1/databases", s.requireAuth(s.handleListDatabases))
	mux.HandleFunc("GET /v1/databases/{db}", s.requireAuth(s.handleGetDatabase))
	mux.HandleFunc("DELETE /v1/databases/{db}", s.requireAuth(s.handleDeleteDatabase))

	mux.HandleFunc("POST /v1/databases/{db}/query", s.requireAuth(s.handleQuery))
	mux.HandleFunc("POST /v1/databases/{db}/batch", s.requireAuth(s.handleBatch))
	mux.HandleFunc("GET /v1/databases/{db}/schema", s.requireAuth(s.handleGetSchema))

	mux.HandleFunc("GET /v1/databases/{db}/migrations", s.requireAuth(s.handleListMigrations))
	mux.HandleFunc("POST /v1/databases/{db}/migrations/apply", s.requireAuth(s.handleApplyMigrations))

	mux.HandleFunc("POST /v1/databases/{db}/snapshots", s.requireAuth(s.handleCreateSnapshot))
	mux.HandleFunc("GET /v1/databases/{db}/snapshots", s.requireAuth(s.handleListSnapshots))
	mux.HandleFunc("POST /v1/databases/{db}/restore", s.requireAuth(s.handleRestoreSnapshot))
	mux.HandleFunc("GET /v1/databases/{db}/download", s.requireAuth(s.handleDownloadDatabase))
	mux.HandleFunc("PUT /v1/databases/{db}/upload", s.requireAuth(s.handleUploadDatabase))

	mux.HandleFunc("POST /v1/databases/{db}/branches", s.requireAuth(s.handleCreateBranch))
	mux.HandleFunc("GET /v1/databases/{db}/branches", s.requireAuth(s.handleListBranches))
	mux.HandleFunc("DELETE /v1/databases/{db}/branches/{branch}", s.requireAuth(s.handleDeleteBranch))
	mux.HandleFunc("GET /v1/databases/{db}/branches/{branch}/diff", s.requireAuth(s.handleDiffBranch))
	mux.HandleFunc("POST /v1/databases/{db}/branches/{branch}/merge", s.requireAuth(s.handleMergeBranch))

	mux.HandleFunc("POST /v1/tokens", s.requireAuth(s.handleCreateToken))
	mux.HandleFunc("GET /v1/tokens", s.requireAuth(s.handleListTokens))
	mux.HandleFunc("DELETE /v1/tokens/{id}", s.requireAuth(s.handleRevokeToken))

	return applySecurityHeaders(s.withObservability(mux))
}
