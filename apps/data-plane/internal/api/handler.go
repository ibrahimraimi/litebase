package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"litebase/apps/data-plane/internal/queue"

	"github.com/go-chi/chi/v5"
)

type SQLExecutor struct {
	db    *sql.DB
	queue *queue.WriteQueue
}

func NewSQLExecutor(db *sql.DB, wq *queue.WriteQueue) *SQLExecutor {
	return &SQLExecutor{
		db:    db,
		queue: wq,
	}
}

func (e *SQLExecutor) RegisterRoutes(r chi.Router) {
	r.Post("/query", e.handleQuery)
}

type QueryRequest struct {
	Query string        `json:"query"`
	Args  []interface{} `json:"args,omitempty"`
}

type QueryResponse struct {
	Columns      []string        `json:"columns,omitempty"`
	Rows         [][]interface{} `json:"rows,omitempty"`
	RowsAffected int64           `json:"rows_affected,omitempty"`
	Error        string          `json:"error,omitempty"`
}

func (e *SQLExecutor) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req QueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	queryUpper := strings.ToUpper(strings.TrimSpace(req.Query))
	isWrite := strings.HasPrefix(queryUpper, "INSERT") ||
		strings.HasPrefix(queryUpper, "UPDATE") ||
		strings.HasPrefix(queryUpper, "DELETE") ||
		strings.HasPrefix(queryUpper, "CREATE") ||
		strings.HasPrefix(queryUpper, "DROP") ||
		strings.HasPrefix(queryUpper, "ALTER")

	w.Header().Set("Content-Type", "application/json")
	var resp QueryResponse

	if isWrite {
		// Use queue for writes to prevent locking issues
		res, err := e.queue.Enqueue(req.Query, req.Args...)
		if err != nil {
			resp.Error = err.Error()
			json.NewEncoder(w).Encode(resp)
			return
		}
		affected, _ := res.RowsAffected()
		resp.RowsAffected = affected
	} else {
		// Use direct DB for reads (allows concurrency)
		rows, err := e.db.Query(req.Query, req.Args...)
		if err != nil {
			resp.Error = err.Error()
			json.NewEncoder(w).Encode(resp)
			return
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err == nil {
			resp.Columns = cols
		}

		var results [][]interface{}
		for rows.Next() {
			valPointers := make([]interface{}, len(cols))
			vals := make([]interface{}, len(cols))
			for i := range vals {
				valPointers[i] = &vals[i]
			}

			if err := rows.Scan(valPointers...); err != nil {
				resp.Error = err.Error()
				json.NewEncoder(w).Encode(resp)
				return
			}
			results = append(results, vals)
		}
		resp.Rows = results
	}

	json.NewEncoder(w).Encode(resp)
}
