package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

const defaultOutputLimit = 4 << 20

type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Runner interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}

type ExecRunner struct {
	Dir         string
	Env         []string
	OutputLimit int64
}

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	if name == "" {
		return Result{}, errors.New("executable is required")
	}
	limit := r.OutputLimit
	if limit <= 0 {
		limit = defaultOutputLimit
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = r.Dir
	if r.Env != nil {
		cmd.Env = append([]string(nil), r.Env...)
	} else {
		cmd.Env = os.Environ()
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, remaining: limit}
	cmd.Stderr = &limitedWriter{w: &stderr, remaining: limit}
	err := cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, &CommandError{Name: name, ExitCode: result.ExitCode}
	}
	return result, fmt.Errorf("start %s: %w", name, err)
}

type CommandError struct {
	Name     string
	ExitCode int
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s exited with code %d", e.Name, e.ExitCode)
}

type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	original := len(p)
	if w.remaining <= 0 {
		return original, nil
	}
	if int64(len(p)) > w.remaining {
		p = p[:w.remaining]
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	if err != nil {
		return n, err
	}
	return original, nil
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var commandErr *CommandError
	if errors.As(err, &commandErr) {
		return commandErr.ExitCode
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
