package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/litebase/litebase/internal/control"
	"github.com/litebase/litebase/internal/engine"
)

type ManagedEntry struct {
	Record     control.DatabaseRecord
	DB         *engine.Database
	lastUsedNS atomic.Int64
	inFlight   atomic.Int32
}

func (me *ManagedEntry) Touch() {
	me.lastUsedNS.Store(time.Now().UnixNano())
}

func (me *ManagedEntry) LastUsed() time.Time {
	return time.Unix(0, me.lastUsedNS.Load())
}

type Manager struct {
	dataDir       string
	store         *control.Store
	sqlEngine     engine.Engine
	defaultOpts   engine.Options
	maxDBSizeByte int64

	mu      sync.Mutex
	openDBs map[string]*ManagedEntry
	closed  bool
}

type Config struct {
	DataDir        string
	Store          *control.Store
	Engine         engine.Engine
	DefaultOptions engine.Options
	MaxDBSizeBytes int64
}

func NewManager(cfg Config) (*Manager, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("initialize database manager: DataDir is empty")
	}
	if cfg.Store == nil {
		return nil, errors.New("initialize database manager: Store is nil")
	}
	sqlEng := cfg.Engine
	if sqlEng == nil {
		sqlEng = engine.NewSQLiteEngine()
	}
	return &Manager{
		dataDir:       cfg.DataDir,
		store:         cfg.Store,
		sqlEngine:     sqlEng,
		defaultOpts:   cfg.DefaultOptions,
		maxDBSizeByte: cfg.MaxDBSizeBytes,
		openDBs:       make(map[string]*ManagedEntry),
	}, nil
}

func (m *Manager) DataDir() string {
	return m.dataDir
}

func (m *Manager) Store() *control.Store {
	return m.store
}

func (m *Manager) CreateDatabase(
	ctx context.Context,
	name string,
	durability engine.DurabilityMode,
) (control.DatabaseRecord, error) {
	if err := engine.ValidateName(name); err != nil {
		return control.DatabaseRecord{}, err
	}
	dbPath, err := engine.SafeJoinPath(m.dataDir, "databases", control.DefaultProjectName, name, "main.sqlite")
	if err != nil {
		return control.DatabaseRecord{}, err
	}
	return m.CreateDatabaseAtPath(ctx, name, dbPath, durability)
}

func (m *Manager) CreateDatabaseAtPath(
	ctx context.Context,
	name string,
	dbPath string,
	durability engine.DurabilityMode,
) (control.DatabaseRecord, error) {
	if err := engine.ValidateName(name); err != nil {
		return control.DatabaseRecord{}, err
	}
	if durability == "" {
		durability = engine.DurabilityNormal
	}
	_, err := m.store.CreateDatabase(ctx, control.CreateDatabaseParams{
		ProjectID:  control.DefaultProjectID,
		Name:       name,
		Path:       dbPath,
		Durability: durability,
	})
	if err != nil {
		return control.DatabaseRecord{}, err
	}
	entry, err := m.Acquire(ctx, name)
	if err != nil {
		return control.DatabaseRecord{}, err
	}
	m.Release(entry)
	return m.RefreshDatabaseInfo(ctx, name)
}

func (m *Manager) Acquire(ctx context.Context, name string) (*ManagedEntry, error) {
	if err := engine.ValidateName(name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, engine.ErrDatabaseClosed
	}
	if existing, ok := m.openDBs[name]; ok {
		existing.inFlight.Add(1)
		existing.Touch()
		return existing, nil
	}
	record, err := m.store.GetDatabaseByName(ctx, control.DefaultProjectID, name)
	if err != nil {
		return nil, err
	}
	opts := m.defaultOpts
	opts.Durability = record.Durability
	openedDB, err := engine.OpenDatabase(ctx, m.sqlEngine, record.Path, opts)
	if err != nil {
		return nil, err
	}
	entry := &ManagedEntry{
		Record: record,
		DB:     openedDB,
	}
	entry.inFlight.Store(1)
	entry.Touch()
	m.openDBs[name] = entry
	return entry, nil
}

func (m *Manager) Release(entry *ManagedEntry) {
	if entry == nil {
		return
	}
	entry.Touch()
	entry.inFlight.Add(-1)
}

func (m *Manager) CheckStorageQuota(record control.DatabaseRecord) error {
	if m.maxDBSizeByte <= 0 {
		return nil
	}
	currentSize := measureSQLiteFileBytes(record.Path)
	if currentSize >= m.maxDBSizeByte {
		return fmt.Errorf("database %q size (%d bytes) exceeds maximum allowed limit (%d bytes)", record.Name, currentSize, m.maxDBSizeByte)
	}
	return nil
}

