package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type AdoptionKind string

const (
	AdoptionFile         AdoptionKind = "file"
	AdoptionHook         AdoptionKind = "hook"
	AdoptionRuntimeCLI   AdoptionKind = "runtime_cli"
	AdoptionRuntimeStdio AdoptionKind = "runtime_stdio"
)

type AdoptionPhase string

const (
	AdoptionPrepared   AdoptionPhase = "prepared"
	AdoptionSwitched   AdoptionPhase = "switched"
	AdoptionFinalizing AdoptionPhase = "finalizing"
	AdoptionCommitted  AdoptionPhase = "committed"
	AdoptionRolledBack AdoptionPhase = "rolled_back"
	AdoptionBlocked    AdoptionPhase = "blocked"
)

// AdoptionJournalPlan contains only recovery metadata and the already
// validated database commit. EffectsJSON is a versioned lifecycle structure;
// it must contain paths and hashes only, never host file contents or commands.
type AdoptionJournalPlan struct {
	Version        int             `json:"version"`
	Kind           AdoptionKind    `json:"kind"`
	Root           string          `json:"root"`
	GenerationPath string          `json:"generation_path"`
	GenerationHash string          `json:"generation_hash"`
	Runtime        bool            `json:"runtime"`
	EffectsJSON    json.RawMessage `json:"effects"`
	Commit         AdoptionCommit  `json:"commit"`
}

type AdoptionIntent struct {
	ID        string              `json:"id"`
	BindingID string              `json:"binding_id"`
	Kind      AdoptionKind        `json:"kind"`
	Plan      AdoptionJournalPlan `json:"plan"`
	PlanHash  string              `json:"plan_hash"`
	Phase     AdoptionPhase       `json:"phase"`
	ErrorCode string              `json:"error_code,omitempty"`
	StartedAt time.Time           `json:"started_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

func (s *Store) PrepareAdoption(ctx context.Context, id string, plan AdoptionJournalPlan, now time.Time) error {
	if id == "" || plan.Version != 1 || plan.Root == "" || plan.GenerationPath == "" || plan.GenerationHash == "" {
		return errors.New("store: adoption journal plan is incomplete")
	}
	if err := validateAdoptionKind(plan.Kind); err != nil {
		return err
	}
	if err := validateAdoptionCommit(plan.Commit); err != nil {
		return err
	}
	if plan.Commit.Binding.ID != plan.Commit.ExpectedBinding.ID || plan.Commit.Binding.ID == "" {
		return errors.New("store: adoption journal binding mismatch")
	}
	if plan.Commit.Generation.ID == "" || plan.GenerationPath == plan.Root || plan.Commit.Generation.IntegrityHash != plan.GenerationHash {
		return errors.New("store: adoption journal generation mismatch")
	}
	if len(plan.EffectsJSON) == 0 || !json.Valid(plan.EffectsJSON) {
		return errors.New("store: adoption journal effects are not valid JSON")
	}
	encoded, hash, err := encodeAdoptionPlan(plan)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO adoption_intents(id,binding_id,kind,plan_json,plan_hash,phase,error_code,started_at,updated_at)
		VALUES(?,?,?,?,?,'prepared','',?,?)`, id, plan.Commit.Binding.ID, plan.Kind, string(encoded), hash, timeText(now), timeText(now))
	if err != nil {
		return fmt.Errorf("store: prepare adoption journal: %w", err)
	}
	return nil
}

func (s *Store) MarkAdoptionSwitched(ctx context.Context, id string, now time.Time) error {
	return s.updateAdoptionPhase(ctx, id, []AdoptionPhase{AdoptionPrepared, AdoptionSwitched}, AdoptionSwitched, "", now)
}

func (s *Store) MarkAdoptionBlocked(ctx context.Context, id, code string, now time.Time) error {
	if code == "" {
		code = "adoption_recovery_conflict"
	}
	return s.updateAdoptionPhase(ctx, id, []AdoptionPhase{AdoptionPrepared, AdoptionSwitched, AdoptionBlocked}, AdoptionBlocked, code, now)
}

func (s *Store) MarkAdoptionRolledBack(ctx context.Context, id string, now time.Time) error {
	return s.updateAdoptionPhase(ctx, id, []AdoptionPhase{AdoptionPrepared, AdoptionSwitched, AdoptionBlocked, AdoptionRolledBack}, AdoptionRolledBack, "", now)
}

