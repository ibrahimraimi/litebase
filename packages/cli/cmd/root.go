package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "litebase",
	Short: "Litebase CLI for managing SQLite infrastructure",
	Long:  `Litebase is a control plane and infrastructure layer for SQLite.`,
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
