package image

import "github.com/spf13/cobra"

var Cmd = &cobra.Command{
	Use:   "image",
	Short: "Manage FORGE base images",
}

func init() {
	Cmd.AddCommand(pullCmd)
	Cmd.AddCommand(listCmd)
}
