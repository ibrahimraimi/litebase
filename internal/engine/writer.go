package engine

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type writeJob struct {
	ctx        context.Context
	statements []Statement
	maxRows    int
	outcomes   []QueryResult
	err        error
	succeeded  bool
	done       chan struct{}
}

type maintenanceJob struct {
	ctx  context.Context
	task func(context.Context, *sql.Conn) error
	done chan error
}

type WriterStats struct {
	TotalJobs        uint64
	TotalBatches     uint64
	TotalQueueFull   uint64
	CurrentQueueSize int
}

type Writer struct {
	conn         *sql.Conn
	secState     *connectionSecurityState
	jobs         chan *writeJob
	maintenance  chan *maintenanceJob
	maxBatchSize int
	batchWait    time.Duration
	maxRows      int
	commitHook   func() error

	isClosed       atomic.Bool
	acceptMu       sync.RWMutex
	workerDone     chan struct{}
	totalJobs      atomic.Uint64
	totalBatches   atomic.Uint64
	totalQueueFull atomic.Uint64
}

func NewWriter(conn *sql.Conn, secState *connectionSecurityState, opts Options) *Writer {
	resolved := opts.withDefaults()
	writer := &Writer{
		conn:         conn,
		secState:     secState,
		jobs:         make(chan *writeJob, resolved.MaxQueueDepth),
		maintenance:  make(chan *maintenanceJob),
		maxBatchSize: resolved.MaxBatchSize,
		batchWait:    resolved.BatchWait,
		maxRows:      resolved.MaxRows,
		workerDone:   make(chan struct{}),
	}
	go writer.runLoop()
	return writer
}

func (w *Writer) Submit(ctx context.Context, statements []Statement) ([]QueryResult, error) {
	if len(statements) == 0 {
		return nil, ErrEmptyStatement
	}
	job := &writeJob{
		ctx:        ctx,
		statements: statements,
		maxRows:    w.maxRows,
		done:       make(chan struct{}),
	}
	if err := w.enqueueJob(job); err != nil {
		return nil, err
	}
	<-job.done
	if job.err != nil {
		return nil, job.err
	}
	return job.outcomes, nil
}

func (w *Writer) enqueueJob(job *writeJob) error {
	w.acceptMu.RLock()
	defer w.acceptMu.RUnlock()

	if w.isClosed.Load() {
		return ErrDatabaseClosed
	}
	select {
	case w.jobs <- job:
		return nil
	default:
		w.totalQueueFull.Add(1)
		return ErrQueueFull
	}
}

