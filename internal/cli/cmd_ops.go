package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ibrahimraimi/litebase/internal/auth"
	"github.com/ibrahimraimi/litebase/internal/client"
	"github.com/ibrahimraimi/litebase/internal/engine"
	"github.com/spf13/cobra"
)

func newDatabaseCommand(flags *RuntimeFlags) *cobra.Command {
	dbCmd := &cobra.Command{
		Use:   "db",
		Short: "Manage remote SQLite databases",
	}
	var durability string
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a remote database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, _, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			rec, err := apiClient.CreateDatabase(cmd.Context(), args[0], engine.DurabilityMode(durability))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created database %q (id=%s, durability=%s)\n",
				rec.Name, rec.ID, rec.Durability)
			return nil
		},
	}
	createCmd.Flags().StringVar(&durability, "durability", "normal", "Durability mode (normal|full)")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List remote databases",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, _, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			dbs, err := apiClient.ListDatabases(cmd.Context())
			if err != nil {
				return err
			}
			for _, rec := range dbs {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d bytes\n", rec.Name, rec.Durability, rec.SizeBytes)
			}
			return nil
		},
	}
	dbCmd.AddCommand(createCmd, listCmd)
	return dbCmd
}

func newTokenCommand(flags *RuntimeFlags) *cobra.Command {
	tokCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API bearer tokens (create, list, revoke)",
	}
	tokCmd.AddCommand(
		newTokenCreateCommand(flags),
		newTokenListCommand(flags),
		newTokenRevokeCommand(flags),
	)
	return tokCmd
}

func newTokenCreateCommand(flags *RuntimeFlags) *cobra.Command {
	var scopes []string
	var expiresInSec int64
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Issue a new API bearer token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			parsedScopes := make([]auth.Scope, 0, len(scopes))
			for _, sc := range scopes {
				parsedScopes = append(parsedScopes, auth.Scope(strings.TrimSpace(sc)))
			}
			targetDB := dbName
			if len(parsedScopes) == 1 && parsedScopes[0] == auth.ScopeAdmin {
				targetDB = ""
			}
			created, err := apiClient.CreateToken(cmd.Context(), targetDB, parsedScopes, expiresInSec)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created token %s (prefix=%s): %s\n", created.ID, created.Prefix, created.Token)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&scopes, "scope", []string{"read", "write"}, "Token scopes (read, write, admin)")
	cmd.Flags().Int64Var(&expiresInSec, "expires-in", 0, "Expiration in seconds (0 for non-expiring)")
	return cmd
}

func newTokenListCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, _, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			tokens, err := apiClient.ListTokens(cmd.Context())
			if err != nil {
				return err
			}
			for _, tok := range tokens {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%v\t%s\n",
					tok.ID, tok.Prefix, tok.Scopes, tok.CreatedAt.Format("2006-01-02T15:04:05Z"))
			}
			return nil
		},
	}
}

func newTokenRevokeCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke an API token by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, _, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			if err := apiClient.RevokeToken(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked token %s\n", args[0])
			return nil
		},
	}
}

func newSnapshotCommand(flags *RuntimeFlags) *cobra.Command {
	snapCmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Create, list, and restore consistent database snapshots",
	}
	snapCmd.AddCommand(
		newSnapshotCreateCommand(flags),
		newSnapshotListCommand(flags),
		newSnapshotRestoreCommand(flags),
	)
	return snapCmd
}

func newSnapshotCreateCommand(flags *RuntimeFlags) *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a consistent snapshot via VACUUM INTO",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			rec, err := apiClient.CreateSnapshot(cmd.Context(), dbName, label)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created snapshot %s (label=%s, size=%d bytes)\n",
				rec.ID, rec.Label, rec.SizeBytes)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "manual", "Snapshot label")
	return cmd
}

func newSnapshotListCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List snapshots for the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			snaps, err := apiClient.ListSnapshots(cmd.Context(), dbName)
			if err != nil {
				return err
			}
			for _, snap := range snaps {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d bytes\t%s\n",
					snap.ID, snap.Label, snap.SizeBytes, snap.CreatedAt.Format("2006-01-02T15:04:05Z"))
			}
			return nil
		},
	}
}

func newSnapshotRestoreCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <snapshot-id>",
		Short: "Restore the active database from a snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			restored, err := apiClient.RestoreSnapshot(cmd.Context(), dbName, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restored %q to snapshot %s\n", dbName, restored.ID)
			return nil
		},
	}
}

func newBranchCommand(flags *RuntimeFlags) *cobra.Command {
	branchCmd := &cobra.Command{
		Use:   "branch",
		Short: "Manage Git-like database branches (create, list, delete, checkout, diff, merge)",
	}
	branchCmd.AddCommand(
		newBranchCreateCommand(flags),
		newBranchListCommand(flags),
		newBranchDeleteCommand(flags),
		newBranchCheckoutCommand(flags),
		newBranchDiffCommand(flags),
		newBranchMergeCommand(flags),
	)
	return branchCmd
}

func newBranchCreateCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "create <branch-name>",
		Short: "Create an isolated branch copy of the active database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			rec, err := apiClient.CreateBranch(cmd.Context(), dbName, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created branch %q from %q (id=%s)\n", rec.Name, dbName, rec.ID)
			return nil
		},
	}
}

func newBranchListCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List branches of the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			branches, err := apiClient.ListBranches(cmd.Context(), dbName)
			if err != nil {
				return err
			}
			for _, br := range branches {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", br.Name, br.CreatedAt.Format("2006-01-02T15:04:05Z"))
			}
			return nil
		},
	}
}

func newBranchDeleteCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <branch-name>",
		Short: "Delete a branch database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			if err := apiClient.DeleteBranch(cmd.Context(), dbName, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted branch %q\n", args[0])
			return nil
		},
	}
}

func newBranchCheckoutCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "checkout <database-or-branch>",
		Short: "Switch the active target database/branch in litebase.toml",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := strings.TrimSpace(args[0])
			if err := engine.ValidateName(target); err != nil {
				return err
			}
			cfg, err := LoadProjectConfig(flags.WorkDir)
			if err != nil {
				return err
			}
			cfg.Remote.Database = target
			if err := SaveProjectConfig(flags.WorkDir, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Switched active database to %q\n", target)
			return nil
		},
	}
}

func newBranchDiffCommand(flags *RuntimeFlags) *cobra.Command {
	var allowDrop bool
	cmd := &cobra.Command{
		Use:   "diff <branch-name>",
		Short: "Show SQL schema diff between active database and branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			stmts, err := apiClient.DiffBranchWithOptions(cmd.Context(), dbName, args[0], allowDrop)
			if err != nil {
				return err
			}
			if len(stmts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "-- No schema differences")
				return nil
			}
			for _, sqlLine := range stmts {
				fmt.Fprintln(cmd.OutOrStdout(), sqlLine)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowDrop, "allow-drop", false, "Allow emitting DROP TABLE statements for tables dropped on the branch")
	return cmd
}

func newBranchMergeCommand(flags *RuntimeFlags) *cobra.Command {
	var allowDrop bool
	var asMigration bool
	cmd := &cobra.Command{
		Use:   "merge <branch-name>",
		Short: "Apply schema changes from branch onto active parent database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			if asMigration {
				return writeBranchDiffAsMigration(cmd, apiClient, flags.WorkDir, dbName, args[0], allowDrop)
			}
			merged, err := apiClient.MergeBranchWithOptions(cmd.Context(), dbName, args[0], allowDrop)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Merged %d schema statement(s) from %q into %q\n",
				len(merged), args[0], dbName)
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowDrop, "allow-drop", false, "Allow dropping tables that were removed on the branch")
	cmd.Flags().BoolVar(&asMigration, "as-migration", false, "Write the schema diff to a numbered migration file instead of executing it directly")
	return cmd
}

func writeBranchDiffAsMigration(
	cmd *cobra.Command,
	apiClient *client.Client,
	workDir string,
	dbName string,
	branchName string,
	allowDrop bool,
) error {
	stmts, err := apiClient.DiffBranchWithOptions(cmd.Context(), dbName, branchName, allowDrop)
	if err != nil {
		return err
	}
	if len(stmts) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "-- No schema differences")
		return nil
	}
	body := strings.Join(stmts, "\n") + "\n"
	relPath, err := createMigrationFileWithContent(workDir, "merge_"+branchName, body)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created %s with %d statement(s)\n", relPath, len(stmts))
	return nil
}

func newPushCommand(flags *RuntimeFlags) *cobra.Command {
	var force bool
	var filePath string
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Upload and overwrite the remote SQLite database file (requires --force)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !force {
				return errors.New("refusing to overwrite remote database without --force; use 'litebase deploy' for schema migrations or pass --force")
			}
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			sourcePath := resolveLocalFilePath(flags.WorkDir, filePath)
			rec, err := apiClient.UploadDatabase(cmd.Context(), dbName, sourcePath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pushed %s to remote database %q (size=%d bytes)\n",
				sourcePath, rec.Name, rec.SizeBytes)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Confirm overwriting the remote database file")
	cmd.Flags().StringVar(&filePath, "file", "", "Path to local SQLite file (defaults to .litebase/local.sqlite)")
	return cmd
}

func newPullCommand(flags *RuntimeFlags) *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Download a consistent copy of the remote SQLite database to a local file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			destPath := resolveLocalFilePath(flags.WorkDir, filePath)
			writtenBytes, err := apiClient.DownloadDatabase(cmd.Context(), dbName, destPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pulled remote database %q to %s (%d bytes)\n",
				dbName, destPath, writtenBytes)
			return nil
		},
	}
	cmd.Flags().StringVar(&filePath, "file", "", "Destination SQLite file path (defaults to .litebase/local.sqlite)")
	return cmd
}

func resolveLocalFilePath(workDir string, overridePath string) string {
	trimmed := strings.TrimSpace(overridePath)
	if trimmed != "" {
		return trimmed
	}
	return filepath.Join(workDir, DefaultLocalDBRelPath)
}
