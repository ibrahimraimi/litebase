package cli

import (
	"github.com/spf13/cobra"
)

func NewRootCommand() *cobra.Command {
	flags := &RuntimeFlags{
		WorkDir: ".",
	}
	rootCmd := &cobra.Command{
		Use:           "litebase",
		Short:         "Developer-first control plane for SQLite",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	rootCmd.PersistentFlags().StringVar(&flags.WorkDir, "work-dir", ".", "Project working directory")
	rootCmd.PersistentFlags().StringVar(&flags.URL, "url", "", "Override remote Litebase server URL")
	rootCmd.PersistentFlags().StringVar(&flags.Token, "token", "", "Override Litebase API token")
	rootCmd.PersistentFlags().StringVar(&flags.Database, "database", "", "Override target database name")

	rootCmd.AddCommand(
		newServeCommand(),
		newDevCommand(flags),
		newInitCommand(flags),
		newLinkCommand(flags),
		newMigrateCommand(flags),
		newDeployCommand(flags),
		newQueryCommand(flags),
		newShellCommand(flags),
		newDatabaseCommand(flags),
		newSnapshotCommand(flags),
		newBranchCommand(flags),
		newPushCommand(flags),
		newPullCommand(flags),
	)
	return rootCmd
}
