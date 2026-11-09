package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/testhook"
)

const (
	DefaultAutoSnapshotKeepLast = 5
	DefaultAutoSnapshotKeepDays = 7
)

var (
	ErrSnapshotProtected        = errors.New("snapshot is pinned or referenced by an active branch")
	ErrSnapshotChecksumMismatch = errors.New("snapshot SHA-256 checksum mismatch")
)

type RemoteDeleteFunc func(ctx context.Context, remoteKey string) error

type CreateOptions struct {
	Label        string
	Kind         string
	Pinned       bool
	RemoteStatus string
}

type RestoreOptions struct {
	SnapshotID string
	AsNewDB    string
}

type PruneOptions struct {
	DryRun   bool
	KeepLast int
	KeepDays int
	Now      time.Time
}

type PruneResult struct {
	DeletedIDs []string `json:"deleted_ids"`
	FreedBytes int64    `json:"freed_bytes"`
	DryRun     bool     `json:"dry_run"`
}

type Service struct {
	mgr           *manager.Manager
	remoteDeleter RemoteDeleteFunc
}

func NewService(mgr *manager.Manager) *Service {
	return &Service{mgr: mgr}
}

func (s *Service) SetRemoteDeleter(fn RemoteDeleteFunc) {
	s.remoteDeleter = fn
}

func (s *Service) Create(
	ctx context.Context,
	dbName string,
	label string,
) (control.SnapshotRecord, error) {
	return s.CreateWithOptions(ctx, dbName, CreateOptions{
		Label: label,
	})
}

func (s *Service) CreateWithOptions(
	ctx context.Context,
	dbName string,
	opts CreateOptions,
) (control.SnapshotRecord, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return control.SnapshotRecord{}, err
	}
	cleanLabel := sanitizeLabel(opts.Label)
	if err := engine.ValidateName(cleanLabel); err != nil {
		return control.SnapshotRecord{}, fmt.Errorf("invalid snapshot label %q: %w", opts.Label, err)
	}
	kind := opts.Kind
	if kind == "" {
		kind = control.InferSnapshotKind(cleanLabel)
	}

	entry, err := s.mgr.Acquire(ctx, dbName)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	defer s.mgr.Release(entry)

	filename := fmt.Sprintf("%d-%s.sqlite", time.Now().UTC().UnixNano(), cleanLabel)
	relPath := filepath.ToSlash(filepath.Join("snapshots", dbName, filename))
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

	sha256Hex, sizeBytes, err := verifyAndPromoteSnapshotTempFile(ctx, tmpPath, destPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return control.SnapshotRecord{}, err
	}
	return s.mgr.Store().CreateSnapshotRecord(ctx, control.CreateSnapshotParams{
		DatabaseID:   entry.Record.ID,
		SnapshotPath: destPath,
		RelPath:      relPath,
		SHA256:       sha256Hex,
		Kind:         kind,
		Pinned:       opts.Pinned,
		RemoteStatus: opts.RemoteStatus,
		SizeBytes:    sizeBytes,
		Label:        cleanLabel,
	})
}

