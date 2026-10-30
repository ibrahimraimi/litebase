package config

import (
	"testing"
)

func TestSaveAndLoadProjectConfig(t *testing.T) {
	if Version != "0.0.2-beta" {
		t.Fatalf("expected Version 0.0.2-beta, got %q", Version)
	}
	workDir := t.TempDir()
	cfg := DefaultProjectConfig("billing-service")
	cfg.Remote.URL = "https://db.example.com"
	cfg.Remote.Database = "billing"

	if err := Save(workDir, cfg); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := Load(workDir)
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if loaded.Project.Name != "billing-service" {
		t.Fatalf("expected project name %q, got %q", "billing-service", loaded.Project.Name)
	}
	if loaded.Remote.URL != "https://db.example.com" {
		t.Fatalf("expected remote URL %q, got %q", "https://db.example.com", loaded.Remote.URL)
	}
	if loaded.Remote.Database != "billing" {
		t.Fatalf("expected database %q, got %q", "billing", loaded.Remote.Database)
	}
	if loaded.Migrations.Dir != DefaultMigrationsDir {
		t.Fatalf("expected default migrations dir %q, got %q", DefaultMigrationsDir, loaded.Migrations.Dir)
	}
}
