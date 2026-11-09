package api

import (
	"net/http"

	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/ibrahimraimi/litebase/internal/backup"
	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
)

type pruneSnapshotsRequest struct {
	DryRun bool `json:"dry_run"`
}

type backupPolicyRequest struct {
	IntervalSeconds int64 `json:"interval_seconds"`
	KeepLast        int   `json:"keep_last"`
	KeepDays        int   `json:"keep_days"`
	Remote          bool  `json:"remote"`
}

type verifyBackupRequest struct {
	Remote       bool     `json:"remote"`
	SampleTables []string `json:"sample_tables"`
	Samples      []string `json:"samples"`
}

func (s *Server) authorizeDatabaseAdmin(w http.ResponseWriter, r *http.Request) (control.DatabaseRecord, auth.TokenRecord, bool) {
	dbName := r.PathValue("db")
	dbRec, err := s.store.GetDatabaseByName(r.Context(), control.DefaultProjectID, dbName)
	if err != nil {
		writeMappedError(w, err)
		return control.DatabaseRecord{}, auth.TokenRecord{}, false
	}
	principal, _ := principalFromContext(r.Context())
	if !principal.CanAdmin() {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "admin scope is required")
		return control.DatabaseRecord{}, auth.TokenRecord{}, false
	}
	return dbRec, principal, true
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseAdmin(w, r)
	if !ok {
		return
	}
	snapID := r.PathValue("id")
	if err := s.snapshots.Delete(r.Context(), dbRec.Name, snapID); err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:   principal.ID,
		Action:  "snapshot.delete",
		Target:  dbRec.Name,
		Details: map[string]string{"snapshot_id": snapID},
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePruneSnapshots(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseAdmin(w, r)
	if !ok {
		return
	}
	var req pruneSnapshotsRequest
	if r.ContentLength != 0 {
		if err := s.decodeJSONBody(w, r, &req); err != nil {
			writeMappedError(w, err)
			return
		}
	}
	res, err := s.snapshots.Prune(r.Context(), dbRec.Name, snapshot.PruneOptions{
		DryRun: req.DryRun,
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	if !req.DryRun {
		_ = s.store.RecordAudit(r.Context(), control.AuditParams{
			Actor:   principal.ID,
			Action:  "snapshot.prune",
			Target:  dbRec.Name,
			Details: map[string]any{"deleted_count": len(res.DeletedIDs)},
		})
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGetBackupPolicy(w http.ResponseWriter, r *http.Request) {
	dbRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	policy, err := s.store.GetBackupPolicy(r.Context(), dbRec.ID)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handlePutBackupPolicy(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseAdmin(w, r)
	if !ok {
		return
	}
	var req backupPolicyRequest
	if err := s.decodeJSONBody(w, r, &req); err != nil {
		writeMappedError(w, err)
		return
	}
	policy, err := s.store.UpsertBackupPolicy(r.Context(), control.UpsertBackupPolicyParams{
		DatabaseID:      dbRec.ID,
		IntervalSeconds: req.IntervalSeconds,
		KeepLast:        req.KeepLast,
		KeepDays:        req.KeepDays,
		Remote:          req.Remote,
		Enabled:         true,
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "backup.policy.set",
		Target: dbRec.Name,
		Details: map[string]any{
			"interval_seconds": policy.IntervalSeconds,
			"keep_last":        policy.KeepLast,
			"keep_days":        policy.KeepDays,
			"remote":           policy.Remote,
		},
	})
	writeJSON(w, http.StatusOK, policy)
}

func (s *Server) handleDeleteBackupPolicy(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseAdmin(w, r)
	if !ok {
		return
	}
	if err := s.store.DisableBackupPolicy(r.Context(), dbRec.ID); err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "backup.policy.disable",
		Target: dbRec.Name,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	dbRec, principal, ok := s.authorizeDatabaseAdmin(w, r)
	if !ok {
		return
	}
	res, err := s.scheduler.RunNow(r.Context(), dbRec.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	var snapID, remoteKey string
	if res.Snapshot != nil {
		snapID = res.Snapshot.ID
		remoteKey = res.Snapshot.RemoteKey
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "backup.run",
		Target: dbRec.Name,
		Details: map[string]any{
			"snapshot_id": snapID,
			"remote_key":  remoteKey,
		},
	})
	writeJSON(w, http.StatusCreated, res)
}

func (s *Server) handleGetBackupStatus(w http.ResponseWriter, r *http.Request) {
	dbRec, _, ok := s.authorizeDatabaseRead(w, r)
	if !ok {
		return
	}
	status, err := s.scheduler.GetStatus(r.Context(), dbRec.Name)
	if err != nil {
		writeMappedError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromContext(r.Context())
	if !principal.CanAdmin() {
		writeProblem(w, http.StatusForbidden, CodeForbidden, "admin scope is required")
		return
	}
	var req verifyBackupRequest
	if r.ContentLength != 0 {
		if err := s.decodeJSONBody(w, r, &req); err != nil {
			writeMappedError(w, err)
			return
		}
	}
	dbName := r.PathValue("db")
	if !req.Remote {
		if _, err := s.store.GetDatabaseByName(r.Context(), control.DefaultProjectID, dbName); err != nil {
			writeMappedError(w, err)
			return
		}
	}
	samples := req.Samples
	if len(samples) == 0 {
		samples = req.SampleTables
	}
	res, err := s.backups.Verify(r.Context(), backup.VerifyOptions{
		Database: dbName,
		Remote:   req.Remote,
		Samples:  samples,
	})
	if err != nil {
		writeMappedError(w, err)
		return
	}
	_ = s.store.RecordAudit(r.Context(), control.AuditParams{
		Actor:  principal.ID,
		Action: "backup.verify",
		Target: dbName,
		Details: map[string]any{
			"source":       res.Source,
			"integrity_ok": res.IntegrityOK,
		},
	})
	writeJSON(w, http.StatusOK, res)
}
