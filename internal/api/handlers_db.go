package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/litebase/litebase/internal/auth"
	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/manager"
	"github.com/litebase/litebase/internal/migrate"
)

type createDatabaseRequest struct {
	Name       string                `json:"name"`
	Durability engine.DurabilityMode `json:"durability"`
}

type queryRequest struct {
	SQL    string              `json:"sql"`
	Params []engine.TypedValue `json:"params"`
	Plain  bool                `json:"plain"`
}

type plainQueryResponse struct {
	Columns         []string `json:"columns"`
	Rows            [][]any  `json:"rows"`
	RowsAffected    int64    `json:"rows_affected"`
	LastInsertRowID *int64   `json:"last_insert_rowid"`
	Truncated       bool     `json:"truncated,omitempty"`
	DurationMS      float64  `json:"duration_ms"`
}

type batchRequest struct {
	Statements []engine.Statement `json:"statements"`
}

type createTokenRequest struct {
	Database         string       `json:"database"`
	Scopes           []auth.Scope `json:"scopes"`
	ExpiresInSeconds int64        `json:"expires_in_seconds"`
}

type createTokenResponse struct {
	ID        string       `json:"id"`
	Token     string       `json:"token"`
	Prefix    string       `json:"prefix"`
	Database  string       `json:"database,omitempty"`
	Scopes    []auth.Scope `json:"scopes"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
}

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromContext(r.Context())
	if !principal.CanAdmin() {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "admin scope is required to create databases")
		return
	}
	var req createDatabaseRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	if req.Durability != "" && req.Durability != engine.DurabilityNormal && req.Durability != engine.DurabilityFull {
		writeProblem(w, http.StatusBadRequest, CodeInvalidRequest, "durability must be 'normal' or 'full'")
		return
	}
	record, err := s.mgr.CreateDatabase(r.Context(), req.Name, req.Durability)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "database.create",
		Target:  record.Name,
		Details: map[string]string{"durability": string(record.Durability)},
	})
	writeJSON(w, http.StatusCreated, record)
}

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request) {
	records, err := s.store.ListDatabases(r.Context(), control.DefaultProjectID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": records})
}

func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	record, err := s.mgr.RefreshDatabaseInfo(r.Context(), dbName)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	principal, _ := principalFromContext(r.Context())
	if !principal.CanReadDatabase(record.ID) {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "token is not authorized for this database")
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromContext(r.Context())
	if !principal.CanAdmin() {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "admin scope is required to delete databases")
		return
	}
	dbName := r.PathValue("db")
	if err := s.mgr.DeleteDatabase(r.Context(), dbName); err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "database.delete",
		Target: dbName,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	var req queryRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	entry, err := s.mgr.Acquire(r.Context(), dbName)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer s.mgr.Release(entry)

	principal, _ := principalFromContext(r.Context())
	if !principal.CanReadDatabase(entry.Record.ID) {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "token is not authorized to access this database")
		return
	}
	queryCtx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	s.metrics.totalQueries.Add(1)
	stmt := engine.Statement{SQL: req.SQL, Params: req.Params}
	outcome, queryErr := s.executeAuthorizedQuery(queryCtx, principal, entry, stmt)
	if queryErr != nil {
		writeMappedError(w, queryErr)
		return
	}
	if req.Plain || r.URL.Query().Get("format") == "plain" {
		s.writePlainQueryOutcome(w, outcome)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

func (s *Server) executeAuthorizedQuery(
	ctx context.Context,
	principal auth.TokenRecord,
	entry *manager.ManagedEntry,
	stmt engine.Statement,
) (engine.QueryResult, error) {
	if !principal.CanWriteDatabase(entry.Record.ID) {
		return entry.DB.ExecuteReadOnly(ctx, stmt)
	}
	if quotaErr := s.mgr.CheckStorageQuota(entry.Record); quotaErr != nil {
		outcome, readErr := entry.DB.ExecuteReadOnly(ctx, stmt)
		if readErr != nil {
			return engine.QueryResult{}, quotaErr
		}
		return outcome, nil
	}
	return entry.DB.Execute(ctx, stmt)
}

func (s *Server) writePlainQueryOutcome(w http.ResponseWriter, outcome engine.QueryResult) {
	plainRows := make([][]any, len(outcome.Rows))
	for rIdx, typedRow := range outcome.Rows {
		plainRow := make([]any, len(typedRow))
		for cIdx, cell := range typedRow {
			plainVal, err := cell.ToPlainValue()
			if err != nil {
				writeMappedError(w, err)
				return
			}
			plainRow[cIdx] = plainVal
		}
		plainRows[rIdx] = plainRow
	}
	writeJSON(w, http.StatusOK, plainQueryResponse{
		Columns:         outcome.Columns,
		Rows:            plainRows,
		RowsAffected:    outcome.RowsAffected,
		LastInsertRowID: outcome.LastInsertRowID,
		Truncated:       outcome.Truncated,
		DurationMS:      outcome.DurationMS,
	})
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	var req batchRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	if len(req.Statements) == 0 {
		writeProblem(w, http.StatusBadRequest, CodeInvalidRequest, "batch statements list cannot be empty")
		return
	}
	if len(req.Statements) > s.cfg.MaxBatchStatements {
		writeProblem(w, http.StatusBadRequest, CodeInvalidRequest,
			fmt.Sprintf("batch statement count %d exceeds limit of %d", len(req.Statements), s.cfg.MaxBatchStatements))
		return
	}
	entry, err := s.mgr.Acquire(r.Context(), dbName)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer s.mgr.Release(entry)

	principal, _ := principalFromContext(r.Context())
	if !principal.CanWriteDatabase(entry.Record.ID) {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "write scope is required for batch execution")
		return
	}
	if err := s.mgr.CheckStorageQuota(entry.Record); err != nil {
		writeProblem(w, http.StatusBadRequest, CodeSQLError, err.Error())
		return
	}

	batchCtx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()

	s.metrics.totalBatches.Add(1)
	outcomes, err := entry.DB.ExecuteBatch(batchCtx, req.Statements)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": outcomes})
}

func (s *Server) handleGetSchema(w http.ResponseWriter, r *http.Request) {
	dbName := r.PathValue("db")
	entry, err := s.mgr.Acquire(r.Context(), dbName)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer s.mgr.Release(entry)

	principal, _ := principalFromContext(r.Context())
	if !principal.CanReadDatabase(entry.Record.ID) {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "read scope is required to inspect schema")
		return
	}
	objects, err := migrate.InspectSchema(r.Context(), entry.DB)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"objects": objects})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromContext(r.Context())
	if !principal.CanAdmin() {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "admin scope is required to issue tokens")
		return
	}
	var req createTokenRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	var targetDBID string
	if req.Database != "" {
		dbRec, err := s.store.GetDatabaseByName(r.Context(), control.DefaultProjectID, req.Database)
		if err != nil {
			writeMappedError(w, err)
			return
		}
		targetDBID = dbRec.ID
	}
	var expiresAt *time.Time
	if req.ExpiresInSeconds > 0 {
		exp := time.Now().UTC().Add(time.Duration(req.ExpiresInSeconds) * time.Second)
		expiresAt = &exp
	}
	tokRec, plaintext, err := s.store.CreateToken(r.Context(), control.CreateTokenParams{
		ProjectID:  control.DefaultProjectID,
		DatabaseID: targetDBID,
		Scopes:     req.Scopes,
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "token.create",
		Target: tokRec.ID,
		Details: map[string]any{
			"database": req.Database,
			"scopes":   req.Scopes,
		},
	})
	writeJSON(w, http.StatusCreated, createTokenResponse{
		ID:        tokRec.ID,
		Token:     plaintext,
		Prefix:    tokRec.Prefix,
		Database:  req.Database,
		Scopes:    tokRec.Scopes,
		ExpiresAt: tokRec.ExpiresAt,
	})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromContext(r.Context())
	if !principal.CanAdmin() {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "admin scope is required to revoke tokens")
		return
	}
	tokenID := r.PathValue("id")
	if err := s.store.RevokeToken(r.Context(), tokenID); err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "token.revoke",
		Target: tokenID,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	return decoder.Decode(target)
}
