package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate a user with Litebase cloud",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("Opening browser to authenticate... (Stub)")
	},
}

func init() {
	rootCmd.AddCommand(loginCmd)
}
