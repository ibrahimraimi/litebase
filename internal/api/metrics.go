package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/litebase/litebase/internal/manager"
)

type serverMetrics struct {
	totalQueries atomic.Uint64
	totalBatches atomic.Uint64
	mu           sync.Mutex
	byStatus     map[string]uint64
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{
		byStatus: make(map[string]uint64),
	}
}

func (sm *serverMetrics) recordRequest(method string, status int) {
	key := fmt.Sprintf(`method=%q,status="%d"`, method, status)
	sm.mu.Lock()
	sm.byStatus[key]++
	sm.mu.Unlock()
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeProblem(w, http.StatusServiceUnavailable, CodeInternalError, "control database is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var builder strings.Builder
	s.appendHTTPMetrics(&builder)

	dbMetrics, err := s.mgr.CollectDatabaseMetrics(r.Context())
	if err == nil {
		s.appendDatabaseMetrics(&builder, dbMetrics)
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(builder.String()))
}

func (s *Server) appendHTTPMetrics(builder *strings.Builder) {
	builder.WriteString("# HELP litebase_queries_total Total single SQL queries executed.\n")
	builder.WriteString("# TYPE litebase_queries_total counter\n")
	fmt.Fprintf(builder, "litebase_queries_total %d\n", s.metrics.totalQueries.Load())

	builder.WriteString("# HELP litebase_batches_total Total SQL batches executed.\n")
	builder.WriteString("# TYPE litebase_batches_total counter\n")
	fmt.Fprintf(builder, "litebase_batches_total %d\n", s.metrics.totalBatches.Load())

	builder.WriteString("# HELP litebase_http_requests_total Total HTTP requests by method and status.\n")
	builder.WriteString("# TYPE litebase_http_requests_total counter\n")

	s.metrics.mu.Lock()
	keys := make([]string, 0, len(s.metrics.byStatus))
	for k := range s.metrics.byStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(builder, "litebase_http_requests_total{%s} %d\n", k, s.metrics.byStatus[k])
	}
	s.metrics.mu.Unlock()
}

func (s *Server) appendDatabaseMetrics(builder *strings.Builder, dbMetrics []manager.DatabaseMetrics) {
	builder.WriteString("# HELP litebase_db_size_bytes SQLite main database file size in bytes.\n")
	builder.WriteString("# TYPE litebase_db_size_bytes gauge\n")
	builder.WriteString("# HELP litebase_wal_size_bytes SQLite WAL file size in bytes.\n")
	builder.WriteString("# TYPE litebase_wal_size_bytes gauge\n")
	builder.WriteString("# HELP litebase_wal_checkpoint_alert 1 when WAL size exceeds the checkpoint alert threshold.\n")
	builder.WriteString("# TYPE litebase_wal_checkpoint_alert gauge\n")
	builder.WriteString("# HELP litebase_queue_depth Current writer actor queue depth.\n")
	builder.WriteString("# TYPE litebase_queue_depth gauge\n")
	builder.WriteString("# HELP litebase_batch_size Size of the most recent group commit batch.\n")
	builder.WriteString("# TYPE litebase_batch_size gauge\n")
	builder.WriteString("# HELP litebase_commit_latency_seconds_total Cumulative writer group-commit duration in seconds.\n")
	builder.WriteString("# TYPE litebase_commit_latency_seconds_total counter\n")

	for _, dm := range dbMetrics {
		alertFlag := 0
		if dm.WALThresholdExceeded {
			alertFlag = 1
			if s.logger != nil {
				s.logger.Warn("sqlite wal size exceeded alert threshold",
					"database", dm.Name,
					"wal_size_bytes", dm.WALSizeBytes,
				)
			}
		}
		fmt.Fprintf(builder, "litebase_db_size_bytes{db=%q} %d\n", dm.Name, dm.DBSizeBytes)
		fmt.Fprintf(builder, "litebase_wal_size_bytes{db=%q} %d\n", dm.Name, dm.WALSizeBytes)
		fmt.Fprintf(builder, "litebase_wal_checkpoint_alert{db=%q} %d\n", dm.Name, alertFlag)
		fmt.Fprintf(builder, "litebase_queue_depth{db=%q} %d\n", dm.Name, dm.Writer.CurrentQueueSize)
		fmt.Fprintf(builder, "litebase_batch_size{db=%q} %d\n", dm.Name, dm.Writer.LastBatchSize)
		commitSeconds := float64(dm.Writer.TotalCommitDurationNS) / 1e9
		fmt.Fprintf(builder, "litebase_commit_latency_seconds_total{db=%q} %.6f\n", dm.Name, commitSeconds)
	}
}
