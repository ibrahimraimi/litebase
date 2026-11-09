package backup

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
)

const (
	DefaultSchedulerTickInterval  = 30 * time.Second
	DefaultSchedulerMaxConcurrent = 2
	DefaultBackoffBase            = 30 * time.Second
	maxJitterFraction             = 0.10
)

type SchedulerConfig struct {
	DBManager     *manager.Manager
	Snapshots     *snapshot.Service
	BackupManager *Manager
	MaxConcurrent int
	TickInterval  time.Duration
	NowFunc       func() time.Time
	JitterFunc    func(databaseID string) float64
	Logger        *slog.Logger
}

type BackupRunResult struct {
	Database   string                  `json:"database"`
	Skipped    bool                    `json:"skipped"`
	Snapshot   *control.SnapshotRecord `json:"snapshot,omitempty"`
	RemoteMeta *RemoteSnapshotMeta     `json:"remote_meta,omitempty"`
	PrunedIDs  []string                `json:"pruned_ids,omitempty"`
	DurationMS float64                 `json:"duration_ms"`
}

type BackupStatus struct {
	Database           string                      `json:"database"`
	Policy             *control.BackupPolicyRecord `json:"policy,omitempty"`
	LastRunAt          *time.Time                  `json:"last_run_at,omitempty"`
	LastSuccessAt      *time.Time                  `json:"last_success_at,omitempty"`
	LastError          string                      `json:"last_error,omitempty"`
	NextRunAt          *time.Time                  `json:"next_run_at,omitempty"`
	PendingRemoteCount int                         `json:"pending_remote_count"`
}

type DatabaseBackupMetrics struct {
	Database             string
	LastSuccessTimestamp int64
	LastDurationSeconds  float64
	FailuresTotal        uint64
	SnapshotsByKind      map[string]int
	SnapshotBytesTotal   int64
}

type Scheduler struct {
	mgr          *manager.Manager
	snapshots    *snapshot.Service
	backupMgr    *Manager
	sem          chan struct{}
	tickInterval time.Duration
	nowFunc      func() time.Time
	jitterFunc   func(databaseID string) float64
	logger       *slog.Logger

	mu               sync.Mutex
	hasCompletedRun  map[string]bool
	lastCommitCounts map[string]uint64
	failureCounts    map[string]int
	failuresTotal    map[string]uint64
	lastDurationSec  map[string]float64
	jitterFraction   map[string]float64

	startOnce sync.Once
	stopOnce  sync.Once
	stopCh    chan struct{}
	inFlight  sync.WaitGroup
}

func NewScheduler(cfg SchedulerConfig) *Scheduler {
	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = DefaultSchedulerMaxConcurrent
	}
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = DefaultSchedulerTickInterval
	}
	nowFn := cfg.NowFunc
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	jitterFn := cfg.JitterFunc
	if jitterFn == nil {
		jitterFn = func(_ string) float64 {
			return rand.Float64() * maxJitterFraction
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		mgr:              cfg.DBManager,
		snapshots:        cfg.Snapshots,
		backupMgr:        cfg.BackupManager,
		sem:              make(chan struct{}, maxConc),
		tickInterval:     tick,
		nowFunc:          nowFn,
		jitterFunc:       jitterFn,
		logger:           logger,
		hasCompletedRun:  make(map[string]bool),
		lastCommitCounts: make(map[string]uint64),
		failureCounts:    make(map[string]int),
		failuresTotal:    make(map[string]uint64),
		lastDurationSec:  make(map[string]float64),
		jitterFraction:   make(map[string]float64),
		stopCh:           make(chan struct{}),
	}
}

func (s *Scheduler) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		s.inFlight.Add(1)
		go s.runLoop(ctx)
	})
}

