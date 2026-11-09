package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type AtomicReplaceHooks struct {
	SyncFile func(path string) error
	Rename   func(oldpath string, newpath string) error
	SyncDir  func(dirPath string) error
}

func defaultAtomicReplaceHooks() AtomicReplaceHooks {
	return AtomicReplaceHooks{
		SyncFile: SyncFileOnDisk,
		Rename:   os.Rename,
		SyncDir:  SyncParentDir,
	}
}

func AtomicReplace(srcPath string, dstPath string) error {
	return AtomicReplaceWithHooks(srcPath, dstPath, defaultAtomicReplaceHooks())
}

func AtomicReplaceWithHooks(srcPath string, dstPath string, hooks AtomicReplaceHooks) error {
	resolved := resolveAtomicReplaceHooks(hooks)
	if err := resolved.SyncFile(srcPath); err != nil {
		return fmt.Errorf("fsync source file %q before rename: %w", srcPath, err)
	}
	if err := resolved.Rename(srcPath, dstPath); err != nil {
		return fmt.Errorf("rename %q to %q: %w", srcPath, dstPath, err)
	}
	parentDir := filepath.Dir(dstPath)
	if err := resolved.SyncDir(parentDir); err != nil {
		return fmt.Errorf("fsync parent directory %q after rename: %w", parentDir, err)
	}
	return nil
}

func resolveAtomicReplaceHooks(hooks AtomicReplaceHooks) AtomicReplaceHooks {
	defaults := defaultAtomicReplaceHooks()
	if hooks.SyncFile == nil {
		hooks.SyncFile = defaults.SyncFile
	}
	if hooks.Rename == nil {
		hooks.Rename = defaults.Rename
	}
	if hooks.SyncDir == nil {
		hooks.SyncDir = defaults.SyncDir
	}
	return hooks
}

func SyncFileOnDisk(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func SyncParentDir(dirPath string) error {
	dir, err := os.Open(dirPath)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func ComputeFileSHA256(filePath string) (string, int64, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	hasher := sha256.New()
	written, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), written, nil
}

