package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/notify"
)

func (a *App) newNotifierCommand() *cobra.Command {
	command := &cobra.Command{
		Use:         "__notifier <install|send> [arguments...]",
		Hidden:      true,
		Annotations: map[string]string{internalDriverAnnotation: "true"},
		Args:        cobra.MinimumNArgs(1),
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "install":
			if len(args) != 1 {
				return fmt.Errorf("notifier install accepts no arguments")
			}
			result, err := notify.InstallDarwin(cmd.Context(), a.home, a.runner)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(a.out, "Installed ToolTend Notifier to %s\n", result.AppPath)
			return err
		case "send":
			if len(args) != 3 {
				return fmt.Errorf("notifier send requires title and message")
			}
			return a.desktopNotifier().Send(cmd.Context(), args[1], args[2])
		default:
			return fmt.Errorf("unsupported notifier action %q on %s", args[0], runtime.GOOS)
		}
	}
	return command
}