func (m *Manager) RefreshDatabaseInfo(ctx context.Context, name string) (control.DatabaseRecord, error) {
	record, err := m.store.GetDatabaseByName(ctx, control.DefaultProjectID, name)
	if err != nil {
		return control.DatabaseRecord{}, err
	}
	sizeBytes := measureSQLiteFileBytes(record.Path)
	_ = m.store.UpdateDatabaseSize(ctx, record.ID, sizeBytes)
	record.SizeBytes = sizeBytes
	return record, nil
}

func (m *Manager) DeleteDatabase(ctx context.Context, name string) error {
	if err := engine.ValidateName(name); err != nil {
		return err
	}
	m.mu.Lock()
	entry, isOpen := m.openDBs[name]
	if isOpen {
		delete(m.openDBs, name)
	}
	m.mu.Unlock()

	var dbPath string
	if isOpen {
		dbPath = entry.Record.Path
		_ = entry.DB.Close(ctx)
	} else {
		rec, err := m.store.GetDatabaseByName(ctx, control.DefaultProjectID, name)
		if err != nil {
			return err
		}
		dbPath = rec.Path
	}
	if err := m.store.SoftDeleteDatabase(ctx, control.DefaultProjectID, name); err != nil {
		return err
	}
	removeSQLiteFiles(dbPath)
	return nil
}

func (m *Manager) ReplaceDatabaseFile(ctx context.Context, name string, snapshotFilePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return engine.ErrDatabaseClosed
	}
	record, err := m.store.GetDatabaseByName(ctx, control.DefaultProjectID, name)
	if err != nil {
		return err
	}
	if openEntry, ok := m.openDBs[name]; ok {
		delete(m.openDBs, name)
		if err := openEntry.DB.Close(ctx); err != nil {
			return fmt.Errorf("drain open database %q before restore: %w", name, err)
		}
	}
	return swapDatabaseFileOnDisk(record.Path, snapshotFilePath)
}

func (m *Manager) EvictIdle(ctx context.Context, maxIdle time.Duration) int {
	m.mu.Lock()
	var toClose []*ManagedEntry
	cutoff := time.Now().Add(-maxIdle)
	for name, entry := range m.openDBs {
		if entry.inFlight.Load() == 0 && entry.LastUsed().Before(cutoff) {
			delete(m.openDBs, name)
			toClose = append(toClose, entry)
		}
	}
	m.mu.Unlock()

	for _, entry := range toClose {
		_ = entry.DB.Close(ctx)
	}
	return len(toClose)
}

func (m *Manager) CloseAll(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	entries := make([]*ManagedEntry, 0, len(m.openDBs))
	for name, entry := range m.openDBs {
		delete(m.openDBs, name)
		entries = append(entries, entry)
	}
	m.mu.Unlock()

	var closeErrs []error
	for _, entry := range entries {
		if err := entry.DB.Close(ctx); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	return errors.Join(closeErrs...)
}

func swapDatabaseFileOnDisk(targetDBPath string, sourceSnapshotPath string) error {
	stagingPath := targetDBPath + ".restore-staging"
	if err := copyFileContents(sourceSnapshotPath, stagingPath); err != nil {
		return fmt.Errorf("stage snapshot file for restore: %w", err)
	}
	preRestorePath := targetDBPath + ".pre-restore"
	if _, err := os.Stat(targetDBPath); err == nil {
		if err := os.Rename(targetDBPath, preRestorePath); err != nil {
			_ = os.Remove(stagingPath)
			return fmt.Errorf("backup target db to .pre-restore: %w", err)
		}
	}
	_ = os.Remove(targetDBPath + "-wal")
	_ = os.Remove(targetDBPath + "-shm")
	if err := os.Rename(stagingPath, targetDBPath); err != nil {
		return fmt.Errorf("rename staged snapshot into place: %w", err)
	}
	return nil
}

func copyFileContents(srcPath string, dstPath string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = dstFile.Close()
		return err
	}
	if err := dstFile.Sync(); err != nil {
		_ = dstFile.Close()
		return err
	}
	return dstFile.Close()
}

func measureSQLiteFileBytes(dbPath string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(dbPath + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

func removeSQLiteFiles(dbPath string) {
	for _, suffix := range []string{"", "-wal", "-shm", ".pre-restore"} {
		_ = os.Remove(dbPath + suffix)
	}
}