func verifyAndPromoteSnapshotTempFile(ctx context.Context, tmpPath string, destPath string) (string, int64, error) {
	if err := engine.SyncFileOnDisk(tmpPath); err != nil {
		return "", 0, fmt.Errorf("fsync temporary snapshot %q: %w", tmpPath, err)
	}
	if err := engine.CheckFileIntegrity(ctx, tmpPath); err != nil {
		return "", 0, err
	}
	sha256Hex, sizeBytes, err := engine.ComputeFileSHA256(tmpPath)
	if err != nil {
		return "", 0, fmt.Errorf("hash temporary snapshot %q: %w", tmpPath, err)
	}
	if err := engine.AtomicReplace(tmpPath, destPath); err != nil {
		return "", 0, fmt.Errorf("promote snapshot %q into place: %w", destPath, err)
	}
	return sha256Hex, sizeBytes, nil
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

func (s *Service) EnsureChecksum(ctx context.Context, snap control.SnapshotRecord) (string, error) {
	if _, err := os.Stat(snap.Path); err != nil {
		return "", fmt.Errorf("snapshot file missing on disk: %w", err)
	}
	actualSHA256, _, err := engine.ComputeFileSHA256(snap.Path)
	if err != nil {
		return "", fmt.Errorf("compute snapshot checksum: %w", err)
	}
	if snap.SHA256 != "" && actualSHA256 != snap.SHA256 {
		return "", fmt.Errorf("%w: snapshot %s expected %s, got %s",
			ErrSnapshotChecksumMismatch, snap.ID, snap.SHA256, actualSHA256)
	}
	if err := engine.CheckFileIntegrity(ctx, snap.Path); err != nil {
		return "", err
	}
	if snap.SHA256 == "" {
		_ = s.mgr.Store().UpdateSnapshotChecksum(ctx, snap.ID, actualSHA256)
	}
	return actualSHA256, nil
}

func (s *Service) Restore(
	ctx context.Context,
	dbName string,
	snapshotID string,
) (control.SnapshotRecord, error) {
	return s.RestoreWithOptions(ctx, dbName, RestoreOptions{
		SnapshotID: snapshotID,
	})
}

func (s *Service) RestoreWithOptions(
	ctx context.Context,
	dbName string,
	opts RestoreOptions,
) (control.SnapshotRecord, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return control.SnapshotRecord{}, err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	targetSnap, err := s.mgr.Store().GetSnapshotRecord(ctx, dbRec.ID, opts.SnapshotID)
	if err != nil {
		return control.SnapshotRecord{}, err
	}
	if _, err := s.EnsureChecksum(ctx, targetSnap); err != nil {
		return control.SnapshotRecord{}, err
	}
	if opts.AsNewDB != "" {
		if err := s.restoreSnapshotAsNewDatabase(ctx, targetSnap.Path, opts.AsNewDB, dbRec.Durability); err != nil {
			return control.SnapshotRecord{}, err
		}
		return targetSnap, nil
	}
	if _, err := s.CreateWithOptions(ctx, dbName, CreateOptions{
		Label: "pre-restore",
		Kind:  control.SnapshotKindPreRestore,
	}); err != nil {
		return control.SnapshotRecord{}, fmt.Errorf("create automatic pre-restore snapshot: %w", err)
	}
	if err := s.mgr.ReplaceDatabaseFile(ctx, dbName, targetSnap.Path); err != nil {
		return control.SnapshotRecord{}, err
	}
	_, _ = s.Prune(ctx, dbName, PruneOptions{})
	return targetSnap, s.verifyRestoredDatabase(ctx, dbName)
}

func (s *Service) restoreSnapshotAsNewDatabase(
	ctx context.Context,
	sourcePath string,
	newDBName string,
	durability engine.DurabilityMode,
) error {
	if err := engine.ValidateName(newDBName); err != nil {
		return err
	}
	destPath, err := engine.SafeJoinPath(
		s.mgr.DataDir(),
		"databases",
		control.DefaultProjectName,
		newDBName,
		"main.sqlite",
	)
	if err != nil {
		return err
	}
	if err := stageAndReplaceNewDatabaseFile(sourcePath, destPath); err != nil {
		return err
	}
	if _, err := s.mgr.CreateDatabaseAtPath(ctx, newDBName, destPath, durability); err != nil {
		return err
	}
	return s.verifyRestoredDatabase(ctx, newDBName)
}

func stageAndReplaceNewDatabaseFile(sourcePath string, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return err
	}
	tmpPath := destPath + ".restore-staging"
	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, src)
	closeErr := dst.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := engine.AtomicReplace(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (s *Service) Delete(ctx context.Context, dbName string, snapshotID string) error {
	if err := engine.ValidateName(dbName); err != nil {
		return err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return err
	}
	snap, err := s.mgr.Store().GetSnapshotRecord(ctx, dbRec.ID, snapshotID)
	if err != nil {
		return err
	}
	if err := s.checkDeletable(ctx, snap); err != nil {
		return err
	}
	return s.deleteSnapshotFileAndRecord(ctx, dbRec.ID, snap, true)
}

func (s *Service) checkDeletable(ctx context.Context, snap control.SnapshotRecord) error {
	if snap.Pinned {
		return fmt.Errorf("%w: snapshot %q is pinned", ErrSnapshotProtected, snap.ID)
	}
	referenced, err := s.mgr.Store().IsSnapshotReferencedByBranch(ctx, snap.ID)
	if err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("%w: snapshot %q is referenced by a branch", ErrSnapshotProtected, snap.ID)
	}
	return nil
}

