package cmd

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/p5n-dev/forge/cmd/env"
	"github.com/p5n-dev/forge/cmd/image"
	"github.com/p5n-dev/forge/cmd/system"
)

var debugFlag bool

var rootCmd = &cobra.Command{
	Use:   "forge",
	Short: "Federated Orchestrated Runtime Guarded Environment",
	Long: `FORGE creates isolated VM environments for running AI coding agents
with native Kubernetes support.`,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		level := zerolog.InfoLevel
		if debugFlag {
			level = zerolog.DebugLevel
		}
		log.Logger = zerolog.New(
			zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339},
		).Level(level).With().Timestamp().Logger()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable debug logging")
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(env.Cmd)
	rootCmd.AddCommand(system.Cmd)
	rootCmd.AddCommand(image.Cmd)
}
