package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/litebase/litebase/internal/auth"
	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
	"github.com/litebase/litebase/internal/migrate"
)

var sqliteFileMagicHeader = []byte("SQLite format 3\x00")

type applyMigrationsRequest struct {
	Migrations []migrate.MigrationInput `json:"migrations"`
	DryRun     bool                     `json:"dry_run"`
}

type createSnapshotRequest struct {
	Label string `json:"label"`
}

type restoreSnapshotRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

type createBranchRequest struct {
	Name string `json:"name"`
}

func (s *Server) authorizeDatabaseRead(w http.ResponseWriter, r *http.Request) (control.DatabaseRecord, auth.TokenRecord, bool) {
	dbName := r.PathValue("db")
	dbRec, err := s.store.GetDatabaseByName(r.Context(), control.DefaultProjectID, dbName)
	if err != nil {
		writeMappedError(w, err)
		return control.DatabaseRecord{}, auth.TokenRecord{}, false
	}
	principal, _ := principalFromContext(r.Context())
	if !principal.CanReadDatabase(dbRec.ID) {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "read scope is required for this database")
		return control.DatabaseRecord{}, auth.TokenRecord{}, false
	}
	return dbRec, principal, true
}

func (s *Server) authorizeDatabaseWrite(w http.ResponseWriter, r *http.Request) (control.DatabaseRecord, auth.TokenRecord, bool) {
	dbName := r.PathValue("db")
	dbRec, err := s.store.GetDatabaseByName(r.Context(), control.DefaultProjectID, dbName)
	if err != nil {
		writeMappedError(w, err)
		return control.DatabaseRecord{}, auth.TokenRecord{}, false
	}
	principal, _ := principalFromContext(r.Context())
	if !principal.CanWriteDatabase(dbRec.ID) {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "write scope is required for this database")
		return control.DatabaseRecord{}, auth.TokenRecord{}, false
	}
	return dbRec, principal, true
}

func (s *Server) handleListMigrations(w http.ResponseWriter, r *http.Request) {
	dbRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	applied, err := s.store.ListAppliedMigrations(r.Context(), dbRec.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"migrations": applied})
}

func (s *Server) handleApplyMigrations(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	var req applyMigrationsRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	if req.DryRun {
		s.respondDryRunMigrations(w, r, dbRec.ID, req.Migrations)
		return
	}
	appliedVersions, err := s.migrator.ApplyPending(r.Context(), dbRec.Name, principal.ID, req.Migrations)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "migrations.apply",
		Target:  dbRec.Name,
		Details: map[string]any{"applied_versions": appliedVersions},
	})
	writeJSON(w, http.StatusOK, migrate.ApplyResult{AppliedVersions: appliedVersions, DryRun: false})
}

func (s *Server) respondDryRunMigrations(
	w http.ResponseWriter,
	r *http.Request,
	databaseID string,
	inputs []migrate.MigrationInput,
) {
	plan, err := s.migrator.BuildPlan(r.Context(), databaseID, inputs)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	pendingVersions := make([]string, 0, len(plan.Pending))
	for _, pending := range plan.Pending {
		pendingVersions = append(pendingVersions, pending.Version)
	}
	writeJSON(w, http.StatusOK, migrate.ApplyResult{AppliedVersions: pendingVersions, DryRun: true})
}

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	var req createSnapshotRequest
	if r.ContentLength != 0 {
		if err := s.decodeJSONBody(w, r, &req); err != nil {
			writeMappedError(w, err)
			return
		}
	}
	snapRec, err := s.snapshots.Create(r.Context(), dbRec.Name, req.Label)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "snapshot.create",
		Target:  dbRec.Name,
		Details: map[string]string{"snapshot_id": snapRec.ID, "label": snapRec.Label},
	})
	writeJSON(w, http.StatusCreated, snapRec)
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	dbRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	snaps, err := s.snapshots.List(r.Context(), dbRec.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) handleRestoreSnapshot(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	var req restoreSnapshotRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	restored, err := s.snapshots.Restore(r.Context(), dbRec.Name, req.SnapshotID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "snapshot.restore",
		Target:  dbRec.Name,
		Details: map[string]string{"snapshot_id": restored.ID},
	})
	writeJSON(w, http.StatusOK, map[string]any{"restored_snapshot": restored})
}

func (s *Server) handleCreateBranch(w http.ResponseWriter, r *http.Request) {
	parentRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	var req createBranchRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	branchRec, branchDBRec, err := s.branches.Create(r.Context(), parentRec.Name, req.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "branch.create",
		Target:  parentRec.Name,
		Details: map[string]string{"branch": req.Name},
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"branch":   branchRec,
		"database": branchDBRec,
	})
}

