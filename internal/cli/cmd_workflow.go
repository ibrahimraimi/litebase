package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ibrahimraimi/litebase/internal/client"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/ibrahimraimi/litebase/internal/migrate"
	"github.com/spf13/cobra"
)

func newInitCommand(flags *RuntimeFlags) *cobra.Command {
	var projectName string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize litebase.toml, ./migrations/, and a local SQLite database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return executeInit(cmd, flags.WorkDir, projectName)
		},
	}
	cmd.Flags().StringVar(&projectName, "project", "", "Project name (defaults to directory name)")
	return cmd
}

func executeInit(cmd *cobra.Command, workDir string, projectName string) error {
	if projectName == "" {
		absDir, _ := filepath.Abs(workDir)
		projectName = strings.ToLower(filepath.Base(absDir))
	}
	cfg := DefaultProjectConfig(projectName)
	if err := SaveProjectConfig(workDir, cfg); err != nil {
		return err
	}
	migrationsPath := filepath.Join(workDir, cfg.Migrations.Dir)
	if err := os.MkdirAll(migrationsPath, 0o750); err != nil {
		return fmt.Errorf("create migrations directory: %w", err)
	}
	localDBPath := filepath.Join(workDir, DefaultLocalDBRelPath)
	localDB, err := engine.OpenDatabase(cmd.Context(), engine.NewSQLiteEngine(), localDBPath, engine.Options{})
	if err != nil {
		return err
	}
	_ = localDB.Close(cmd.Context())
	fmt.Fprintf(cmd.OutOrStdout(), "Initialized Litebase project %q in %s\n", cfg.Project.Name, workDir)
	return nil
}

func newLinkCommand(flags *RuntimeFlags) *cobra.Command {
	var project string
	var database string
	cmd := &cobra.Command{
		Use:   "link <url>",
		Short: "Link local project to a remote Litebase server URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadProjectConfig(flags.WorkDir)
			if err != nil {
				cfg = DefaultProjectConfig(project)
			}
			if project != "" {
				cfg.Project.Name = project
			}
			if database != "" {
				cfg.Remote.Database = database
			}
			cfg.Remote.URL = strings.TrimSpace(args[0])
			if err := SaveProjectConfig(flags.WorkDir, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Linked project %q to %s (database: %s)\n",
				cfg.Project.Name, cfg.Remote.URL, cfg.Remote.Database)
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "Project name")
	cmd.Flags().StringVar(&database, "database", "", "Target database name")
	return cmd
}

func newMigrateCommand(flags *RuntimeFlags) *cobra.Command {
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage SQL schema migrations",
	}
	migrateCmd.AddCommand(newMigrateNewCommand(flags))
	migrateCmd.AddCommand(newMigrateStatusCommand(flags))
	return migrateCmd
}

func newMigrateNewCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new numbered migration file NNNN_<name>.sql",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createNewMigrationFile(cmd, flags.WorkDir, args[0])
		},
	}
}

func createNewMigrationFile(cmd *cobra.Command, workDir string, rawName string) error {
	relPath, err := createMigrationFileWithContent(workDir, rawName, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created %s\n", relPath)
	return nil
}

func createMigrationFileWithContent(workDir string, rawName string, sqlBody string) (string, error) {
	cleanSlug := strings.ToLower(strings.TrimSpace(rawName))
	if err := engine.ValidateName(cleanSlug); err != nil {
		return "", fmt.Errorf("invalid migration name %q: %w", rawName, err)
	}
	cfg, err := LoadProjectConfig(workDir)
	if err != nil {
		cfg = DefaultProjectConfig("myapp")
	}
	migDir := filepath.Join(workDir, cfg.Migrations.Dir)
	if err := os.MkdirAll(migDir, 0o750); err != nil {
		return "", err
	}
	existing, err := migrate.LoadDirectoryMigrations(migDir)
	if err != nil {
		return "", err
	}
	nextSeq := computeNextMigrationSequence(existing)
	filename := fmt.Sprintf("%04d_%s.sql", nextSeq, cleanSlug)
	fullPath := filepath.Join(migDir, filename)
	content := fmt.Sprintf("-- Migration: %s\n\n%s", filename, sqlBody)
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write migration file %q: %w", fullPath, err)
	}
	return filepath.Join(cfg.Migrations.Dir, filename), nil
}

func computeNextMigrationSequence(existing []migrate.MigrationInput) int {
	maxVersion := 0
	for _, mig := range existing {
		if n, err := strconv.Atoi(mig.Version); err == nil && n > maxVersion {
			maxVersion = n
		}
	}
	return maxVersion + 1
}

func newMigrateStatusCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Compare local migration files against remote applied migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrateStatus(cmd, *flags)
		},
	}
}

func runMigrateStatus(cmd *cobra.Command, flags RuntimeFlags) error {
	apiClient, dbName, cfg, err := ResolveRemoteTarget(flags)
	if err != nil {
		return err
	}
	localList, err := migrate.LoadDirectoryMigrations(filepath.Join(flags.WorkDir, cfg.Migrations.Dir))
	if err != nil {
		return err
	}
	appliedList, err := apiClient.ListAppliedMigrations(cmd.Context(), dbName)
	if err != nil {
		return err
	}
	appliedMap := make(map[string]string, len(appliedList))
	for _, rec := range appliedList {
		appliedMap[rec.Version] = rec.Checksum
	}
	for _, loc := range localList {
		state := "PENDING"
		if remoteSum, ok := appliedMap[loc.Version]; ok {
			state = "APPLIED"
			if remoteSum != migrate.ComputeChecksum(loc.SQL) {
				state = "CHECKSUM_MISMATCH"
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", loc.Version, state, loc.Name)
	}
	return nil
}

func newDeployCommand(flags *RuntimeFlags) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Apply pending local migrations to the remote database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dryRun {
				return executeDeployDryRun(cmd, *flags)
			}
			return executeDeployApply(cmd, *flags)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List pending migrations without applying them")
	return cmd
}

func executeDeployDryRun(cmd *cobra.Command, flags RuntimeFlags) error {
	apiClient, dbName, localMigrations, err := prepareDeployInputs(flags)
	if err != nil {
		return err
	}
	outcome, err := apiClient.DryRunMigrations(cmd.Context(), dbName, localMigrations)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Dry-run pending migrations for %q: %v\n", dbName, outcome.AppliedVersions)
	return nil
}

func executeDeployApply(cmd *cobra.Command, flags RuntimeFlags) error {
	apiClient, dbName, localMigrations, err := prepareDeployInputs(flags)
	if err != nil {
		return err
	}
	outcome, err := apiClient.ApplyMigrations(cmd.Context(), dbName, localMigrations)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Applied %d migration(s) to %q: %v\n",
		len(outcome.AppliedVersions), dbName, outcome.AppliedVersions)
	return nil
}

func prepareDeployInputs(flags RuntimeFlags) (*client.Client, string, []migrate.MigrationInput, error) {
	apiClient, dbName, cfg, err := ResolveRemoteTarget(flags)
	if err != nil {
		return nil, "", nil, err
	}
	localMigrations, err := migrate.LoadDirectoryMigrations(filepath.Join(flags.WorkDir, cfg.Migrations.Dir))
	if err != nil {
		return nil, "", nil, err
	}
	return apiClient, dbName, localMigrations, nil
}