func (s *Scheduler) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	waitCh := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scheduler) runLoop(ctx context.Context) {
	defer s.inFlight.Done()
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

func (s *Scheduler) Tick(ctx context.Context) {
	policies, err := s.mgr.Store().ListBackupPolicies(ctx)
	if err != nil {
		return
	}
	now := s.nowFunc()
	var duePolicies []control.BackupPolicyRecord
	for _, pol := range policies {
		if pol.Enabled && s.isPolicyDue(pol, now) {
			duePolicies = append(duePolicies, pol)
		}
	}
	if len(duePolicies) == 0 {
		return
	}

	var tickWG sync.WaitGroup
	ranRemote := false
	var remoteMu sync.Mutex

	for _, pol := range duePolicies {
		polCopy := pol
		tickWG.Add(1)
		s.inFlight.Add(1)
		go func() {
			defer tickWG.Done()
			defer s.inFlight.Done()
			select {
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			}
			didRemote := s.executeScheduledPolicy(ctx, polCopy)
			if didRemote {
				remoteMu.Lock()
				ranRemote = true
				remoteMu.Unlock()
			}
		}()
	}
	tickWG.Wait()

	if ranRemote && s.backupMgr != nil && s.backupMgr.BlobStore() != nil {
		_, _ = s.backupMgr.BackupControlDB(ctx)
	}
}

func (s *Scheduler) isPolicyDue(pol control.BackupPolicyRecord, now time.Time) bool {
	if pol.LastRunAt == nil {
		return true
	}
	interval := time.Duration(pol.IntervalSeconds) * time.Second
	s.mu.Lock()
	failCount := s.failureCounts[pol.DatabaseID]
	jitterFrac, hasJitter := s.jitterFraction[pol.DatabaseID]
	if !hasJitter {
		jitterFrac = s.jitterFunc(pol.DatabaseID)
		if jitterFrac < 0 {
			jitterFrac = 0
		}
		if jitterFrac > maxJitterFraction {
			jitterFrac = maxJitterFraction
		}
		s.jitterFraction[pol.DatabaseID] = jitterFrac
	}
	s.mu.Unlock()

	if failCount > 0 {
		backoff := computeBackoff(interval, failCount)
		return !now.Before(pol.LastRunAt.Add(backoff))
	}
	jitter := time.Duration(float64(interval) * jitterFrac)
	return !now.Before(pol.LastRunAt.Add(interval + jitter))
}

func computeBackoff(interval time.Duration, failureCount int) time.Duration {
	shift := failureCount - 1
	if shift > 16 {
		shift = 16
	}
	backoff := DefaultBackoffBase * time.Duration(1<<shift)
	if backoff > interval {
		return interval
	}
	return backoff
}

func (s *Scheduler) executeScheduledPolicy(ctx context.Context, pol control.BackupPolicyRecord) bool {
	dbRec, err := s.mgr.Store().GetDatabaseByID(ctx, pol.DatabaseID)
	if err != nil {
		return false
	}
	commits, isRestoring := s.mgr.TotalCommitsSinceStart(dbRec.Name)
	if isRestoring {
		return false
	}

	pendingRemote, _ := s.mgr.Store().ListPendingRemoteSnapshots(ctx, dbRec.ID)
	s.mu.Lock()
	seenBefore := s.hasCompletedRun[dbRec.ID]
	lastCommits := s.lastCommitCounts[dbRec.ID]
	s.mu.Unlock()

	if seenBefore && commits == lastCommits && (!pol.Remote || len(pendingRemote) == 0) {
		now := s.nowFunc()
		_ = s.mgr.Store().UpdateBackupPolicyRunState(ctx, dbRec.ID, now, &now, "")
		s.logger.Info("backup run completed",
			"db", dbRec.Name,
			"kind", control.SnapshotKindScheduled,
			"duration_ms", 0.0,
			"size_bytes", 0,
			"outcome", "skipped_idle",
		)
		return false
	}

	_, err = s.runDatabaseBackup(ctx, dbRec, &pol, commits)
	if errors.Is(err, manager.ErrDatabaseRestoring) {
		return false
	}
	return err == nil && pol.Remote
}

func (s *Scheduler) RunNow(ctx context.Context, dbName string) (BackupRunResult, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return BackupRunResult{}, err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return BackupRunResult{}, err
	}
	var polPtr *control.BackupPolicyRecord
	if pol, pErr := s.mgr.Store().GetBackupPolicy(ctx, dbRec.ID); pErr == nil {
		polPtr = &pol
	}
	commits, isRestoring := s.mgr.TotalCommitsSinceStart(dbRec.Name)
	if isRestoring {
		return BackupRunResult{}, manager.ErrDatabaseRestoring
	}
	res, err := s.runDatabaseBackup(ctx, dbRec, polPtr, commits)
	if err != nil {
		return BackupRunResult{}, err
	}
	if polPtr != nil && polPtr.Remote && s.backupMgr != nil && s.backupMgr.BlobStore() != nil {
		_, _ = s.backupMgr.BackupControlDB(ctx)
	}
	return res, nil
}

