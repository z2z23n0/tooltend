package notify

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/z2z23n0/tooltend/internal/execx"
)

const launchServicesRegister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

//go:embed macos/ToolTendNotifier.swift
var darwinNotifierSource []byte

const darwinInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>applet</string>
	<key>CFBundleIdentifier</key>
	<string>io.tooltend.notifier.native</string>
	<key>CFBundleName</key>
	<string>ToolTend Notifier</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>1.0</string>
	<key>CFBundleVersion</key>
	<string>1</string>
	<key>LSUIElement</key>
	<true/>
</dict>
</plist>
`

type InstallResult struct {
	AppPath    string `json:"app_path"`
	Executable string `json:"executable"`
}

func CheckDarwin(home string) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported
	}
	appPath := DarwinAppPath(home)
	executable := DarwinNotifierExecutable(home)
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ErrNotifierUnavailable
	}
	plist, err := os.ReadFile(filepath.Join(appPath, "Contents", "Info.plist"))
	if err != nil || !strings.Contains(string(plist), DarwinBundleID) {
		return errors.New("ToolTend Notifier has an invalid application identity")
	}
	return nil
}

func CheckDarwinAuthorization(ctx context.Context, home string, runner execx.Runner) error {
	if err := CheckDarwin(home); err != nil {
		return err
	}
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	result, err := runner.Run(ctx, DarwinNotifierExecutable(home), "--check")
	if err != nil {
		return commandFailure("check notifier authorization", result, err)
	}
	return nil
}

func InstallDarwin(ctx context.Context, home string, runner execx.Runner) (InstallResult, error) {
	result := InstallResult{AppPath: DarwinAppPath(home), Executable: DarwinNotifierExecutable(home)}
	if runtime.GOOS != "darwin" {
		return result, ErrUnsupported
	}
	if strings.TrimSpace(home) == "" || !filepath.IsAbs(home) {
		return result, errors.New("notifier install requires an absolute home directory")
	}
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	applicationsDir := filepath.Join(home, "Applications")
	if err := os.MkdirAll(applicationsDir, 0o755); err != nil {
		return result, fmt.Errorf("create Applications directory: %w", err)
	}
	buildDir, err := os.MkdirTemp(applicationsDir, ".tooltend-notifier-build-")
	if err != nil {
		return result, fmt.Errorf("create notifier build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)
	buildApp := filepath.Join(buildDir, DarwinAppName)
	contentsDir := filepath.Join(buildApp, "Contents")
	macOSDir := filepath.Join(contentsDir, "MacOS")
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		return result, fmt.Errorf("create notifier bundle: %w", err)
	}
	sourcePath := filepath.Join(buildDir, "ToolTendNotifier.swift")
	if err := os.WriteFile(sourcePath, darwinNotifierSource, 0o600); err != nil {
		return result, fmt.Errorf("write notifier source: %w", err)
	}
	plistPath := filepath.Join(contentsDir, "Info.plist")
	if err := os.WriteFile(plistPath, []byte(darwinInfoPlist), 0o644); err != nil {
		return result, fmt.Errorf("write notifier application identity: %w", err)
	}
	if commandResult, commandErr := runner.Run(ctx, "/usr/bin/xcrun", "--sdk", "macosx", "swiftc", "-framework", "AppKit", "-framework", "UserNotifications", "-o", resultExecutable(buildApp), sourcePath); commandErr != nil {
		return result, commandFailure("compile notifier", commandResult, commandErr)
	}
	if commandResult, commandErr := runner.Run(ctx, "/usr/bin/codesign", "--force", "--sign", "-", buildApp); commandErr != nil {
		return result, commandFailure("sign notifier", commandResult, commandErr)
	}
	backupPath := filepath.Join(applicationsDir, fmt.Sprintf(".tooltend-notifier-backup-%d", time.Now().UnixNano()))
	hadPrevious := false
	if _, statErr := os.Lstat(result.AppPath); statErr == nil {
		if err := os.Rename(result.AppPath, backupPath); err != nil {
			return result, fmt.Errorf("back up existing notifier: %w", err)
		}
		hadPrevious = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("inspect existing notifier: %w", statErr)
	}
	restore := func() {
		_ = os.RemoveAll(result.AppPath)
		if hadPrevious {
			_ = os.Rename(backupPath, result.AppPath)
		}
	}
	if err := os.Rename(buildApp, result.AppPath); err != nil {
		restore()
		return result, fmt.Errorf("install notifier: %w", err)
	}
	if commandResult, commandErr := runner.Run(ctx, launchServicesRegister, "-f", result.AppPath); commandErr != nil {
		restore()
		return result, commandFailure("register notifier", commandResult, commandErr)
	}
	if err := CheckDarwin(home); err != nil {
		restore()
		return result, err
	}
	if hadPrevious {
		_ = os.RemoveAll(backupPath)
	}
	return result, nil
}

func resultExecutable(appPath string) string {
	return filepath.Join(appPath, "Contents", "MacOS", DarwinExecutable)
}

func commandFailure(action string, result execx.Result, err error) error {
	detail := strings.TrimSpace(string(result.Stderr))
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %s: %w", action, detail, err)
}
