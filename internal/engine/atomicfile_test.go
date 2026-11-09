package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicReplaceSuccessAndInjectedSyncFailures(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source.tmp")
	dst := filepath.Join(dir, "final.sqlite")

	if err := os.WriteFile(src, []byte("sqlite-bytes"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	injectedSyncErr := errors.New("injected file fsync error")
	err := AtomicReplaceWithHooks(src, dst, AtomicReplaceHooks{
		SyncFile: func(_ string) error {
			return injectedSyncErr
		},
	})
	if !errors.Is(err, injectedSyncErr) {
		t.Fatalf("expected injected file fsync error, got %v", err)
	}
	if _, statErr := os.Stat(dst); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("expected dst not to exist when pre-rename SyncFile fails")
	}

	injectedDirSyncErr := errors.New("injected directory fsync error")
	err = AtomicReplaceWithHooks(src, dst, AtomicReplaceHooks{
		SyncDir: func(_ string) error {
			return injectedDirSyncErr
		},
	})
	if !errors.Is(err, injectedDirSyncErr) {
		t.Fatalf("expected injected dir fsync error, got %v", err)
	}

	src2 := filepath.Join(dir, "source2.tmp")
	dst2 := filepath.Join(dir, "final2.sqlite")
	if err := os.WriteFile(src2, []byte("verified-payload"), 0o600); err != nil {
		t.Fatalf("write src2: %v", err)
	}
	if err := AtomicReplace(src2, dst2); err != nil {
		t.Fatalf("AtomicReplace failed: %v", err)
	}
	if _, statErr := os.Stat(src2); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("expected src2 to be moved by AtomicReplace")
	}
	gotBytes, err := os.ReadFile(dst2)
	if err != nil || string(gotBytes) != "verified-payload" {
		t.Fatalf("unexpected dst2 contents: %q, err=%v", string(gotBytes), err)
	}
}

