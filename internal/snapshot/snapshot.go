package snapshot

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/testhook"
)

type Service struct {
	mgr *manager.Manager
}

func NewService(mgr *manager.Manager) *Service {
	return &Service{mgr: mgr}
}

func (s *Service) Create(
	ctx context.Context,
	dbName string,
	label string,
) (control.SnapshotRecord, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return control.SnapshotRecord{}, err
	}
	cleanLabel := sanitizeLabel(label)
	if err := engine.ValidateName(cleanLabel); err != nil {
		return control.SnapshotRecord{}, fmt.Errorf("invalid snapshot label %q: %w", label, err)
	}

	entry, err := s.mgr.Acquire(ctx, dbName)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	defer s.mgr.Release(entry)

	filename := fmt.Sprintf("%d-%s.sqlite", time.Now().UTC().UnixNano(), cleanLabel)
	destPath, err := engine.SafeJoinPath(s.mgr.DataDir(), "snapshots", dbName, filename)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	tmpPath := destPath + ".tmp"
	_ = os.Remove(tmpPath)
	if err := entry.DB.VacuumInto(ctx, tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return control.SnapshotRecord{}, err
	}
	testhook.At("snapshot.during-vacuum")

	sizeBytes, err := verifyAndPromoteSnapshotTempFile(ctx, tmpPath, destPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return control.SnapshotRecord{}, err
	}
	return s.mgr.Store().CreateSnapshotRecord(ctx, control.CreateSnapshotParams{
		DatabaseID:   entry.Record.ID,
		SnapshotPath: destPath,
		SizeBytes:    sizeBytes,
		Label:        cleanLabel,
	})
}

func verifyAndPromoteSnapshotTempFile(ctx context.Context, tmpPath string, destPath string) (int64, error) {
	if err := engine.SyncFileOnDisk(tmpPath); err != nil {
		return 0, fmt.Errorf("fsync temporary snapshot %q: %w", tmpPath, err)
	}
	if err := engine.CheckFileIntegrity(ctx, tmpPath); err != nil {
		return 0, err
	}
	_, sizeBytes, err := engine.ComputeFileSHA256(tmpPath)
	if err != nil {
		return 0, fmt.Errorf("hash temporary snapshot %q: %w", tmpPath, err)
	}
	if err := engine.AtomicReplace(tmpPath, destPath); err != nil {
		return 0, fmt.Errorf("promote snapshot %q into place: %w", destPath, err)
	}
	return sizeBytes, nil
}

func (s *Service) List(ctx context.Context, dbName string) ([]control.SnapshotRecord, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return nil, err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return nil, err
	}
	return s.mgr.Store().ListSnapshotRecords(ctx, dbRec.ID)
}

func (s *Service) Restore(
	ctx context.Context,
	dbName string,
	snapshotID string,
) (control.SnapshotRecord, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return control.SnapshotRecord{}, err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	targetSnap, err := s.mgr.Store().GetSnapshotRecord(ctx, dbRec.ID, snapshotID)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	if _, err := os.Stat(targetSnap.Path); err != nil {
		return control.SnapshotRecord{}, fmt.Errorf("snapshot file missing on disk: %w", err)
	}
	if _, err := s.Create(ctx, dbName, "pre-restore"); err != nil {
		return control.SnapshotRecord{}, fmt.Errorf("create automatic pre-restore snapshot: %w", err)
	}
	if err := s.mgr.ReplaceDatabaseFile(ctx, dbName, targetSnap.Path); err != nil {
		return control.SnapshotRecord{}, err
	}
	return targetSnap, s.verifyRestoredDatabase(ctx, dbName)
}

func (s *Service) verifyRestoredDatabase(ctx context.Context, dbName string) error {
	entry, err := s.mgr.Acquire(ctx, dbName)
	if err != nil {
		return fmt.Errorf("reopen database %q after restore: %w", dbName, err)
	}
	defer s.mgr.Release(entry)
	return entry.DB.IntegrityCheck(ctx)
}

func sanitizeLabel(label string) string {
	if label == "" {
		return "manual"
	}
	return label
}
