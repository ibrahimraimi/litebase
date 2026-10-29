package cli

import (
	"fmt"
	"strings"

	"github.com/litebase/litebase/internal/engine"
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
	return &cobra.Command{
		Use:   "diff <branch-name>",
		Short: "Show SQL schema diff between active database and branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			stmts, err := apiClient.DiffBranch(cmd.Context(), dbName, args[0])
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
}

func newBranchMergeCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "merge <branch-name>",
		Short: "Apply schema changes from branch onto active parent database",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			merged, err := apiClient.MergeBranch(cmd.Context(), dbName, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Merged %d schema statement(s) from %q into %q\n",
				len(merged), args[0], dbName)
			return nil
		},
	}
}