func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	parentRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	branchList, err := s.branches.List(r.Context(), parentRec.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branchList})
}

func (s *Server) handleDeleteBranch(w http.ResponseWriter, r *http.Request) {
	parentRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	branchName := r.PathValue("branch")
	if err := s.branches.Delete(r.Context(), parentRec.Name, branchName); err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "branch.delete",
		Target:  parentRec.Name,
		Details: map[string]string{"branch": branchName},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDiffBranch(w http.ResponseWriter, r *http.Request) {
	parentRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	branchName := r.PathValue("branch")
	diffSQL, err := s.branches.DiffSchema(r.Context(), parentRec.Name, branchName)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"statements": diffSQL})
}

func (s *Server) handleMergeBranch(w http.ResponseWriter, r *http.Request) {
	parentRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	branchName := r.PathValue("branch")
	mergedSQL, err := s.branches.MergeSchema(r.Context(), parentRec.Name, branchName)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "branch.merge",
		Target:  parentRec.Name,
		Details: map[string]any{"branch": branchName, "statements": len(mergedSQL)},
	})
	writeJSON(w, http.StatusOK, map[string]any{"merged_statements": mergedSQL})
}

func (s *Server) handleDownloadDatabase(w http.ResponseWriter, r *http.Request) {
	dbRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	entry, err := s.mgr.Acquire(r.Context(), dbRec.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer s.mgr.Release(entry)

	tempDir, err := os.MkdirTemp("", "litebase-pull-*")
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer os.RemoveAll(tempDir)

	exportPath := filepath.Join(tempDir, dbRec.Name+".sqlite")
	if err := entry.DB.VacuumInto(r.Context(), exportPath); err != nil {
		writeMappedError(w, err)
		return
	}
	file, err := os.Open(exportPath)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer file.Close()

	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, dbRec.Name+".sqlite"))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, file)
}

func (s *Server) handleUploadDatabase(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseWrite(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("force") != "true" {
		writeProblem(w, http.StatusBadRequest, CodeInvalidRequest, "whole-file upload requires ?force=true")
		return
	}
	tempDir, err := os.MkdirTemp("", "litebase-push-*")
	if err != nil {
		writeMappedError(w, err)
		return
	}
	defer os.RemoveAll(tempDir)

	stagedPath := filepath.Join(tempDir, "uploaded.sqlite")
	if err := s.stageAndValidateSQLiteUpload(w, r, stagedPath); err != nil {
		writeProblem(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	_, _ = s.snapshots.Create(r.Context(), dbRec.Name, "pre-push")
	if err := s.mgr.ReplaceDatabaseFile(r.Context(), dbRec.Name, stagedPath); err != nil {
		writeMappedError(w, err)
		return
	}
	updatedRec, err := s.mgr.RefreshDatabaseInfo(r.Context(), dbRec.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "database.push",
		Target:  dbRec.Name,
		Details: map[string]any{"size_bytes": updatedRec.SizeBytes},
	})
	writeJSON(w, http.StatusOK, updatedRec)
}

func (s *Server) stageAndValidateSQLiteUpload(w http.ResponseWriter, r *http.Request, stagedPath string) error {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	defer r.Body.Close()

	outFile, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	writtenBytes, copyErr := io.Copy(outFile, r.Body)
	closeErr := outFile.Close()
	if copyErr != nil {
		return fmt.Errorf("read uploaded database stream: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close staged database file: %w", closeErr)
	}
	if writtenBytes < int64(len(sqliteFileMagicHeader)) {
		return fmt.Errorf("uploaded file is too small (%d bytes) to be a valid SQLite database", writtenBytes)
	}
	return verifyStagedSQLiteFile(r.Context(), stagedPath)
}

func verifyStagedSQLiteFile(ctx context.Context, stagedPath string) error {
	header := make([]byte, len(sqliteFileMagicHeader))
	file, err := os.Open(stagedPath)
	if err != nil {
		return err
	}
	_, readErr := io.ReadFull(file, header)
	_ = file.Close()
	if readErr != nil || !bytes.Equal(header, sqliteFileMagicHeader) {
		return fmt.Errorf("uploaded file does not have a valid SQLite 3 file header")
	}
	tempDB, err := engine.OpenDatabase(ctx, engine.NewSQLiteEngine(), stagedPath, engine.Options{})
	if err != nil {
		return fmt.Errorf("open uploaded SQLite database: %w", err)
	}
	defer tempDB.Close(ctx)
	return tempDB.IntegrityCheck(ctx)
}
