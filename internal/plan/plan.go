package plan

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrConfirmationRequired = errors.New("plan: confirmation required")

type OperationKind string

const (
	OperationCreateDirectory OperationKind = "create_directory"
	OperationWriteFile       OperationKind = "write_file"
	OperationDatabase        OperationKind = "database"
	OperationInstallHook     OperationKind = "install_hook"
	OperationInstallSchedule OperationKind = "install_schedule"
	OperationStageRuntime    OperationKind = "stage_runtime"
	OperationActivate        OperationKind = "activate"
	OperationOther           OperationKind = "other"
)

type OperationPreview struct {
	ID                   string            `json:"id"`
	Kind                 OperationKind     `json:"kind"`
	Target               string            `json:"target"`
	Summary              string            `json:"summary"`
	BeforeHash           string            `json:"before_hash,omitempty"`
	AfterHash            string            `json:"after_hash,omitempty"`
	Reversible           bool              `json:"reversible"`
	RequiresConfirmation bool              `json:"requires_confirmation"`
	Details              map[string]string `json:"details,omitempty"`
}

type Preview struct {
	ID                   string             `json:"id"`
	Title                string             `json:"title"`
	Operations           []OperationPreview `json:"operations"`
	RequiresConfirmation bool               `json:"requires_confirmation"`
}

type Operation interface {
	Preview() OperationPreview
	Apply(context.Context) error
}

type ReversibleOperation interface {
	Operation
	Rollback(context.Context) error
}

type FuncOperation struct {
	Description  OperationPreview
	ApplyFunc    func(context.Context) error
	RollbackFunc func(context.Context) error
}

func (o FuncOperation) Preview() OperationPreview { return clonePreview(o.Description) }
func (o FuncOperation) Apply(ctx context.Context) error {
	if o.ApplyFunc == nil {
		return nil
	}
	return o.ApplyFunc(ctx)
}
func (o FuncOperation) Rollback(ctx context.Context) error {
	if o.RollbackFunc == nil {
		return errors.New("plan: operation has no rollback")
	}
	return o.RollbackFunc(ctx)
}

type Plan struct {
	ID         string
	Title      string
	Operations []Operation
}

func (p Plan) Preview() Preview {
	result := Preview{ID: p.ID, Title: p.Title, Operations: make([]OperationPreview, 0, len(p.Operations))}
	for _, operation := range p.Operations {
		description := operation.Preview()
		result.Operations = append(result.Operations, description)
		result.RequiresConfirmation = result.RequiresConfirmation || description.RequiresConfirmation
	}
	return result
}

func (p Plan) Validate() error {
	if p.ID == "" || p.Title == "" {
		return errors.New("plan: id and title are required")
	}
	seen := make(map[string]struct{}, len(p.Operations))
	for i, operation := range p.Operations {
		if operation == nil {
			return fmt.Errorf("plan: operation %d is nil", i)
		}
		description := operation.Preview()
		if description.ID == "" || description.Kind == "" || description.Summary == "" {
			return fmt.Errorf("plan: operation %d has incomplete preview", i)
		}
		if _, exists := seen[description.ID]; exists {
			return fmt.Errorf("plan: duplicate operation id %q", description.ID)
		}
		seen[description.ID] = struct{}{}
		if description.Reversible {
			if _, ok := operation.(ReversibleOperation); !ok {
				return fmt.Errorf("plan: operation %q claims reversibility without rollback", description.ID)
			}
		}
	}
	return nil
}

type ApplyOptions struct {
	DryRun    bool
	Confirmed bool
}

type OperationResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type ApplyResult struct {
	PlanID     string            `json:"plan_id"`
	DryRun     bool              `json:"dry_run"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at"`
	Operations []OperationResult `json:"operations"`
	RolledBack bool              `json:"rolled_back"`
}

func (p Plan) Apply(ctx context.Context, options ApplyOptions) (ApplyResult, error) {
	result := ApplyResult{PlanID: p.ID, DryRun: options.DryRun, StartedAt: time.Now().UTC(), Operations: []OperationResult{}}
	if err := p.Validate(); err != nil {
		result.FinishedAt = time.Now().UTC()
		return result, err
	}
	preview := p.Preview()
	if preview.RequiresConfirmation && !options.Confirmed && !options.DryRun {
		result.FinishedAt = time.Now().UTC()
		return result, ErrConfirmationRequired
	}
	if options.DryRun {
		result.FinishedAt = time.Now().UTC()
		return result, nil
	}

	applied := make([]Operation, 0, len(p.Operations))
	for _, operation := range p.Operations {
		if err := ctx.Err(); err != nil {
			result.RolledBack, err = p.rollback(ctx, applied, err)
			result.FinishedAt = time.Now().UTC()
			return result, err
		}
		description := operation.Preview()
		if err := operation.Apply(ctx); err != nil {
			result.Operations = append(result.Operations, OperationResult{ID: description.ID, Status: "failed", Error: err.Error()})
			result.RolledBack, err = p.rollback(ctx, applied, fmt.Errorf("plan: apply %s: %w", description.ID, err))
			result.FinishedAt = time.Now().UTC()
			return result, err
		}
		applied = append(applied, operation)
		result.Operations = append(result.Operations, OperationResult{ID: description.ID, Status: "applied"})
	}
	result.FinishedAt = time.Now().UTC()
	return result, nil
}

func (p Plan) rollback(ctx context.Context, applied []Operation, cause error) (bool, error) {
	var rollbackErrors []error
	rolledBack := false
	for i := len(applied) - 1; i >= 0; i-- {
		if !applied[i].Preview().Reversible {
			continue
		}
		operation, ok := applied[i].(ReversibleOperation)
		if !ok {
			continue
		}
		rolledBack = true
		if err := operation.Rollback(context.WithoutCancel(ctx)); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback %s: %w", operation.Preview().ID, err))
		}
	}
	return rolledBack, errors.Join(append([]error{cause}, rollbackErrors...)...)
}

func clonePreview(value OperationPreview) OperationPreview {
	result := value
	if value.Details != nil {
		result.Details = make(map[string]string, len(value.Details))
		for key, item := range value.Details {
			result.Details[key] = item
		}
	}
	return result
}
