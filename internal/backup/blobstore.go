package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ibrahimraimi/litebase/internal/engine"
)

var (
	ErrInvalidObjectKey    = errors.New("invalid remote object key")
	ErrRemoteNotConfigured = errors.New("remote backup storage is not configured")
)

type BlobInfo struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	LastModified time.Time         `json:"last_modified"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, meta map[string]string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]BlobInfo, error)
	Delete(ctx context.Context, key string) error
}

func ValidateObjectKey(key string) error {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" || len(trimmed) > 512 {
		return fmt.Errorf("%w %q: key must be 1-512 characters", ErrInvalidObjectKey, key)
	}
	if strings.ContainsRune(trimmed, 0) || strings.Contains(trimmed, `\`) {
		return fmt.Errorf("%w %q: backslashes and null bytes are not allowed", ErrInvalidObjectKey, key)
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasSuffix(trimmed, "/") {
		return fmt.Errorf("%w %q: leading or trailing slash is not allowed", ErrInvalidObjectKey, key)
	}
	parts := strings.Split(trimmed, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w %q: empty or traversal segment %q is not allowed", ErrInvalidObjectKey, key, part)
		}
	}
	return nil
}

type LocalDirStore struct {
	rootDir string
	prefix  string
}

func NewLocalDirStore(rootDir string, prefix string) (*LocalDirStore, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, errors.New("local blob store rootDir cannot be empty")
	}
	absRoot, err := filepath.Abs(filepath.Clean(rootDir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absRoot, 0o750); err != nil {
		return nil, err
	}
	cleanPrefix := strings.Trim(strings.TrimSpace(prefix), "/")
	return &LocalDirStore{
		rootDir: absRoot,
		prefix:  cleanPrefix,
	}, nil
}

func (l *LocalDirStore) fullKey(key string) string {
	if l.prefix == "" {
		return key
	}
	return path.Join(l.prefix, key)
}

func (l *LocalDirStore) stripPrefix(full string) (string, bool) {
	if l.prefix == "" {
		return full, true
	}
	withSlash := l.prefix + "/"
	if !strings.HasPrefix(full, withSlash) {
		return "", false
	}
	return strings.TrimPrefix(full, withSlash), true
}

func (l *LocalDirStore) resolveKeyPath(key string) (string, error) {
	if err := ValidateObjectKey(key); err != nil {
		return "", err
	}
	full := l.fullKey(key)
	segments := strings.Split(full, "/")
	return engine.SafeJoinPath(l.rootDir, segments...)
}

func (l *LocalDirStore) Put(_ context.Context, key string, r io.Reader, _ int64, meta map[string]string) error {
	targetPath, err := l.resolveKeyPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
		return err
	}
	tmpPath := targetPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return closeErr
	}
	if err := engine.AtomicReplace(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if len(meta) > 0 {
		if encoded, mErr := json.Marshal(meta); mErr == nil {
			_ = os.WriteFile(targetPath+".xmeta", encoded, 0o600)
		}
	}
	return nil
}

func (l *LocalDirStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	targetPath, err := l.resolveKeyPath(key)
	if err != nil {
		return nil, err
	}
	return os.Open(targetPath)
}

func (l *LocalDirStore) List(_ context.Context, prefix string) ([]BlobInfo, error) {
	cleanPrefix := strings.Trim(strings.TrimSpace(prefix), "/")
	if cleanPrefix != "" {
		if err := ValidateObjectKey(cleanPrefix); err != nil {
			return nil, err
		}
	}
	var items []BlobInfo
	err := filepath.WalkDir(l.rootDir, func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".xmeta") {
			return nil
		}
		rel, err := filepath.Rel(l.rootDir, filePath)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		logicalKey, ok := l.stripPrefix(relSlash)
		if !ok {
			return nil
		}
		if cleanPrefix != "" && !strings.HasPrefix(logicalKey, cleanPrefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		items = append(items, BlobInfo{
			Key:          logicalKey,
			Size:         info.Size(),
			LastModified: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].Key < items[j].Key
	})
	return items, nil
}

func (l *LocalDirStore) Delete(_ context.Context, key string) error {
	targetPath, err := l.resolveKeyPath(key)
	if err != nil {
		return err
	}
	_ = os.Remove(targetPath + ".xmeta")
	if err := os.Remove(targetPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
