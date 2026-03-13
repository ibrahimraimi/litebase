package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start local Litebase server",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Starting local Litebase server...")
		// Future implementation: spawn data-plane locally
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
}