func (w *Writer) RunMaintenance(ctx context.Context, task func(context.Context, *sql.Conn) error) error {
	w.acceptMu.RLock()
	if w.isClosed.Load() {
		w.acceptMu.RUnlock()
		return ErrDatabaseClosed
	}
	maint := &maintenanceJob{
		ctx:  ctx,
		task: task,
		done: make(chan error, 1),
	}
	w.maintenance <- maint
	w.acceptMu.RUnlock()

	select {
	case err := <-maint.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) Stats() WriterStats {
	return WriterStats{
		TotalJobs:        w.totalJobs.Load(),
		TotalBatches:     w.totalBatches.Load(),
		TotalQueueFull:   w.totalQueueFull.Load(),
		CurrentQueueSize: len(w.jobs),
	}
}

func (w *Writer) Close(ctx context.Context) error {
	w.acceptMu.Lock()
	if w.isClosed.Swap(true) {
		w.acceptMu.Unlock()
		<-w.workerDone
		return nil
	}
	close(w.jobs)
	close(w.maintenance)
	w.acceptMu.Unlock()

	select {
	case <-w.workerDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Writer) runLoop() {
	defer close(w.workerDone)
	for {
		select {
		case maint, ok := <-w.maintenance:
			if !ok {
				w.drainRemainingJobs()
				w.checkpointAndClose()
				return
			}
			w.executeMaintenance(maint)
		case firstJob, ok := <-w.jobs:
			if !ok {
				w.checkpointAndClose()
				return
			}
			batch := w.collectBatch(firstJob)
			w.commitBatch(batch)
		}
	}
}

func (w *Writer) drainRemainingJobs() {
	for {
		select {
		case firstJob, ok := <-w.jobs:
			if !ok {
				return
			}
			batch := w.collectBatch(firstJob)
			w.commitBatch(batch)
		default:
			return
		}
	}
}

func (w *Writer) executeMaintenance(maint *maintenanceJob) {
	if w.secState != nil {
		w.secState.isPrivileged.Store(true)
		defer w.secState.isPrivileged.Store(false)
	}
	maint.done <- maint.task(maint.ctx, w.conn)
}

func (w *Writer) checkpointAndClose() {
	if w.secState != nil {
		w.secState.isPrivileged.Store(true)
	}
	_, _ = w.conn.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	_ = w.conn.Close()
}

func (w *Writer) collectBatch(firstJob *writeJob) []*writeJob {
	batch := make([]*writeJob, 0, w.maxBatchSize)
	batch = append(batch, firstJob)
	if w.maxBatchSize <= 1 {
		return batch
	}
	if len(w.jobs) == 0 && w.batchWait > 0 {
		timer := time.NewTimer(w.batchWait)
		select {
		case secondJob, ok := <-w.jobs:
			timer.Stop()
			if !ok {
				return batch
			}
			batch = append(batch, secondJob)
		case <-timer.C:
			return batch
		}
	}
	for len(batch) < w.maxBatchSize {
		select {
		case nextJob, ok := <-w.jobs:
			if !ok {
				return batch
			}
			batch = append(batch, nextJob)
		default:
			return batch
		}
	}
	return batch
}

func (w *Writer) commitBatch(batch []*writeJob) {
	w.totalBatches.Add(1)
	w.totalJobs.Add(uint64(len(batch)))

	bgCtx := context.Background()
	if _, err := w.conn.ExecContext(bgCtx, "BEGIN IMMEDIATE"); err != nil {
		failEntireBatch(batch, normalizeSQLiteError(err))
		return
	}

	succeededCount := 0
	for idx, job := range batch {
		if w.runJobWithSavepoint(bgCtx, idx, job) {
			succeededCount++
		}
	}
	if succeededCount == 0 {
		_, _ = w.conn.ExecContext(bgCtx, "ROLLBACK")
		notifyBatchDone(batch)
		return
	}
	w.finalizeBatchCommit(bgCtx, batch)
}

func (w *Writer) runJobWithSavepoint(bgCtx context.Context, idx int, job *writeJob) bool {
	if err := job.ctx.Err(); err != nil {
		job.err = err
		return false
	}
	savepointName := fmt.Sprintf("sp_%d", idx)
	if _, err := w.conn.ExecContext(bgCtx, "SAVEPOINT "+savepointName); err != nil {
		job.err = normalizeSQLiteError(err)
		return false
	}
	outcomes, err := w.executeJobStatements(job)
	if err != nil {
		_, _ = w.conn.ExecContext(bgCtx, "ROLLBACK TO SAVEPOINT "+savepointName)
		_, _ = w.conn.ExecContext(bgCtx, "RELEASE SAVEPOINT "+savepointName)
		job.err = err
		return false
	}
	if _, err := w.conn.ExecContext(bgCtx, "RELEASE SAVEPOINT "+savepointName); err != nil {
		job.err = normalizeSQLiteError(err)
		return false
	}
	job.outcomes = outcomes
	job.succeeded = true
	return true
}

func (w *Writer) executeJobStatements(job *writeJob) ([]QueryResult, error) {
	outcomes := make([]QueryResult, 0, len(job.statements))
	for _, stmt := range job.statements {
		outcome, err := executeStatementOnConn(job.ctx, w.conn, stmt, job.maxRows)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func (w *Writer) finalizeBatchCommit(bgCtx context.Context, batch []*writeJob) {
	if w.commitHook != nil {
		if hookErr := w.commitHook(); hookErr != nil {
			_, _ = w.conn.ExecContext(bgCtx, "ROLLBACK")
			failSucceededJobs(batch, hookErr)
			notifyBatchDone(batch)
			return
		}
	}
	if _, err := w.conn.ExecContext(bgCtx, "COMMIT"); err != nil {
		_, _ = w.conn.ExecContext(bgCtx, "ROLLBACK")
		failSucceededJobs(batch, normalizeSQLiteError(err))
		notifyBatchDone(batch)
		return
	}
	notifyBatchDone(batch)
}

func failEntireBatch(batch []*writeJob, err error) {
	for _, job := range batch {
		job.err = err
		job.outcomes = nil
		close(job.done)
	}
}

func failSucceededJobs(batch []*writeJob, err error) {
	for _, job := range batch {
		if job.succeeded {
			job.succeeded = false
			job.outcomes = nil
			job.err = err
		}
	}
}

func notifyBatchDone(batch []*writeJob) {
	for _, job := range batch {
		close(job.done)
	}
}
