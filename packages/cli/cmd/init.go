package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a project",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Initializing Litebase project...")
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}
