package notify

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"github.com/z2z23n0/tooltend/internal/execx"
)

var ErrUnsupported = errors.New("desktop notifications are not supported on this platform")

type Desktop struct {
	GOOS   string
	Runner execx.Runner
}

func (d Desktop) Send(ctx context.Context, title, message string) error {
	if strings.TrimSpace(title) == "" || strings.TrimSpace(message) == "" {
		return errors.New("desktop notification title and message are required")
	}
	goos := d.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	runner := d.Runner
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	switch goos {
	case "darwin":
		script := "display notification " + appleScriptString(message) + " with title " + appleScriptString(title)
		_, err := runner.Run(ctx, "/usr/bin/osascript", "-e", script)
		return err
	case "linux":
		_, err := runner.Run(ctx, "notify-send", "--app-name=ToolTend", title, message)
		return err
	default:
		return ErrUnsupported
	}
}

func appleScriptString(value string) string {
	value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\r", " ", "\n", " ").Replace(value)
	return "\"" + value + "\""
}
