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
	"time"

	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/safeio"
)

type File struct {
	Path    string      `json:"path"`
	Content []byte      `json:"-"`
	Mode    os.FileMode `json:"mode"`
}

type Plan struct {
	Platform    string   `json:"platform"`
	Files       []File   `json:"files"`
	Directories []string `json:"directories,omitempty"`
}

type Options struct {
	Executable string
	Home       string
	StateDir   string
	PathEnv    string
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
		root := filepath.Join(options.Home, "Library", "LaunchAgents")
		return Plan{Platform: "launchd", Directories: []string{filepath.Join(options.StateDir, "logs")}, Files: []File{
			{Path: filepath.Join(root, "io.tooltend.reconcile.plist"), Content: []byte(renderLaunchd(options)), Mode: 0o600},
			{Path: filepath.Join(root, "io.tooltend.watchdog.plist"), Content: []byte(renderLaunchdWatchdog(options)), Mode: 0o600},
		}}, nil
	case "linux":
		root := filepath.Join(options.Home, ".config", "systemd", "user")
		return Plan{Platform: "systemd", Files: []File{
			{Path: filepath.Join(root, "tooltend-reconcile.service"), Content: []byte(renderSystemdService(options)), Mode: 0o600},
			{Path: filepath.Join(root, "tooltend-reconcile.timer"), Content: []byte(renderSystemdTimer(options)), Mode: 0o600},
			{Path: filepath.Join(root, "tooltend-watchdog.service"), Content: []byte(renderSystemdWatchdogService(options)), Mode: 0o600},
			{Path: filepath.Join(root, "tooltend-watchdog.timer"), Content: []byte(renderSystemdWatchdogTimer(options)), Mode: 0o600},
		}}, nil
	default:
		return Plan{}, fmt.Errorf("daily scheduling is not supported on %s", runtime.GOOS)
	}
}

