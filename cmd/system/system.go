package system

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "system",
	Short: "Manage FORGE system services (Forgejo)",
	// Cobra only auto-suggests typo'd subcommands at the root level —
	// for nested groups it silently treats unknown args as positional.
	// Wire the suggestion path back in by erroring on unknown args
	// here, falling through to help when called bare.
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		return unknownSubcommandError(cmd, args[0])
	},
	SilenceUsage: true,
}

func init() {
	Cmd.AddCommand(startCmd)
	Cmd.AddCommand(stopCmd)
	Cmd.AddCommand(statusCmd)
	Cmd.AddCommand(disconnectCmd)
}

// unknownSubcommandError formats the same "unknown command … Did you
// mean: …" message cobra emits for the root command, so typos in
// nested groups (`forge system disconnet`) get the same hint.
//
// SuggestionsFor only applies the distance default lazily inside the
// unexported findSuggestions, so we pre-set it here — otherwise every
// suggestion call against a default-constructed command returns
// nothing.
func unknownSubcommandError(cmd *cobra.Command, arg string) error {
	if cmd.SuggestionsMinimumDistance <= 0 {
		cmd.SuggestionsMinimumDistance = 2
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unknown command %q for %q", arg, cmd.CommandPath())
	if suggs := cmd.SuggestionsFor(arg); len(suggs) > 0 {
		b.WriteString("\n\nDid you mean this?")
		for _, s := range suggs {
			b.WriteString("\n\t" + s)
		}
	}
	return errors.New(b.String())
}
