package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var snapshotCmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Create and restore database snapshots",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Snapshot commands... (Stub)")
	},
}

func init() {
	rootCmd.AddCommand(snapshotCmd)
}
