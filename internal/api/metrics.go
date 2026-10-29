package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var builder strings.Builder
	builder.WriteString("# HELP litebase_queries_total Total single SQL queries executed.\n")
	builder.WriteString("# TYPE litebase_queries_total counter\n")
	fmt.Fprintf(&builder, "litebase_queries_total %d\n", s.metrics.totalQueries.Load())

	builder.WriteString("# HELP litebase_batches_total Total SQL batches executed.\n")
	builder.WriteString("# TYPE litebase_batches_total counter\n")
	fmt.Fprintf(&builder, "litebase_batches_total %d\n", s.metrics.totalBatches.Load())

	builder.WriteString("# HELP litebase_http_requests_total Total HTTP requests by method and status.\n")
	builder.WriteString("# TYPE litebase_http_requests_total counter\n")

	s.metrics.mu.Lock()
	keys := make([]string, 0, len(s.metrics.byStatus))
	for k := range s.metrics.byStatus {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&builder, "litebase_http_requests_total{%s} %d\n", k, s.metrics.byStatus[k])
	}
	s.metrics.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(builder.String()))
}
