package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/safeio"
)

type File struct {
	Path    string      `json:"path"`
	Content []byte      `json:"-"`
	Mode    os.FileMode `json:"mode"`
}

type Plan struct {
	Platform string `json:"platform"`
	Files    []File `json:"files"`
}

type Options struct {
	Executable string
	Home       string
	StateDir   string
	Hour       int
	Minute     int
}

func BuildPlan(options Options) (Plan, error) {
	if options.Executable == "" || options.Home == "" || options.StateDir == "" {
		return Plan{}, errors.New("executable, home, and state directory are required")
	}
	if options.Hour < 0 || options.Hour > 23 || options.Minute < 0 || options.Minute > 59 {
		options.Hour, options.Minute = randomDailyTime()
	}
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(options.Home, "Library", "LaunchAgents", "io.tooltend.reconcile.plist")
		return Plan{Platform: "launchd", Files: []File{{Path: path, Content: []byte(renderLaunchd(options)), Mode: 0o600}}}, nil
	case "linux":
		root := filepath.Join(options.Home, ".config", "systemd", "user")
		return Plan{Platform: "systemd", Files: []File{
			{Path: filepath.Join(root, "tooltend-reconcile.service"), Content: []byte(renderSystemdService(options)), Mode: 0o600},
			{Path: filepath.Join(root, "tooltend-reconcile.timer"), Content: []byte(renderSystemdTimer(options)), Mode: 0o600},
		}}, nil
	default:
		return Plan{}, fmt.Errorf("daily scheduling is not supported on %s", runtime.GOOS)
	}
}

func Apply(plan Plan) error {
	for _, file := range plan.Files {
		if err := safeio.AtomicWriteFile(file.Path, file.Content, file.Mode); err != nil {
			return err
		}
	}
	return nil
}

// Activate registers an already-written schedule with the operating system.
// Keeping this separate from Apply lets callers show and confirm every file
// mutation before any external scheduler state is changed.
func Activate(ctx context.Context, schedule Plan, runner execx.Runner) error {
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	switch schedule.Platform {
	case "launchd":
		if len(schedule.Files) != 1 || filepath.Base(schedule.Files[0].Path) != "io.tooltend.reconcile.plist" {
			return errors.New("scheduler: invalid launchd plan")
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		// bootout is intentionally best-effort: a first installation has no
		// existing job, while a repair must replace an already loaded plist.
		_, _ = runner.Run(ctx, "launchctl", "bootout", domain, schedule.Files[0].Path)
		if _, err := runner.Run(ctx, "launchctl", "bootstrap", domain, schedule.Files[0].Path); err != nil {
			return fmt.Errorf("scheduler: activate launchd job: %w", err)
		}
		return nil
	case "systemd":
		if len(schedule.Files) != 2 {
			return errors.New("scheduler: invalid systemd plan")
		}
		if _, err := runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("scheduler: reload systemd user units: %w", err)
		}
		if _, err := runner.Run(ctx, "systemctl", "--user", "enable", "--now", "tooltend-reconcile.timer"); err != nil {
			return fmt.Errorf("scheduler: activate systemd timer: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("scheduler: unsupported plan platform %q", schedule.Platform)
	}
}

func randomDailyTime() (int, int) {
	var value uint16
	if err := binary.Read(rand.Reader, binary.LittleEndian, &value); err != nil {
		return 3, 17
	}
	minutes := int(value) % (24 * 60)
	return minutes / 60, minutes % 60
}

func renderLaunchd(options Options) string {
	args := []string{options.Executable, "reconcile", "--once", "--state-dir", options.StateDir, "--json"}
	var program strings.Builder
	for _, arg := range args {
		program.WriteString("      <string>")
		program.WriteString(xmlEscape(arg))
		program.WriteString("</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>io.tooltend.reconcile</string>
  <key>ProgramArguments</key>
  <array>
` + program.String() + `  </array>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key><integer>` + strconv.Itoa(options.Hour) + `</integer>
    <key>Minute</key><integer>` + strconv.Itoa(options.Minute) + `</integer>
  </dict>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>StandardOutPath</key><string>/dev/null</string>
  <key>StandardErrorPath</key><string>/dev/null</string>
</dict>
</plist>
`
}

func renderSystemdService(options Options) string {
	return `[Unit]
Description=ToolTend one-shot reconciliation

[Service]
Type=oneshot
ExecStart=` + systemdQuote(options.Executable) + ` reconcile --once --state-dir ` + systemdQuote(options.StateDir) + ` --json
`
}

func renderSystemdTimer(options Options) string {
	return `[Unit]
Description=Run ToolTend reconciliation daily

[Timer]
OnCalendar=*-*-* ` + fmt.Sprintf("%02d:%02d:00", options.Hour, options.Minute) + `
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
`
}

func systemdQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func xmlEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(value)
}
