package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/litebase/litebase/internal/client"
)

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

type RuntimeFlags struct {
	WorkDir  string
	URL      string
	Token    string
	Database string
}

func DefaultProjectConfig(projectName string) ProjectConfig {
	if strings.TrimSpace(projectName) == "" {
		projectName = "myapp"
	}
	return ProjectConfig{
		Project: ProjectSection{Name: projectName},
		Remote: RemoteSection{
			URL:      "http://127.0.0.1:8080",
			Database: "main",
		},
		Migrations: MigrationsSection{
			Dir: DefaultMigrationsDir,
		},
	}
}

func LoadProjectConfig(workDir string) (ProjectConfig, error) {
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

func SaveProjectConfig(workDir string, cfg ProjectConfig) error {
	configPath := filepath.Join(workDir, ConfigFileName)
	var buf bytes.Buffer
	buf.WriteString("# Litebase project configuration\n")
	buf.WriteString("# Set LITEBASE_TOKEN in your environment; never commit tokens to version control.\n\n")
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encode %s: %w", ConfigFileName, err)
	}
	return os.WriteFile(configPath, buf.Bytes(), 0o644)
}

func ResolveRemoteTarget(flags RuntimeFlags) (*client.Client, string, ProjectConfig, error) {
	cfg, cfgErr := LoadProjectConfig(flags.WorkDir)
	if cfgErr != nil {
		cfg = DefaultProjectConfig("myapp")
	}
	remoteURL := strings.TrimSpace(flags.URL)
	if remoteURL == "" {
		remoteURL = strings.TrimSpace(cfg.Remote.URL)
	}
	if remoteURL == "" {
		return nil, "", cfg, errors.New("remote URL is not configured; run 'litebase link <url>' or pass --url")
	}
	dbName := strings.TrimSpace(flags.Database)
	if dbName == "" {
		dbName = strings.TrimSpace(cfg.Remote.Database)
	}
	if dbName == "" {
		dbName = "main"
	}
	token := strings.TrimSpace(flags.Token)
	if token == "" {
		token = strings.TrimSpace(os.Getenv(TokenEnvVarName))
	}
	if token == "" {
		return nil, "", cfg, fmt.Errorf("missing API token; set %s or pass --token", TokenEnvVarName)
	}
	return client.New(remoteURL, token), dbName, cfg, nil
}

