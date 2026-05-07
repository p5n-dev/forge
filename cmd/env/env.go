package env

import "github.com/spf13/cobra"

var Cmd = &cobra.Command{
	Use:   "env",
	Short: "Manage FORGE environments",
}

func init() {
	Cmd.AddCommand(listCmd)
}
