package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/litebase/litebase/internal/client"
	"github.com/litebase/litebase/internal/config"
)

const (
	ConfigFileName        = config.ConfigFileName
	DefaultMigrationsDir  = config.DefaultMigrationsDir
	DefaultLocalDBRelPath = config.DefaultLocalDBRelPath
	TokenEnvVarName       = config.TokenEnvVarName
	BootstrapTokenEnvVar  = config.BootstrapTokenEnvVar
)

type ProjectConfig = config.ProjectConfig

type RuntimeFlags struct {
	WorkDir  string
	URL      string
	Token    string
	Database string
}

func DefaultProjectConfig(projectName string) ProjectConfig {
	return config.DefaultProjectConfig(projectName)
}

func LoadProjectConfig(workDir string) (ProjectConfig, error) {
	return config.Load(workDir)
}

func SaveProjectConfig(workDir string, cfg ProjectConfig) error {
	return config.Save(workDir, cfg)
}

func ResolveRemoteTarget(flags RuntimeFlags) (*client.Client, string, ProjectConfig, error) {
	cfg, cfgErr := config.Load(flags.WorkDir)
	if cfgErr != nil {
		cfg = config.DefaultProjectConfig("myapp")
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