func Apply(plan Plan) error {
	for _, directory := range plan.Directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
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
		if !hasExactFiles(schedule.Files, "io.tooltend.reconcile.plist", "io.tooltend.watchdog.plist") {
			return errors.New("scheduler: invalid launchd plan")
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		for _, file := range schedule.Files {
			// bootout is intentionally best-effort: a first installation has no
			// existing job, while a repair must replace an already loaded plist.
			_, _ = runner.Run(ctx, "launchctl", "bootout", domain, file.Path)
			if _, err := runner.Run(ctx, "launchctl", "bootstrap", domain, file.Path); err != nil {
				return fmt.Errorf("scheduler: activate launchd job: %w", err)
			}
		}
		return nil
	case "systemd":
		if !hasExactFiles(schedule.Files, "tooltend-reconcile.service", "tooltend-reconcile.timer", "tooltend-watchdog.service", "tooltend-watchdog.timer") {
			return errors.New("scheduler: invalid systemd plan")
		}
		if _, err := runner.Run(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
			return fmt.Errorf("scheduler: reload systemd user units: %w", err)
		}
		if _, err := runner.Run(ctx, "systemctl", "--user", "enable", "--now", "tooltend-reconcile.timer", "tooltend-watchdog.timer"); err != nil {
			return fmt.Errorf("scheduler: activate systemd timer: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("scheduler: unsupported plan platform %q", schedule.Platform)
	}
}

// Deactivate pauses the registered one-shot schedule without deleting its
// files. A reset uses this before snapshotting state and re-activates either
// the new schedule or the restored old schedule.
func Deactivate(ctx context.Context, schedule Plan, runner execx.Runner) error {
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	switch schedule.Platform {
	case "launchd":
		if !hasExactFiles(schedule.Files, "io.tooltend.reconcile.plist", "io.tooltend.watchdog.plist") {
			return errors.New("scheduler: invalid launchd plan")
		}
		domain := "gui/" + strconv.Itoa(os.Getuid())
		var result error
		for _, file := range schedule.Files {
			_, err := runner.Run(ctx, "launchctl", "bootout", domain, file.Path)
			if filepath.Base(file.Path) == "io.tooltend.reconcile.plist" {
				result = errors.Join(result, err)
			}
		}
		return result
	case "systemd":
		if !hasExactFiles(schedule.Files, "tooltend-reconcile.service", "tooltend-reconcile.timer", "tooltend-watchdog.service", "tooltend-watchdog.timer") {
			return errors.New("scheduler: invalid systemd plan")
		}
		_, err := runner.Run(ctx, "systemctl", "--user", "disable", "--now", "tooltend-reconcile.timer", "tooltend-watchdog.timer")
		return err
	default:
		return fmt.Errorf("scheduler: unsupported plan platform %q", schedule.Platform)
	}
}

func hasExactFiles(files []File, names ...string) bool {
	if len(files) != len(names) {
		return false
	}
	expected := make(map[string]struct{}, len(names))
	for _, name := range names {
		expected[name] = struct{}{}
	}
	for _, file := range files {
		name := filepath.Base(file.Path)
		if _, ok := expected[name]; !ok {
			return false
		}
		delete(expected, name)
	}
	return len(expected) == 0
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
	return renderLaunchdJob("io.tooltend.reconcile", args, options, options.Hour, options.Minute, "reconcile")
}

func renderLaunchdWatchdog(options Options) string {
	hour, minute := watchdogTime(options.Hour, options.Minute)
	args := []string{options.Executable, "watchdog", "--max-age", "2h", "--state-dir", options.StateDir, "--json"}
	return renderLaunchdJob("io.tooltend.watchdog", args, options, hour, minute, "watchdog")
}

func renderLaunchdJob(label string, args []string, options Options, hour, minute int, logName string) string {
	pathEnv := workerPATH(options.Executable, options.PathEnv)
	logsDir := filepath.Join(options.StateDir, "logs")
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
  <key>Label</key><string>` + xmlEscape(label) + `</string>
  <key>ProgramArguments</key>
  <array>
` + program.String() + `  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>` + xmlEscape(pathEnv) + `</string>
  </dict>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key><integer>` + strconv.Itoa(hour) + `</integer>
    <key>Minute</key><integer>` + strconv.Itoa(minute) + `</integer>
  </dict>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>StandardOutPath</key><string>` + xmlEscape(filepath.Join(logsDir, logName+".stdout.log")) + `</string>
  <key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(logsDir, logName+".stderr.log")) + `</string>
</dict>
</plist>
`
}

func renderSystemdService(options Options) string {
	return `[Unit]
Description=ToolTend one-shot reconciliation

[Service]
Type=oneshot
Environment=` + systemdQuote("PATH="+workerPATH(options.Executable, options.PathEnv)) + `
ExecStart=` + systemdQuote(options.Executable) + ` reconcile --once --state-dir ` + systemdQuote(options.StateDir) + ` --json
`
}

func renderSystemdWatchdogService(options Options) string {
	return `[Unit]
Description=Check ToolTend scheduled reconciliation

[Service]
Type=oneshot
Environment=` + systemdQuote("PATH="+workerPATH(options.Executable, options.PathEnv)) + `
ExecStart=` + systemdQuote(options.Executable) + ` watchdog --max-age 3h --state-dir ` + systemdQuote(options.StateDir) + ` --json
`
}

func workerPATH(executable, current string) string {
	if strings.TrimSpace(current) == "" {
		current = os.Getenv("PATH")
	}
	candidates := []string{filepath.Dir(executable)}
	candidates = append(candidates, filepath.SplitList(current)...)
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/opt/homebrew/bin", "/opt/homebrew/sbin")
	}
	candidates = append(candidates, "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	seen := map[string]struct{}{}
	entries := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(strings.TrimSpace(candidate))
		if !filepath.IsAbs(candidate) {
			continue
		}
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		entries = append(entries, candidate)
	}
	return strings.Join(entries, string(os.PathListSeparator))
}

func renderSystemdTimer(options Options) string {
	return renderSystemdTimerAt("Run ToolTend reconciliation daily", options.Hour, options.Minute)
}

func renderSystemdWatchdogTimer(options Options) string {
	hour, minute := timeAfter(options.Hour, options.Minute, 2*time.Hour)
	return renderSystemdTimerAt("Check ToolTend reconciliation daily", hour, minute)
}

func renderSystemdTimerAt(description string, hour, minute int) string {
	return `[Unit]
Description=` + description + `

[Timer]
OnCalendar=*-*-* ` + fmt.Sprintf("%02d:%02d:00", hour, minute) + `
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
`
}

func watchdogTime(hour, minute int) (int, int) {
	return timeAfter(hour, minute, time.Hour)
}

func timeAfter(hour, minute int, offset time.Duration) (int, int) {
	minutes := (hour*60 + minute + int(offset/time.Minute)) % (24 * 60)
	return minutes / 60, minutes % 60
}

func systemdQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

func xmlEscape(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(value)
}
