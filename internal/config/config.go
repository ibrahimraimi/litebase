package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

var Version = "0.0.1-beta"

const (
	ConfigFileName        = "litebase.toml"
	DefaultMigrationsDir  = "migrations"
	DefaultLocalDBRelPath = ".litebase/local.sqlite"
	TokenEnvVarName       = "LITEBASE_TOKEN"
	BootstrapTokenEnvVar  = "LITEBASE_BOOTSTRAP_TOKEN"
)

type ProjectConfig struct {
	Project    ProjectSection    `toml:"project"`
	Remote     RemoteSection     `toml:"remote"`
	Migrations MigrationsSection `toml:"migrations"`
}

type ProjectSection struct {
	Name string `toml:"name"`
}

type RemoteSection struct {
	URL      string `toml:"url"`
	Database string `toml:"database"`
}

type MigrationsSection struct {
	Dir string `toml:"dir"`
}

func DefaultProjectConfig(projectName string) ProjectConfig {
	cleanName := strings.TrimSpace(projectName)
	if cleanName == "" {
		cleanName = "myapp"
	}
	return ProjectConfig{
		Project: ProjectSection{Name: cleanName},
		Remote: RemoteSection{
			URL:      "http://127.0.0.1:8080",
			Database: "main",
		},
		Migrations: MigrationsSection{
			Dir: DefaultMigrationsDir,
		},
	}
}

func Load(workDir string) (ProjectConfig, error) {
	configPath := filepath.Join(workDir, ConfigFileName)
	var cfg ProjectConfig
	if _, err := toml.DecodeFile(configPath, &cfg); err != nil {
		return ProjectConfig{}, fmt.Errorf("read %s: %w; run 'litebase init' first", configPath, err)
	}
	if cfg.Migrations.Dir == "" {
		cfg.Migrations.Dir = DefaultMigrationsDir
	}
	if cfg.Remote.Database == "" {
		cfg.Remote.Database = "main"
	}
	return cfg, nil
}

func Save(workDir string, cfg ProjectConfig) error {
	configPath := filepath.Join(workDir, ConfigFileName)
	var buf bytes.Buffer
	buf.WriteString("# Litebase project configuration\n")
	buf.WriteString("# Set LITEBASE_TOKEN in your environment; never commit tokens to version control.\n\n")
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encode %s: %w", ConfigFileName, err)
	}
	return os.WriteFile(configPath, buf.Bytes(), 0o644)
}

