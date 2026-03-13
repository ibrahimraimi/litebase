package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Deploy database to remote host",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Deploying local database schema mapped to remote... (Stub)")
	},
}

func init() {
	rootCmd.AddCommand(deployCmd)
}
