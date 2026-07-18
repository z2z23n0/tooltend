package cli

import (
	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/bundledriver"
)

const internalDriverAnnotation = "tooltend.io/internal-bundle-driver"

func (a *App) newBundleDriverCommand() *cobra.Command {
	command := &cobra.Command{
		Use:                "__bundle-driver <action> [arguments...]",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.MinimumNArgs(1),
		Annotations:        map[string]string{internalDriverAnnotation: "true"},
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		driver := bundledriver.Driver{Runner: a.runner, Out: a.out}
		return driver.Execute(cmd.Context(), args)
	}
	return command
}