func (s *Scheduler) runDatabaseBackup(
	ctx context.Context,
	dbRec control.DatabaseRecord,
	pol *control.BackupPolicyRecord,
	commitsBefore uint64,
) (BackupRunResult, error) {
	started := s.nowFunc()
	wantRemote := pol != nil && pol.Remote
	initialRemoteStatus := control.RemoteStatusNone
	if wantRemote {
		initialRemoteStatus = control.RemoteStatusPending
	}

	snapRec, err := s.snapshots.CreateWithOptions(ctx, dbRec.Name, snapshot.CreateOptions{
		Label:        "scheduled",
		Kind:         control.SnapshotKindScheduled,
		RemoteStatus: initialRemoteStatus,
	})
	if err != nil {
		if !errors.Is(err, manager.ErrDatabaseRestoring) {
			s.recordBackupFailure(ctx, dbRec, started, err)
		}
		return BackupRunResult{}, err
	}

	var uploadedMeta *RemoteSnapshotMeta
	if wantRemote {
		meta, upErr := s.uploadPendingAndCurrent(ctx, dbRec, snapRec)
		if upErr != nil {
			s.recordBackupFailure(ctx, dbRec, started, upErr)
			return BackupRunResult{}, upErr
		}
		uploadedMeta = &meta
		updatedSnap, gErr := s.mgr.Store().GetSnapshotRecord(ctx, dbRec.ID, snapRec.ID)
		if gErr == nil {
			snapRec = updatedSnap
		}
		_, _ = s.backupMgr.GarbageCollectOrphans(ctx, dbRec.Name, s.nowFunc())
	}

	pruneRes, _ := s.snapshots.Prune(ctx, dbRec.Name, snapshot.PruneOptions{Now: s.nowFunc()})
	finished := s.nowFunc()
	durSec := finished.Sub(started).Seconds()
	if durSec < 0 {
		durSec = 0
	}
	s.recordBackupSuccess(ctx, dbRec, finished, durSec, commitsBefore, snapRec.SizeBytes)

	return BackupRunResult{
		Database:   dbRec.Name,
		Snapshot:   &snapRec,
		RemoteMeta: uploadedMeta,
		PrunedIDs:  pruneRes.DeletedIDs,
		DurationMS: durSec * 1000,
	}, nil
}

func (s *Scheduler) uploadPendingAndCurrent(
	ctx context.Context,
	dbRec control.DatabaseRecord,
	currentSnap control.SnapshotRecord,
) (RemoteSnapshotMeta, error) {
	if s.backupMgr == nil {
		return RemoteSnapshotMeta{}, ErrRemoteNotConfigured
	}
	pending, err := s.mgr.Store().ListPendingRemoteSnapshots(ctx, dbRec.ID)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}
	var currentMeta RemoteSnapshotMeta
	uploadedCurrent := false
	for _, p := range pending {
		meta, uErr := s.backupMgr.UploadSnapshot(ctx, dbRec.Name, p)
		if uErr != nil {
			return RemoteSnapshotMeta{}, uErr
		}
		if p.ID == currentSnap.ID {
			currentMeta = meta
			uploadedCurrent = true
		}
	}
	if !uploadedCurrent {
		return s.backupMgr.UploadSnapshot(ctx, dbRec.Name, currentSnap)
	}
	return currentMeta, nil
}

func (s *Scheduler) recordBackupSuccess(
	ctx context.Context,
	dbRec control.DatabaseRecord,
	finished time.Time,
	durationSec float64,
	commits uint64,
	sizeBytes int64,
) {
	s.mu.Lock()
	s.hasCompletedRun[dbRec.ID] = true
	s.lastCommitCounts[dbRec.ID] = commits
	s.failureCounts[dbRec.ID] = 0
	s.lastDurationSec[dbRec.Name] = durationSec
	delete(s.jitterFraction, dbRec.ID)
	s.mu.Unlock()

	_ = s.mgr.Store().UpdateBackupPolicyRunState(ctx, dbRec.ID, finished, &finished, "")
	s.logger.Info("backup run completed",
		"db", dbRec.Name,
		"kind", control.SnapshotKindScheduled,
		"duration_ms", durationSec*1000,
		"size_bytes", sizeBytes,
		"outcome", "success",
	)
}

