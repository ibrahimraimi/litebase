package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ibrahimraimi/litebase/internal/config"
	"github.com/ibrahimraimi/litebase/internal/control"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/manager"
	"github.com/ibrahimraimi/litebase/internal/snapshot"
	"github.com/ibrahimraimi/litebase/internal/testhook"
)

type CompressionMode string

const (
	CompressionGzip CompressionMode = "gzip"
	CompressionNone CompressionMode = "none"

	ControlBackupDBName        = "_control"
	DefaultMaxDownloadBytes    = 4 * 1024 * 1024 * 1024
	DefaultOrphanGCThreshold   = 24 * time.Hour
	defaultUploadMaxRetries    = 3
	defaultUploadRetryBaseWait = 20 * time.Millisecond
)

var (
	ErrRemoteBackupNotFound = errors.New("remote backup not found")
	ErrDownloadTooLarge     = errors.New("remote backup exceeds maximum allowed download size")
)

type RemoteSnapshotMeta struct {
	SnapshotID        string    `json:"snapshot_id,omitempty"`
	Database          string    `json:"database"`
	Label             string    `json:"label"`
	Kind              string    `json:"kind"`
	DataKey           string    `json:"data_key"`
	MetaKey           string    `json:"meta_key"`
	Compression       string    `json:"compression"`
	SHA256            string    `json:"sha256"`
	SizeBytes         int64     `json:"size_bytes"`
	CompressedBytes   int64     `json:"compressed_bytes"`
	LitebaseVersion   string    `json:"litebase_version"`
	MigrationVersions []string  `json:"migration_versions,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

func ParseCompressionMode(raw string) (CompressionMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "gzip":
		return CompressionGzip, nil
	case "none":
		return CompressionNone, nil
	default:
		return "", fmt.Errorf("invalid backup compression mode %q: expected 'gzip' or 'none'", raw)
	}
}

type Manager struct {
	mgr              *manager.Manager
	snapshots        *snapshot.Service
	blobStore        BlobStore
	compression      CompressionMode
	maxDownloadBytes int64
	retryBaseWait    time.Duration
	uploadBytesTotal atomic.Uint64
}

type ManagerConfig struct {
	DBManager        *manager.Manager
	Snapshots        *snapshot.Service
	BlobStore        BlobStore
	Compression      CompressionMode
	MaxDownloadBytes int64
	RetryBaseWait    time.Duration
}

func NewManager(cfg ManagerConfig) *Manager {
	comp := cfg.Compression
	if comp == "" {
		comp = CompressionGzip
	}
	maxDL := cfg.MaxDownloadBytes
	if maxDL <= 0 {
		maxDL = DefaultMaxDownloadBytes
	}
	retryWait := cfg.RetryBaseWait
	if retryWait <= 0 {
		retryWait = defaultUploadRetryBaseWait
	}
	bm := &Manager{
		mgr:              cfg.DBManager,
		snapshots:        cfg.Snapshots,
		blobStore:        cfg.BlobStore,
		compression:      comp,
		maxDownloadBytes: maxDL,
		retryBaseWait:    retryWait,
	}
	if cfg.Snapshots != nil {
		cfg.Snapshots.SetRemoteDeleter(bm.DeleteRemoteSnapshot)
	}
	return bm
}

func (bm *Manager) BlobStore() BlobStore {
	return bm.blobStore
}

func (bm *Manager) SetBlobStore(store BlobStore) {
	bm.blobStore = store
}

func (bm *Manager) UploadBytesTotal() uint64 {
	return bm.uploadBytesTotal.Load()
}

func (bm *Manager) UploadSnapshot(
	ctx context.Context,
	dbName string,
	snap control.SnapshotRecord,
) (RemoteSnapshotMeta, error) {
	if bm.blobStore == nil {
		return RemoteSnapshotMeta{}, ErrRemoteNotConfigured
	}
	sha256Hex, err := bm.snapshots.EnsureChecksum(ctx, snap)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}
	migVersions := bm.readMigrationVersions(ctx, snap.DatabaseID)
	dataKey, metaKey := buildObjectKeys(dbName, snap.CreatedAt, snap.Label, bm.compression)

	_ = bm.mgr.Store().UpdateSnapshotRemoteState(ctx, snap.ID, dataKey, control.RemoteStatusPending, nil)

	compressedBytes, err := bm.streamUploadWithRetry(ctx, snap.Path, dataKey, map[string]string{
		"sha256":   sha256Hex,
		"database": dbName,
	})
	if err != nil {
		_ = bm.mgr.Store().UpdateSnapshotRemoteState(ctx, snap.ID, dataKey, control.RemoteStatusFailed, nil)
		return RemoteSnapshotMeta{}, err
	}

	testhook.At("backup.after-data-upload")

	meta := RemoteSnapshotMeta{
		SnapshotID:        snap.ID,
		Database:          dbName,
		Label:             snap.Label,
		Kind:              snap.Kind,
		DataKey:           dataKey,
		MetaKey:           metaKey,
		Compression:       string(bm.compression),
		SHA256:            sha256Hex,
		SizeBytes:         snap.SizeBytes,
		CompressedBytes:   compressedBytes,
		LitebaseVersion:   config.Version,
		MigrationVersions: migVersions,
		CreatedAt:         snap.CreatedAt.UTC(),
	}
	if err := bm.putMetaJSONWithRetry(ctx, metaKey, meta); err != nil {
		_ = bm.mgr.Store().UpdateSnapshotRemoteState(ctx, snap.ID, dataKey, control.RemoteStatusFailed, nil)
		return RemoteSnapshotMeta{}, err
	}

	now := time.Now().UTC()
	if err := bm.mgr.Store().UpdateSnapshotRemoteState(ctx, snap.ID, dataKey, control.RemoteStatusUploaded, &now); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	bm.uploadBytesTotal.Add(uint64(compressedBytes))
	return meta, nil
}

func (bm *Manager) BackupControlDB(ctx context.Context) (RemoteSnapshotMeta, error) {
	if bm.blobStore == nil {
		return RemoteSnapshotMeta{}, ErrRemoteNotConfigured
	}
	controlPath := bm.mgr.Store().ControlPath()
	tmpDir := filepath.Join(bm.mgr.DataDir(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	tmpSnap := filepath.Join(tmpDir, fmt.Sprintf("control-%d.sqlite.tmp", time.Now().UTC().UnixNano()))
	defer os.Remove(tmpSnap)

	if err := vacuumControlDatabase(ctx, controlPath, tmpSnap); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	if err := engine.SyncFileOnDisk(tmpSnap); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	if err := engine.CheckFileIntegrity(ctx, tmpSnap); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	sha256Hex, sizeBytes, err := engine.ComputeFileSHA256(tmpSnap)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}

	now := time.Now().UTC()
	dataKey, metaKey := buildObjectKeys(ControlBackupDBName, now, "control", bm.compression)
	compressedBytes, err := bm.streamUploadWithRetry(ctx, tmpSnap, dataKey, map[string]string{
		"sha256":   sha256Hex,
		"database": ControlBackupDBName,
	})
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}
	meta := RemoteSnapshotMeta{
		Database:        ControlBackupDBName,
		Label:           "control",
		Kind:            control.SnapshotKindScheduled,
		DataKey:         dataKey,
		MetaKey:         metaKey,
		Compression:     string(bm.compression),
		SHA256:          sha256Hex,
		SizeBytes:       sizeBytes,
		CompressedBytes: compressedBytes,
		LitebaseVersion: config.Version,
		CreatedAt:       now,
	}
	if err := bm.putMetaJSONWithRetry(ctx, metaKey, meta); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	bm.uploadBytesTotal.Add(uint64(compressedBytes))
	return meta, nil
}

func vacuumControlDatabase(ctx context.Context, controlPath string, destPath string) error {
	_ = os.Remove(destPath)
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000", controlPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", destPath)
	return err
}

func (bm *Manager) ListRemoteSnapshots(ctx context.Context, dbName string) ([]RemoteSnapshotMeta, error) {
	if bm.blobStore == nil {
		return nil, ErrRemoteNotConfigured
	}
	if dbName != ControlBackupDBName {
		if err := engine.ValidateName(dbName); err != nil {
			return nil, err
		}
	}
	blobs, err := bm.blobStore.List(ctx, dbName)
	if err != nil {
		return nil, err
	}
	blobMap := make(map[string]BlobInfo, len(blobs))
	for _, b := range blobs {
		blobMap[b.Key] = b
	}

	var results []RemoteSnapshotMeta
	for _, b := range blobs {
		if !strings.HasSuffix(b.Key, ".meta.json") {
			continue
		}
		meta, err := bm.readRemoteMeta(ctx, b.Key)
		if err != nil {
			continue
		}
		if _, dataExists := blobMap[meta.DataKey]; !dataExists {
			continue
		}
		results = append(results, meta)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].CreatedAt.Equal(results[j].CreatedAt) {
			return results[i].DataKey > results[j].DataKey
		}
		return results[i].CreatedAt.After(results[j].CreatedAt)
	})
	return results, nil
}

func (bm *Manager) GarbageCollectOrphans(ctx context.Context, dbName string, now time.Time) ([]string, error) {
	if bm.blobStore == nil {
		return nil, ErrRemoteNotConfigured
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	blobs, err := bm.blobStore.List(ctx, dbName)
	if err != nil {
		return nil, err
	}
	metaKeys := make(map[string]struct{})
	for _, b := range blobs {
		if strings.HasSuffix(b.Key, ".meta.json") {
			metaKeys[b.Key] = struct{}{}
		}
	}

	var deleted []string
	for _, b := range blobs {
		if strings.HasSuffix(b.Key, ".meta.json") {
			continue
		}
		expectedMeta := deriveMetaKeyFromDataKey(b.Key)
		if _, hasMeta := metaKeys[expectedMeta]; hasMeta {
			continue
		}
		if now.Sub(b.LastModified) >= DefaultOrphanGCThreshold {
			if err := bm.blobStore.Delete(ctx, b.Key); err == nil {
				deleted = append(deleted, b.Key)
			}
		}
	}
	return deleted, nil
}

func (bm *Manager) DeleteRemoteSnapshot(ctx context.Context, dataOrMetaKey string) error {
	if bm.blobStore == nil {
		return nil
	}
	if err := ValidateObjectKey(dataOrMetaKey); err != nil {
		return err
	}
	metaKey := deriveMetaKeyFromDataKey(dataOrMetaKey)
	_ = bm.blobStore.Delete(ctx, metaKey)
	if dataOrMetaKey != metaKey {
		return bm.blobStore.Delete(ctx, dataOrMetaKey)
	}
	return nil
}

func (bm *Manager) RestoreFromRemote(
	ctx context.Context,
	dbName string,
	keyOrLatest string,
	asNewDB string,
) (RemoteSnapshotMeta, error) {
	if bm.blobStore == nil {
		return RemoteSnapshotMeta{}, ErrRemoteNotConfigured
	}
	if err := engine.ValidateName(dbName); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	if asNewDB != "" {
		if err := engine.ValidateName(asNewDB); err != nil {
			return RemoteSnapshotMeta{}, err
		}
	}
	meta, err := bm.resolveRemoteSnapshotMeta(ctx, dbName, keyOrLatest)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}

	stagedFile, cleanup, err := bm.DownloadAndVerifyToTemp(ctx, meta)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}
	defer cleanup()

	targetName := dbName
	if asNewDB != "" {
		targetName = asNewDB
	}
	if err := bm.applyVerifiedRemoteFile(ctx, targetName, stagedFile, asNewDB != ""); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	return meta, nil
}

func (bm *Manager) applyVerifiedRemoteFile(
	ctx context.Context,
	targetName string,
	stagedFile string,
	isRestoreAsNew bool,
) error {
	store := bm.mgr.Store()
	existingRec, err := store.GetDatabaseByName(ctx, control.DefaultProjectID, targetName)
	if err != nil && !errors.Is(err, control.ErrNotFound) {
		return err
	}
	if errors.Is(err, control.ErrNotFound) {
		destPath, joinErr := engine.SafeJoinPath(
			bm.mgr.DataDir(),
			"databases",
			control.DefaultProjectName,
			targetName,
			"main.sqlite",
		)
		if joinErr != nil {
			return joinErr
		}
		if err := copyVerifiedFileAtomically(stagedFile, destPath); err != nil {
			return err
		}
		_, err = bm.mgr.CreateDatabaseAtPath(ctx, targetName, destPath, engine.DurabilityNormal)
		return err
	}
	if isRestoreAsNew {
		return fmt.Errorf("%w: database %q", control.ErrAlreadyExists, targetName)
	}
	if _, statErr := os.Stat(existingRec.Path); statErr == nil && bm.snapshots != nil {
		_, _ = bm.snapshots.CreateWithOptions(ctx, targetName, snapshot.CreateOptions{
			Label: "pre-restore",
			Kind:  control.SnapshotKindPreRestore,
		})
	}
	if err := bm.mgr.ReplaceDatabaseFile(ctx, targetName, stagedFile); err != nil {
		return err
	}
	if bm.snapshots != nil {
		_, _ = bm.snapshots.Prune(ctx, targetName, snapshot.PruneOptions{})
	}
	_, err = bm.mgr.RefreshDatabaseInfo(ctx, targetName)
	return err
}

func RestoreControlFromRemote(
	ctx context.Context,
	dataDir string,
	blobStore BlobStore,
	keyOrLatest string,
	maxDownloadBytes int64,
) (RemoteSnapshotMeta, error) {
	if blobStore == nil {
		return RemoteSnapshotMeta{}, ErrRemoteNotConfigured
	}
	if maxDownloadBytes <= 0 {
		maxDownloadBytes = DefaultMaxDownloadBytes
	}
	tempMgr := &Manager{
		blobStore:        blobStore,
		maxDownloadBytes: maxDownloadBytes,
	}
	meta, err := tempMgr.resolveRemoteSnapshotMeta(ctx, ControlBackupDBName, keyOrLatest)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}

	tmpDir := filepath.Join(dataDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	stagedPath := filepath.Join(tmpDir, fmt.Sprintf("control-restore-%d.sqlite", time.Now().UTC().UnixNano()))
	defer os.Remove(stagedPath)

	if err := downloadAndVerifyObject(ctx, blobStore, meta, stagedPath, maxDownloadBytes); err != nil {
		return RemoteSnapshotMeta{}, err
	}

	controlPath, err := engine.SafeJoinPath(dataDir, "control.db")
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}
	_ = os.Remove(controlPath + "-wal")
	_ = os.Remove(controlPath + "-shm")
	if err := engine.AtomicReplace(stagedPath, controlPath); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	return meta, nil
}

func (bm *Manager) DownloadAndVerifyToTemp(
	ctx context.Context,
	meta RemoteSnapshotMeta,
) (string, func(), error) {
	tmpDir := filepath.Join(bm.mgr.DataDir(), "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", nil, err
	}
	stagedPath := filepath.Join(tmpDir, fmt.Sprintf("remote-%d.sqlite", time.Now().UTC().UnixNano()))
	cleanup := func() {
		_ = os.Remove(stagedPath)
	}
	if err := downloadAndVerifyObject(ctx, bm.blobStore, meta, stagedPath, bm.maxDownloadBytes); err != nil {
		cleanup()
		return "", nil, err
	}
	return stagedPath, cleanup, nil
}

func downloadAndVerifyObject(
	ctx context.Context,
	blobStore BlobStore,
	meta RemoteSnapshotMeta,
	destPath string,
	maxDownloadBytes int64,
) error {
	if err := ValidateObjectKey(meta.DataKey); err != nil {
		return err
	}
	rc, err := blobStore.Get(ctx, meta.DataKey)
	if err != nil {
		return fmt.Errorf("download %q: %w", meta.DataKey, err)
	}
	defer rc.Close()

	var stream io.Reader = io.LimitReader(rc, maxDownloadBytes+1)
	if meta.Compression == string(CompressionGzip) || strings.HasSuffix(meta.DataKey, ".gz") {
		gzReader, gzErr := gzip.NewReader(stream)
		if gzErr != nil {
			return fmt.Errorf("open gzip stream for %q: %w", meta.DataKey, gzErr)
		}
		defer gzReader.Close()
		stream = io.LimitReader(gzReader, maxDownloadBytes+1)
	}

	outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(outFile, stream)
	syncErr := outFile.Sync()
	closeErr := outFile.Close()
	if copyErr != nil {
		return fmt.Errorf("stream remote snapshot %q: %w", meta.DataKey, copyErr)
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxDownloadBytes {
		return fmt.Errorf("%w (%d bytes)", ErrDownloadTooLarge, maxDownloadBytes)
	}

	actualSHA256, _, err := engine.ComputeFileSHA256(destPath)
	if err != nil {
		return err
	}
	if meta.SHA256 == "" || actualSHA256 != meta.SHA256 {
		return fmt.Errorf("%w: remote %q expected %s, got %s",
			snapshot.ErrSnapshotChecksumMismatch, meta.DataKey, meta.SHA256, actualSHA256)
	}
	return engine.CheckFileIntegrity(ctx, destPath)
}

func (bm *Manager) resolveRemoteSnapshotMeta(
	ctx context.Context,
	dbName string,
	keyOrLatest string,
) (RemoteSnapshotMeta, error) {
	trimmed := strings.TrimSpace(keyOrLatest)
	if trimmed == "" || trimmed == "latest" {
		list, err := bm.ListRemoteSnapshots(ctx, dbName)
		if err != nil {
			return RemoteSnapshotMeta{}, err
		}
		if len(list) == 0 {
			return RemoteSnapshotMeta{}, fmt.Errorf("%w for database %q", ErrRemoteBackupNotFound, dbName)
		}
		return list[0], nil
	}
	if err := ValidateObjectKey(trimmed); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	metaKey := deriveMetaKeyFromDataKey(trimmed)
	meta, err := bm.readRemoteMeta(ctx, metaKey)
	if err != nil {
		return RemoteSnapshotMeta{}, fmt.Errorf("%w: %v", ErrRemoteBackupNotFound, err)
	}
	return meta, nil
}

func (bm *Manager) readRemoteMeta(ctx context.Context, metaKey string) (RemoteSnapshotMeta, error) {
	rc, err := bm.blobStore.Get(ctx, metaKey)
	if err != nil {
		return RemoteSnapshotMeta{}, err
	}
	defer rc.Close()

	var meta RemoteSnapshotMeta
	if err := json.NewDecoder(io.LimitReader(rc, 1024*1024)).Decode(&meta); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	if err := ValidateObjectKey(meta.DataKey); err != nil {
		return RemoteSnapshotMeta{}, err
	}
	return meta, nil
}

func (bm *Manager) streamUploadWithRetry(
	ctx context.Context,
	localFilePath string,
	remoteKey string,
	meta map[string]string,
) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < defaultUploadMaxRetries; attempt++ {
		if attempt > 0 {
			wait := bm.retryBaseWait * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(wait):
			}
		}
		uploadedBytes, err := bm.streamUploadOnce(ctx, localFilePath, remoteKey, meta)
		if err == nil {
			return uploadedBytes, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("upload %q failed after %d attempts: %w", remoteKey, defaultUploadMaxRetries, lastErr)
}

func (bm *Manager) streamUploadOnce(
	ctx context.Context,
	localFilePath string,
	remoteKey string,
	meta map[string]string,
) (int64, error) {
	file, err := os.Open(localFilePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}

	if bm.compression == CompressionNone {
		counter := &countingReader{r: file}
		if err := bm.blobStore.Put(ctx, remoteKey, counter, info.Size(), meta); err != nil {
			return 0, err
		}
		return counter.n.Load(), nil
	}

	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		_, copyErr := io.Copy(gz, file)
		closeErr := gz.Close()
		if copyErr != nil {
			_ = pw.CloseWithError(copyErr)
			return
		}
		_ = pw.CloseWithError(closeErr)
	}()

	counter := &countingReader{r: pr}
	if err := bm.blobStore.Put(ctx, remoteKey, counter, -1, meta); err != nil {
		_ = pr.Close()
		return 0, err
	}
	return counter.n.Load(), nil
}

func (bm *Manager) putMetaJSONWithRetry(ctx context.Context, metaKey string, meta RemoteSnapshotMeta) error {
	payload, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < defaultUploadMaxRetries; attempt++ {
		if attempt > 0 {
			wait := bm.retryBaseWait * time.Duration(1<<(attempt-1))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		err := bm.blobStore.Put(ctx, metaKey, bytes.NewReader(payload), int64(len(payload)), nil)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("upload meta %q failed: %w", metaKey, lastErr)
}

func (bm *Manager) readMigrationVersions(ctx context.Context, databaseID string) []string {
	migs, err := bm.mgr.Store().ListAppliedMigrations(ctx, databaseID)
	if err != nil {
		return nil
	}
	versions := make([]string, 0, len(migs))
	for _, m := range migs {
		versions = append(versions, m.Version)
	}
	return versions
}

type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (cr *countingReader) Read(p []byte) (int, error) {
	readBytes, err := cr.r.Read(p)
	if readBytes > 0 {
		cr.n.Add(int64(readBytes))
	}
	return readBytes, err
}

func buildObjectKeys(
	dbName string,
	ts time.Time,
	label string,
	comp CompressionMode,
) (string, string) {
	utc := ts.UTC()
	year := fmt.Sprintf("%04d", utc.Year())
	month := fmt.Sprintf("%02d", int(utc.Month()))
	stem := fmt.Sprintf("%d-%s", utc.UnixNano(), label)
	ext := ".sqlite.gz"
	if comp == CompressionNone {
		ext = ".sqlite"
	}
	dataKey := path.Join(dbName, year, month, stem+ext)
	metaKey := path.Join(dbName, year, month, stem+".meta.json")
	return dataKey, metaKey
}

func deriveMetaKeyFromDataKey(key string) string {
	switch {
	case strings.HasSuffix(key, ".meta.json"):
		return key
	case strings.HasSuffix(key, ".sqlite.gz"):
		return strings.TrimSuffix(key, ".sqlite.gz") + ".meta.json"
	case strings.HasSuffix(key, ".sqlite"):
		return strings.TrimSuffix(key, ".sqlite") + ".meta.json"
	default:
		return key + ".meta.json"
	}
}

func copyVerifiedFileAtomically(srcPath string, dstPath string) error {
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return err
	}
	tmpPath := dstPath + ".restore-staging"
	src, err := os.Open(srcPath)
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
	return engine.AtomicReplace(tmpPath, dstPath)
}