func (s *Service) deleteSnapshotFileAndRecord(
	ctx context.Context,
	databaseID string,
	snap control.SnapshotRecord,
	deleteRemote bool,
) error {
	if deleteRemote && snap.RemoteKey != "" && s.remoteDeleter != nil {
		if err := s.remoteDeleter(ctx, snap.RemoteKey); err != nil {
			return fmt.Errorf("delete remote snapshot %q: %w", snap.RemoteKey, err)
		}
	}
	if err := os.Remove(snap.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove snapshot file %q: %w", snap.Path, err)
	}
	_ = engine.SyncParentDir(filepath.Dir(snap.Path))
	return s.mgr.Store().DeleteSnapshotRecord(ctx, databaseID, snap.ID)
}

func (s *Service) Prune(ctx context.Context, dbName string, opts PruneOptions) (PruneResult, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return PruneResult{}, err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return PruneResult{}, err
	}
	keepLast, keepDays, pruneRemote := s.resolveRetentionPolicy(ctx, dbRec.ID, opts)
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	snaps, err := s.mgr.Store().ListSnapshotRecords(ctx, dbRec.ID)
	if err != nil {
		return PruneResult{}, err
	}
	branchSnapIDs, err := s.mgr.Store().ListBranchReferencedSnapshotIDs(ctx, dbRec.ID)
	if err != nil {
		return PruneResult{}, err
	}

	candidates := selectPruneCandidates(snaps, branchSnapIDs, keepLast, keepDays, now)
	result := PruneResult{
		DeletedIDs: make([]string, 0, len(candidates)),
		DryRun:     opts.DryRun,
	}
	for _, snap := range candidates {
		if !opts.DryRun {
			if err := s.deleteSnapshotFileAndRecord(ctx, dbRec.ID, snap, pruneRemote); err != nil {
				return result, err
			}
		}
		result.DeletedIDs = append(result.DeletedIDs, snap.ID)
		result.FreedBytes += snap.SizeBytes
	}
	return result, nil
}

func (s *Service) resolveRetentionPolicy(
	ctx context.Context,
	databaseID string,
	opts PruneOptions,
) (int, int, bool) {
	keepLast := opts.KeepLast
	keepDays := opts.KeepDays
	pruneRemote := false
	if policy, err := s.mgr.Store().GetBackupPolicy(ctx, databaseID); err == nil {
		if keepLast <= 0 {
			keepLast = policy.KeepLast
		}
		if keepDays <= 0 {
			keepDays = policy.KeepDays
		}
		pruneRemote = policy.Remote
	}
	if keepLast <= 0 {
		keepLast = control.DefaultKeepLastSnapshots
	}
	if keepDays <= 0 {
		keepDays = control.DefaultKeepDaysSnapshots
	}
	return keepLast, keepDays, pruneRemote
}

func selectPruneCandidates(
	snaps []control.SnapshotRecord,
	branchSnapIDs map[string]struct{},
	keepLast int,
	keepDays int,
	now time.Time,
) []control.SnapshotRecord {
	kindCounts := make(map[string]int)
	var candidates []control.SnapshotRecord
	for _, snap := range snaps {
		rank := kindCounts[snap.Kind]
		kindCounts[snap.Kind]++
		if snap.Pinned {
			continue
		}
		if _, referenced := branchSnapIDs[snap.ID]; referenced {
			continue
		}
		if shouldPruneByKind(snap, rank, keepLast, keepDays, now) {
			candidates = append(candidates, snap)
		}
	}
	return candidates
}

func shouldPruneByKind(
	snap control.SnapshotRecord,
	rank int,
	scheduledKeepLast int,
	scheduledKeepDays int,
	now time.Time,
) bool {
	age := now.Sub(snap.CreatedAt)
	switch snap.Kind {
	case control.SnapshotKindScheduled:
		cutoff := time.Duration(scheduledKeepDays) * 24 * time.Hour
		return rank >= scheduledKeepLast && age > cutoff
	case control.SnapshotKindPreRestore, control.SnapshotKindPrePush:
		cutoff := time.Duration(DefaultAutoSnapshotKeepDays) * 24 * time.Hour
		return rank >= DefaultAutoSnapshotKeepLast && age > cutoff
	default:
		return false
	}
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