func (s *Scheduler) recordBackupFailure(
	ctx context.Context,
	dbRec control.DatabaseRecord,
	runAt time.Time,
	runErr error,
) {
	s.mu.Lock()
	s.failureCounts[dbRec.ID]++
	s.failuresTotal[dbRec.Name]++
	s.mu.Unlock()

	_ = s.mgr.Store().UpdateBackupPolicyRunState(ctx, dbRec.ID, runAt, nil, runErr.Error())
	s.logger.Warn("backup run failed",
		"db", dbRec.Name,
		"kind", control.SnapshotKindScheduled,
		"outcome", "failed",
		"error", runErr.Error(),
	)
}

func (s *Scheduler) GetStatus(ctx context.Context, dbName string) (BackupStatus, error) {
	if err := engine.ValidateName(dbName); err != nil {
		return BackupStatus{}, err
	}
	dbRec, err := s.mgr.Store().GetDatabaseByName(ctx, control.DefaultProjectID, dbName)
	if err != nil {
		return BackupStatus{}, err
	}
	pending, _ := s.mgr.Store().ListPendingRemoteSnapshots(ctx, dbRec.ID)
	status := BackupStatus{
		Database:           dbRec.Name,
		PendingRemoteCount: len(pending),
	}
	pol, err := s.mgr.Store().GetBackupPolicy(ctx, dbRec.ID)
	if err != nil {
		if errors.Is(err, control.ErrNotFound) {
			return status, nil
		}
		return BackupStatus{}, err
	}
	status.Policy = &pol
	status.LastRunAt = pol.LastRunAt
	status.LastSuccessAt = pol.LastSuccessAt
	status.LastError = pol.LastError
	if pol.Enabled {
		next := s.computeNextRunAt(pol)
		status.NextRunAt = &next
	}
	return status, nil
}

func (s *Scheduler) computeNextRunAt(pol control.BackupPolicyRecord) time.Time {
	if pol.LastRunAt == nil {
		return s.nowFunc()
	}
	interval := time.Duration(pol.IntervalSeconds) * time.Second
	s.mu.Lock()
	failCount := s.failureCounts[pol.DatabaseID]
	s.mu.Unlock()
	if failCount > 0 {
		return pol.LastRunAt.Add(computeBackoff(interval, failCount))
	}
	return pol.LastRunAt.Add(interval)
}

func (s *Scheduler) CollectMetrics(ctx context.Context) ([]DatabaseBackupMetrics, uint64, error) {
	dbs, err := s.mgr.Store().ListDatabases(ctx, control.DefaultProjectID)
	if err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	failuresCopy := make(map[string]uint64, len(s.failuresTotal))
	for k, v := range s.failuresTotal {
		failuresCopy[k] = v
	}
	durationsCopy := make(map[string]float64, len(s.lastDurationSec))
	for k, v := range s.lastDurationSec {
		durationsCopy[k] = v
	}
	s.mu.Unlock()

	metrics := make([]DatabaseBackupMetrics, 0, len(dbs))
	for _, dbRec := range dbs {
		item := DatabaseBackupMetrics{
			Database:            dbRec.Name,
			LastDurationSeconds: durationsCopy[dbRec.Name],
			FailuresTotal:       failuresCopy[dbRec.Name],
			SnapshotsByKind: map[string]int{
				control.SnapshotKindManual:     0,
				control.SnapshotKindScheduled:  0,
				control.SnapshotKindPreRestore: 0,
				control.SnapshotKindPrePush:    0,
				control.SnapshotKindBranch:     0,
			},
		}
		if pol, pErr := s.mgr.Store().GetBackupPolicy(ctx, dbRec.ID); pErr == nil && pol.LastSuccessAt != nil {
			item.LastSuccessTimestamp = pol.LastSuccessAt.Unix()
		}
		if snaps, sErr := s.mgr.Store().ListSnapshotRecords(ctx, dbRec.ID); sErr == nil {
			for _, sn := range snaps {
				item.SnapshotsByKind[sn.Kind]++
				item.SnapshotBytesTotal += sn.SizeBytes
			}
		}
		metrics = append(metrics, item)
	}
	var uploadBytes uint64
	if s.backupMgr != nil {
		uploadBytes = s.backupMgr.UploadBytesTotal()
	}
	return metrics, uploadBytes, nil
}
