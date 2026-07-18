package cli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/z2z23n0/tooltend/internal/api/v1"
	"github.com/z2z23n0/tooltend/internal/config"
	"github.com/z2z23n0/tooltend/internal/execx"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/selfupdate"
	"github.com/z2z23n0/tooltend/internal/store"
)

// Options makes the command tree embeddable and keeps filesystem-sensitive
// behavior deterministic in tests. Zero values use the current process.
type Options struct {
	In         io.Reader
	Out        io.Writer
	ErrOut     io.Writer
	HomeDir    string
	WorkingDir string
	Executable string
	Getenv     func(string) string
	Runner     execx.Runner
	// RunOnce is supplied by the reconciliation package. Keeping this narrow
	// avoids making command construction depend on worker implementation detail.
	RunOnce func(context.Context, config.Paths) (any, error)
}

type globalOptions struct {
	JSON     bool
	DryRun   bool
	Yes      bool
	Config   string
	StateDir string
	NoColor  bool
}

type App struct {
	in         io.Reader
	out        io.Writer
	errOut     io.Writer
	home       string
	workingDir string
	executable string
	getenv     func(string) string
	runOnce    func(context.Context, config.Paths) (any, error)
	runner     execx.Runner
	global     globalOptions
	selfApply  selfupdate.ApplyResult
	warnings   []v1.Warning
}

// New builds the complete ToolTend v0.2 command tree. Constructing it is
// side-effect free: it does not create configuration, state, or data paths.
func New(options Options) *cobra.Command {
	a := newApp(options)
	root := &cobra.Command{
		Use:           "tooltend",
		Short:         "Bundle lifecycle manager for coding-agent tooling",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetIn(a.in)
	root.SetOut(a.out)
	root.SetErr(a.errOut)
	flags := root.PersistentFlags()
	flags.BoolVar(&a.global.JSON, "json", false, "emit a stable JSON envelope")
	flags.BoolVar(&a.global.DryRun, "dry-run", false, "preview writes without applying them")
	flags.BoolVarP(&a.global.Yes, "yes", "y", false, "confirm the complete write plan")
	flags.StringVar(&a.global.Config, "config", "", "use an alternate configuration file")
	flags.StringVar(&a.global.StateDir, "state-dir", "", "use an alternate state directory")
	flags.BoolVar(&a.global.NoColor, "no-color", false, "disable colored human output")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		a.warnings = nil
		if cmd.Annotations[internalDriverAnnotation] == "true" {
			return nil
		}
		if legacyCommand(commandName(cmd)) {
			a.warnings = append(a.warnings, v1.Warning{Code: "deprecated_component_api", Message: "this component-level command is deprecated; use tooltend bundles instead"})
		}
		if a.global.DryRun {
			return nil
		}
		name := cmd.Name()
		if commandName(cmd) == "init" || name == "hook" || name == "kick" || name == "reconcile" || name == "version" {
			return nil
		}
		paths, err := a.paths()
		if err != nil {
			return a.writeFailure(commandName(cmd), err)
		}
		a.selfApply, err = a.selfUpdateManager(paths.StateDir, "").ApplyPending(cmd.Context())
		if err != nil {
			if errors.Is(err, selfupdate.ErrHomebrewManaged) {
				err = &commandError{Code: "homebrew_managed", Message: selfupdate.ErrHomebrewManaged.Error(), Cause: err}
			}
			return a.writeFailure(commandName(cmd), err)
		}
		if a.selfApply.Applied {
			a.warnings = append(a.warnings, a.repairAfterSelfUpdate(cmd.Context(), paths)...)
		}
		return nil
	}

	root.AddCommand(
		a.newInitCommand(),
		a.newScanCommand(),
		a.newStatusCommand(),
		a.newBundlesCommand(),
		a.newComponentsCommand(),
		a.newPolicyCommand(),
		a.newUpdateCommand(),
		a.newReviewCommand(),
		a.newHistoryCommand(),
		a.newRollbackCommand(),
		a.newAdoptCommand(),
		a.newProjectCommand(),
		a.newSelfCommand(),
		a.newDoctorCommand(),
		a.newHookCommand(),
		a.newKickCommand(),
		a.newReconcileCommand(),
		a.newWatchdogCommand(),
		a.newVersionCommand(),
		a.newBundleDriverCommand(),
		a.newNotifierCommand(),
	)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return a.writeFailure(commandName(cmd), cliError("invalid_argument", err.Error(), err))
	})
	a.wrapArgumentErrors(root)
	return root
}

