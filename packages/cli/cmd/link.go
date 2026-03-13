package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var linkCmd = &cobra.Command{
	Use:   "link",
	Short: "Connect local project to a remote instance",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Linking local project to remote... (Stub)")
	},
}

func init() {
	rootCmd.AddCommand(linkCmd)
}
