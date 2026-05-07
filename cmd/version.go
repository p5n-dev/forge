package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the forge version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Printf("forge %s\n", version)
	},
}