func (a *App) wrapArgumentErrors(command *cobra.Command) {
	if command.Args != nil {
		validate := command.Args
		command.Args = func(cmd *cobra.Command, args []string) error {
			if err := validate(cmd, args); err != nil {
				return a.writeFailure(commandName(cmd), cliError("invalid_argument", err.Error(), err))
			}
			return nil
		}
	}
	for _, child := range command.Commands() {
		a.wrapArgumentErrors(child)
	}
}

func commandName(command *cobra.Command) string {
	name := strings.TrimSpace(strings.TrimPrefix(command.CommandPath(), "tooltend"))
	if name == "" {
		return "root"
	}
	return name
}

func newApp(options Options) *App {
	in, out, errOut := options.In, options.Out, options.ErrOut
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if errOut == nil {
		errOut = os.Stderr
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := options.HomeDir
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	workingDir := options.WorkingDir
	if workingDir == "" {
		workingDir, _ = os.Getwd()
	}
	executable := options.Executable
	if executable == "" {
		executable, _ = os.Executable()
	}
	if home != "" {
		home = filepath.Clean(home)
	}
	if workingDir != "" {
		workingDir = filepath.Clean(workingDir)
	}
	if executable != "" {
		executable = filepath.Clean(executable)
	}
	runner := options.Runner
	if runner == nil {
		runner = execx.ExecRunner{}
	}
	return &App{
		in: in, out: out, errOut: errOut,
		home: home, workingDir: workingDir,
		executable: executable, getenv: getenv, runOnce: options.RunOnce, runner: runner,
	}
}

func (a *App) paths() (config.Paths, error) {
	if a.home == "" {
		return config.Paths{}, errors.New("cannot determine user home directory")
	}
	paths := config.ResolveWith(a.home, a.getenv)
	if value := strings.TrimSpace(a.global.Config); value != "" {
		absolute, err := a.absolute(value)
		if err != nil {
			return config.Paths{}, err
		}
		paths.ConfigFile = absolute
		paths.ConfigDir = filepath.Dir(absolute)
	}
	if value := strings.TrimSpace(a.global.StateDir); value != "" {
		absolute, err := a.absolute(value)
		if err != nil {
			return config.Paths{}, err
		}
		paths.StateDir = absolute
		paths.DatabaseFile = filepath.Join(absolute, "state.db")
		paths.ActivationLock = filepath.Join(absolute, "activation.lock")
	}
	return paths, nil
}

func (a *App) absolute(path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.workingDir, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

type commandError struct {
	Code      string
	Message   string
	Retryable bool
	Details   map[string]any
	Cause     error
}

func (e *commandError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Code
}

func (e *commandError) Unwrap() error { return e.Cause }

// reportedError prevents main from printing a second, non-protocol error.
type reportedError struct{ cause error }

func (e reportedError) Error() string { return e.cause.Error() }
func (e reportedError) Unwrap() error { return e.cause }

// IsReported reports whether the command already emitted a human error or a
// JSON failure envelope.
func IsReported(err error) bool {
	var value reportedError
	return errors.As(err, &value)
}

func cliError(code, message string, cause error) error {
	return &commandError{Code: code, Message: message, Cause: cause}
}

func (a *App) run(command string, action func(context.Context) (any, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		data, err := action(cmd.Context())
		if err != nil {
			return a.writeFailure(command, err)
		}
		if err := a.writeSuccess(command, data); err != nil {
			return err
		}
		return nil
	}
}

func (a *App) writeSuccess(command string, data any) error {
	if a.global.JSON {
		warnings := append([]v1.Warning(nil), a.warnings...)
		if a.selfApply.Applied {
			warnings = append(warnings, v1.Warning{
				Code: "self_update_applied", Message: "a previously confirmed ToolTend self-update was applied before this command",
				Details: map[string]any{"version": a.selfApply.Version},
			})
		}
		return v1.Write(a.out, v1.Success(command, data, warnings...))
	}
	for _, warning := range a.warnings {
		_, _ = fmt.Fprintf(a.errOut, "Warning: %s\n", warning.Message)
	}
	if a.selfApply.Applied {
		_, _ = fmt.Fprintf(a.out, "ToolTend self-update %s was applied before this command.\n", a.selfApply.Version)
	}
	return writeHuman(a.out, data)
}

func legacyCommand(name string) bool {
	if strings.HasPrefix(name, "components ") || strings.HasPrefix(name, "policy ") {
		return true
	}
	switch name {
	case "update", "adopt", "rollback", "history", "review":
		return true
	default:
		return false
	}
}

func (a *App) writeFailure(command string, err error) error {
	value := classifyError(err)
	if a.global.JSON {
		writeErr := v1.Write(a.out, v1.Failure(command, value.Code, value.Message, value.Retryable, value.Details))
		if writeErr != nil {
			return writeErr
		}
	} else {
		_, _ = fmt.Fprintf(a.errOut, "Error: %s\n", value.Message)
	}
	return reportedError{cause: err}
}

func classifyError(err error) *commandError {
	var value *commandError
	if errors.As(err, &value) {
		return value
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return &commandError{Code: "not_found", Message: "requested record was not found", Cause: err}
	case errors.Is(err, os.ErrNotExist):
		return &commandError{Code: "not_initialized", Message: "ToolTend is not initialized; run tooltend init", Cause: err}
	default:
		return &commandError{Code: "operation_failed", Message: err.Error(), Cause: err}
	}
}

func writeHuman(w io.Writer, data any) error {
	if data == nil {
		_, err := fmt.Fprintln(w, "Done.")
		return err
	}
	switch value := data.(type) {
	case string:
		_, err := fmt.Fprintln(w, value)
		return err
	case fmt.Stringer:
		_, err := fmt.Fprintln(w, value.String())
		return err
	default:
		encoded, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(encoded))
		return err
	}
}

type mutationResult struct {
	Preview plan.Preview     `json:"preview"`
	Apply   plan.ApplyResult `json:"apply"`
	Result  any              `json:"result,omitempty"`
}

func (a *App) applyPlan(ctx context.Context, value plan.Plan, result func() any) (mutationResult, error) {
	if err := value.Validate(); err != nil {
		return mutationResult{}, err
	}
	preview := value.Preview()
	if a.global.DryRun {
		applied, err := value.Apply(ctx, plan.ApplyOptions{DryRun: true})
		return mutationResult{Preview: preview, Apply: applied}, err
	}
	confirmed := a.global.Yes
	if preview.RequiresConfirmation && !confirmed {
		if a.global.JSON {
			return mutationResult{}, &commandError{
				Code: "confirmation_required", Message: "write plan requires --yes in JSON mode",
				Details: map[string]any{"preview": preview}, Cause: plan.ErrConfirmationRequired,
			}
		}
		if err := writeHuman(a.out, preview); err != nil {
			return mutationResult{}, err
		}
		accepted, err := a.confirmOnce()
		if err != nil {
			return mutationResult{}, err
		}
		if !accepted {
			return mutationResult{}, &commandError{Code: "confirmation_declined", Message: "write plan was not applied", Cause: plan.ErrConfirmationRequired}
		}
		confirmed = true
	}
	applied, err := value.Apply(ctx, plan.ApplyOptions{Confirmed: confirmed})
	if err != nil {
		failed := mutationResult{Preview: preview, Apply: applied}
		var classified *commandError
		if errors.As(err, &classified) {
			details := make(map[string]any, len(classified.Details)+2)
			for key, item := range classified.Details {
				details[key] = item
			}
			details["preview"], details["apply"] = preview, applied
			return failed, &commandError{Code: classified.Code, Message: classified.Message, Retryable: classified.Retryable, Details: details, Cause: err}
		}
		return failed, &commandError{
			Code: "plan_apply_failed", Message: "write plan failed; inspect operation results before retrying",
			Details: map[string]any{"preview": preview, "apply": applied}, Cause: err,
		}
	}
	var payload any
	if result != nil {
		payload = result()
	}
	return mutationResult{Preview: preview, Apply: applied, Result: payload}, nil
}

func (a *App) confirmOnce() (bool, error) {
	if _, err := fmt.Fprint(a.out, "Apply this plan? [y/N] "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(a.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes", nil
}

func (a *App) openReadOnly(paths config.Paths) (*store.Store, error) {
	database, err := store.OpenReadOnly(paths.DatabaseFile)
	if err != nil {
		return nil, err
	}
	version, err := database.UserVersion(context.Background())
	if err != nil {
		_ = database.Close()
		return nil, err
	}
	if version != store.SchemaVersion {
		_ = database.Close()
		return nil, fmt.Errorf("database schema %d is not supported", version)
	}
	return database, nil
}

func (a *App) loadConfig(paths config.Paths) (config.Config, error) {
	value, err := config.Load(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return config.Default(), nil
	}
	return value, err
}