func (s *Store) updateAdoptionPhase(ctx context.Context, id string, from []AdoptionPhase, to AdoptionPhase, code string, now time.Time) error {
	if id == "" {
		return errors.New("store: adoption intent ID is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	placeholders := ""
	args := []any{to, code, timeText(now), id}
	for index, phase := range from {
		if index != 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, phase)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE adoption_intents SET phase=?,error_code=?,updated_at=? WHERE id=? AND phase IN (`+placeholders+`)`, args...)
	if err != nil {
		return fmt.Errorf("store: update adoption phase: %w", err)
	}
	return requireOneRow(result, "adoption intent")
}

func (s *Store) GetAdoptionIntent(ctx context.Context, id string) (AdoptionIntent, error) {
	return scanAdoptionIntent(s.db.QueryRowContext(ctx, `SELECT id,binding_id,kind,plan_json,plan_hash,phase,error_code,started_at,updated_at FROM adoption_intents WHERE id=?`, id))
}

func (s *Store) ListPendingAdoptions(ctx context.Context) ([]AdoptionIntent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,binding_id,kind,plan_json,plan_hash,phase,error_code,started_at,updated_at
		FROM adoption_intents WHERE phase IN ('prepared','switched','blocked') ORDER BY started_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AdoptionIntent
	for rows.Next() {
		value, err := scanAdoptionIntent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

// FinalizeAdoption makes the journal terminal in the same SQLite transaction
// as every mutable adoption row. A crash exposes either the old prepared plan
// or the complete committed adoption, never an unjournaled half commit.
func (s *Store) FinalizeAdoption(ctx context.Context, id string, now time.Time) error {
	return s.WithTx(ctx, func(tx *sql.Tx) error {
		intent, err := scanAdoptionIntent(tx.QueryRowContext(ctx, `SELECT id,binding_id,kind,plan_json,plan_hash,phase,error_code,started_at,updated_at FROM adoption_intents WHERE id=?`, id))
		if err != nil {
			return err
		}
		if intent.Phase == AdoptionCommitted {
			return nil
		}
		if intent.Phase != AdoptionPrepared && intent.Phase != AdoptionSwitched && intent.Phase != AdoptionBlocked {
			return fmt.Errorf("store: adoption intent cannot finalize from %s", intent.Phase)
		}
		if err := validateAdoptionCommit(intent.Plan.Commit); err != nil {
			return err
		}
		if now.IsZero() {
			now = time.Now().UTC()
		}
		result, err := tx.ExecContext(ctx, `UPDATE adoption_intents SET phase='finalizing',error_code='',updated_at=? WHERE id=? AND phase=?`,
			timeText(now), id, intent.Phase)
		if err != nil {
			return err
		}
		if err := requireOneRow(result, "adoption intent"); err != nil {
			return err
		}
		if err := commitAdoptionTx(ctx, tx, intent.Plan.Commit); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE adoption_intents SET phase='committed',error_code='',updated_at=? WHERE id=? AND phase='finalizing'`, timeText(now), id)
		if err != nil {
			return err
		}
		return requireOneRow(result, "adoption intent")
	})
}

type adoptionScanner interface {
	Scan(...any) error
}

func scanAdoptionIntent(row adoptionScanner) (AdoptionIntent, error) {
	var value AdoptionIntent
	var planJSON, started, updated string
	if err := row.Scan(&value.ID, &value.BindingID, &value.Kind, &planJSON, &value.PlanHash, &value.Phase, &value.ErrorCode, &started, &updated); err != nil {
		return value, err
	}
	if err := validateAdoptionKind(value.Kind); err != nil {
		return value, err
	}
	digest := sha256.Sum256([]byte(planJSON))
	if hex.EncodeToString(digest[:]) != value.PlanHash {
		return value, errors.New("store: adoption journal plan hash mismatch")
	}
	if err := json.Unmarshal([]byte(planJSON), &value.Plan); err != nil {
		return value, fmt.Errorf("store: decode adoption journal: %w", err)
	}
	if value.Plan.Kind != value.Kind || value.Plan.Commit.Binding.ID != value.BindingID {
		return value, errors.New("store: adoption journal row does not match plan")
	}
	var err error
	value.StartedAt, err = parseTime(started)
	if err != nil {
		return value, err
	}
	value.UpdatedAt, err = parseTime(updated)
	return value, err
}

func encodeAdoptionPlan(plan AdoptionJournalPlan) ([]byte, string, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return nil, "", fmt.Errorf("store: encode adoption journal: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(digest[:]), nil
}

func validateAdoptionKind(kind AdoptionKind) error {
	switch kind {
	case AdoptionFile, AdoptionHook, AdoptionRuntimeCLI, AdoptionRuntimeStdio:
		return nil
	default:
		return fmt.Errorf("store: invalid adoption kind %q", kind)
	}
}
