package notify

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/z2z23n0/tooltend/internal/execx"
)

var ErrUnsupported = errors.New("desktop notifications are not supported on this platform")
var ErrNotifierUnavailable = errors.New("ToolTend Notifier is not installed")

const (
	DarwinAppName    = "ToolTend Notifier.app"
	DarwinBundleID   = "io.tooltend.notifier.native"
	DarwinExecutable = "applet"
)

type Desktop struct {
	GOOS    string
	AppPath string
	Runner  execx.Runner
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
		path := strings.TrimSpace(d.AppPath)
		if path == "" || !filepath.IsAbs(path) {
			return ErrNotifierUnavailable
		}
		result, err := runner.Run(ctx, path, title, message)
		if err == nil {
			return nil
		}
		detail := strings.TrimSpace(string(result.Stderr))
		if detail == "" {
			return fmt.Errorf("desktop notification: %w", err)
		}
		return fmt.Errorf("desktop notification: %s: %w", detail, err)
	case "linux":
		_, err := runner.Run(ctx, "notify-send", "--app-name=ToolTend", title, message)
		return err
	default:
		return ErrUnsupported
	}
}

func DarwinAppPath(home string) string {
	return filepath.Join(home, "Applications", DarwinAppName)
}

func DarwinNotifierExecutable(home string) string {
	return filepath.Join(DarwinAppPath(home), "Contents", "MacOS", DarwinExecutable)
}
