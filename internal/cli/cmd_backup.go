package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ibrahimraimi/litebase/internal/backup"
	"github.com/ibrahimraimi/litebase/internal/client"
	"github.com/spf13/cobra"
)

func newBackupCommand(flags *RuntimeFlags) *cobra.Command {
	backupCmd := &cobra.Command{
		Use:   "backup",
		Short: "Manage scheduled backups, remote storage policies, and verification drills",
	}
	backupCmd.AddCommand(
		newBackupPolicyCommand(flags),
		newBackupRunCommand(flags),
		newBackupStatusCommand(flags),
		newBackupVerifyCommand(flags),
	)
	return backupCmd
}

func newBackupPolicyCommand(flags *RuntimeFlags) *cobra.Command {
	policyCmd := &cobra.Command{
		Use:   "policy",
		Short: "Configure per-database backup and retention policy",
	}
	policyCmd.AddCommand(
		newBackupPolicySetCommand(flags),
		newBackupPolicyShowCommand(flags),
		newBackupPolicyDisableCommand(flags),
	)
	return policyCmd
}

func newBackupPolicySetCommand(flags *RuntimeFlags) *cobra.Command {
	var everyStr string
	var keepLast int
	var keepDays int
	var remote bool
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set and enable a backup schedule and retention policy for the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			dur, err := time.ParseDuration(strings.TrimSpace(everyStr))
			if err != nil || dur <= 0 {
				return fmt.Errorf("invalid --every duration %q: must be a positive Go duration such as 1h or 30m", everyStr)
			}
			pol, err := apiClient.SetBackupPolicy(cmd.Context(), dbName, client.BackupPolicyParams{
				IntervalSeconds: int64(dur.Seconds()),
				KeepLast:        keepLast,
				KeepDays:        keepDays,
				Remote:          remote,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Updated backup policy for %q (every=%ds, keep_last=%d, keep_days=%d, remote=%v, enabled=%v)\n",
				dbName, pol.IntervalSeconds, pol.KeepLast, pol.KeepDays, pol.Remote, pol.Enabled,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&everyStr, "every", "1h", "Backup interval duration (e.g. 1h, 30m)")
	cmd.Flags().IntVar(&keepLast, "keep-last", 24, "Minimum number of recent scheduled snapshots to keep")
	cmd.Flags().IntVar(&keepDays, "keep-days", 14, "Retention window in days for scheduled snapshots")
	cmd.Flags().BoolVar(&remote, "remote", false, "Upload scheduled snapshots to configured remote storage")
	return cmd
}

func newBackupPolicyShowCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the backup policy for the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			pol, err := apiClient.GetBackupPolicy(cmd.Context(), dbName)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"database=%s\tenabled=%v\tevery=%ds\tkeep_last=%d\tkeep_days=%d\tremote=%v\n",
				dbName, pol.Enabled, pol.IntervalSeconds, pol.KeepLast, pol.KeepDays, pol.Remote,
			)
			return nil
		},
	}
}

func newBackupPolicyDisableCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Disable scheduled backups for the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			if err := apiClient.DisableBackupPolicy(cmd.Context(), dbName); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Disabled backup policy for %q\n", dbName)
			return nil
		},
	}
}

func newBackupRunCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Trigger an immediate backup run for the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			res, err := apiClient.RunBackup(cmd.Context(), dbName)
			if err != nil {
				return err
			}
			var snapID, remoteKey string
			if res.Snapshot != nil {
				snapID = res.Snapshot.ID
				remoteKey = res.Snapshot.RemoteKey
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backup completed for %q (snapshot=%s, remote_key=%s, pruned=%d)\n",
				res.Database, snapID, remoteKey, len(res.PrunedIDs))
			return nil
		},
	}
}

func newBackupStatusCommand(flags *RuntimeFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show backup status for the active database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiClient, dbName, _, err := ResolveRemoteTarget(*flags)
			if err != nil {
				return err
			}
			st, err := apiClient.GetBackupStatus(cmd.Context(), dbName)
			if err != nil {
				return err
			}
			lastSuccess := "never"
			if st.LastSuccessAt != nil {
				lastSuccess = st.LastSuccessAt.Format(time.RFC3339)
			}
			nextRun := "disabled"
			if st.NextRunAt != nil {
				nextRun = st.NextRunAt.Format(time.RFC3339)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"database=%s\tlast_success=%s\tnext_run=%s\tpending_remote=%d\tlast_error=%s\n",
				st.Database, lastSuccess, nextRun, st.PendingRemoteCount, st.LastError,
			)
			return nil
		},
	}
}

func newBackupVerifyCommand(flags *RuntimeFlags) *cobra.Command {
	var dbOverride string
	var remote bool
	var samples []string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Run a non-destructive restore drill on the latest local or remote backup",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedFlags := *flags
			if strings.TrimSpace(dbOverride) != "" {
				resolvedFlags.Database = strings.TrimSpace(dbOverride)
			}
			apiClient, dbName, _, err := ResolveRemoteTarget(resolvedFlags)
			if err != nil {
				return err
			}
			res, err := apiClient.VerifyBackup(cmd.Context(), dbName, client.VerifyBackupParams{
				Remote:       remote,
				SampleTables: samples,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Verified %s backup for %q (integrity_ok=%v, foreign_keys_ok=%v, sha256=%s, migrations=%d)\n",
				res.Source, res.Database, res.IntegrityOK, res.ForeignKeysOK, res.SHA256, len(res.MigrationVersions),
			)
			for tbl, cnt := range res.TableRowCounts {
				fmt.Fprintf(cmd.OutOrStdout(), "  table %s: %d row(s)\n", tbl, cnt)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dbOverride, "db", "", "Target database name override")
	cmd.Flags().BoolVar(&remote, "remote", false, "Verify the latest remote backup instead of local snapshot")
	cmd.Flags().StringSliceVar(&samples, "sample", nil, "Comma-separated table names to count rows during verification")
	return cmd
}

func newRestoreControlCommand() *cobra.Command {
	var fromRemote string
	var dataDir string
	cmd := &cobra.Command{
		Use:   "restore-control",
		Short: "Restore control.db from remote backup storage before starting the server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(fromRemote) == "" {
				return errors.New("--from-remote <key|latest> is required")
			}
			blobStore, err := backup.LoadBlobStoreFromEnv()
			if err != nil {
				return err
			}
			if blobStore == nil {
				return backup.ErrRemoteNotConfigured
			}
			meta, err := backup.RestoreControlFromRemote(
				cmd.Context(),
				dataDir,
				blobStore,
				strings.TrimSpace(fromRemote),
				backup.DefaultMaxDownloadBytes,
			)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Restored control.db to %s from remote key %s (sha256=%s)\n",
				dataDir, meta.DataKey, meta.SHA256,
			)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromRemote, "from-remote", "latest", "Remote control.db object key or 'latest'")
	cmd.Flags().StringVar(&dataDir, "data-dir", "./litebase-data", "Data directory where control.db will be restored")
	return cmd
}
