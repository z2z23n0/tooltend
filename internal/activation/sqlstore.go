package activation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/z2z23n0/tooltend/internal/model"
	storepkg "github.com/z2z23n0/tooltend/internal/store"
)

// SQLStore adapts the repository SQLite store to the activation journal. The
// pointer_switched phase is durable so recovery can distinguish a prepared
// journal from a filesystem pointer that was already replaced.
type SQLStore struct {
	db  *sql.DB
	now func() time.Time
}

var _ Store = (*SQLStore)(nil)

func NewSQLStore(value *storepkg.Store) (*SQLStore, error) {
	if value == nil || value.DB() == nil {
		return nil, errors.New("activation: SQLite store is required")
	}
	return &SQLStore{db: value.DB(), now: time.Now}, nil
}

func (s *SQLStore) SaveIntent(ctx context.Context, intent Intent) error {
	pointer, err := encodeExpectedPointer(intent)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("activation: begin intent transaction: %w", err)
	}
	defer tx.Rollback()

	existing, found, err := loadIntent(ctx, tx, intent.ID)
	if err != nil {
		return err
	}
	if found {
		if !sameIntent(existing, intent) {
			return errors.New("activation: intent ID already has different content")
		}
		return tx.Commit()
	}
	var activeGeneration sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT active_generation_id FROM bindings WHERE id=?`, intent.BindingID).Scan(&activeGeneration); err != nil {
		return fmt.Errorf("activation: load binding generation: %w", err)
	}
	if activeGeneration.String != intent.OldGeneration {
		return fmt.Errorf("activation: binding active generation changed: got %q, expected %q", activeGeneration.String, intent.OldGeneration)
	}

	var candidateHash, status string
	if err := tx.QueryRowContext(ctx, `SELECT candidate_hash,status FROM candidates WHERE id=? AND binding_id=?`, intent.CandidateID, intent.BindingID).Scan(&candidateHash, &status); err != nil {
		return fmt.Errorf("activation: load candidate: %w", err)
	}
	if candidateHash != intent.CandidateHash {
		return errors.New("activation: candidate hash mismatch")
	}
	if status != string(model.CandidateReady) {
		return fmt.Errorf("activation: candidate is %s, expected ready", status)
	}
	if err := requireGeneration(ctx, tx, intent.NewGeneration, intent.BindingID); err != nil {
		return err
	}
	if intent.OldGeneration != "" {
		if err := requireGeneration(ctx, tx, intent.OldGeneration, intent.BindingID); err != nil {
			return err
		}
	}

	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO activation_intents(id,binding_id,candidate_id,old_generation_id,new_generation_id,expected_pointer,phase,error_code,started_at,updated_at)
		VALUES(?,?,?,?,?,?,'prepared','',?,?)`, intent.ID, intent.BindingID, intent.CandidateID, nullable(intent.OldGeneration), intent.NewGeneration, pointer, now, now); err != nil {
		return fmt.Errorf("activation: insert intent: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE candidates SET status='activating',updated_at=? WHERE id=? AND binding_id=? AND status='ready'`, now, intent.CandidateID, intent.BindingID)
	if err != nil {
		return fmt.Errorf("activation: mark candidate activating: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return errors.New("activation: candidate transition raced")
	}
	return tx.Commit()
}

func (s *SQLStore) SetPhase(ctx context.Context, intentID string, phase Phase, _ string) error {
	switch phase {
	case PhasePointerSwitched:
		result, err := s.db.ExecContext(ctx, `UPDATE activation_intents SET phase='pointer_switched',error_code='',updated_at=? WHERE id=? AND phase IN ('prepared','pointer_switched')`, s.nowText(), intentID)
		if err != nil {
			return fmt.Errorf("activation: mark pointer switched: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 1 {
			return nil
		}
		var current string
		if err := s.db.QueryRowContext(ctx, `SELECT phase FROM activation_intents WHERE id=?`, intentID).Scan(&current); err != nil {
			return err
		}
		if current == "committed" || current == "pointer_switched" {
			return nil
		}
		return fmt.Errorf("activation: cannot mark pointer switched from %s", current)
	case PhaseRolledBack:
		return s.markRolledBack(ctx, intentID)
	case PhasePrepared:
		return nil
	case PhaseCommitted:
		return errors.New("activation: committed phase requires Complete")
	default:
		return fmt.Errorf("activation: unsupported phase %q", phase)
	}
}

func (s *SQLStore) Complete(ctx context.Context, intentID string, receipt Receipt) (Receipt, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()

	intent, found, err := loadIntent(ctx, tx, intentID)
	if err != nil {
		return Receipt{}, err
	}
	if !found {
		return Receipt{}, sql.ErrNoRows
	}
	if existing, found, err := loadReceipt(ctx, tx, intentID); err != nil {
		return Receipt{}, err
	} else if found {
		if !sameReceiptIntent(existing, intent) {
			return Receipt{}, errors.New("activation: receipt ID already has different content")
		}
		if err := tx.Commit(); err != nil {
			return Receipt{}, err
		}
		return existing, nil
	}
	var databasePhase string
	if err := tx.QueryRowContext(ctx, `SELECT phase FROM activation_intents WHERE id=?`, intentID).Scan(&databasePhase); err != nil {
		return Receipt{}, err
	}
	if databasePhase == "rolled_back" {
		return Receipt{}, errors.New("activation: rolled-back intent cannot commit")
	}
	if !sameReceiptIntent(receipt, intent) {
		return Receipt{}, errors.New("activation: receipt does not match intent")
	}

	now := receipt.ActivatedAt.UTC()
	if now.IsZero() {
		now = s.now().UTC()
	}
	nowText := now.Format(time.RFC3339Nano)
	if intent.OldGeneration != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE generations SET state='inactive' WHERE id=? AND binding_id=?`, intent.OldGeneration, intent.BindingID); err != nil {
			return Receipt{}, err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE generations SET state='active',candidate_id=?,activated_at=? WHERE id=? AND binding_id=?`, intent.CandidateID, nowText, intent.NewGeneration, intent.BindingID)
	if err != nil {
		return Receipt{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return Receipt{}, err
		}
		return Receipt{}, errors.New("activation: new generation is missing")
	}
	result, err = tx.ExecContext(ctx, `UPDATE bindings SET active_generation_id=? WHERE id=? AND COALESCE(active_generation_id,'')=?`,
		intent.NewGeneration, intent.BindingID, intent.OldGeneration)
	if err != nil {
		return Receipt{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return Receipt{}, err
		}
		return Receipt{}, errors.New("activation: binding generation changed before commit")
	}
	result, err = tx.ExecContext(ctx, `UPDATE candidates SET status='active',updated_at=? WHERE id=? AND binding_id=? AND status IN ('activating','active')`, nowText, intent.CandidateID, intent.BindingID)
	if err != nil {
		return Receipt{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return Receipt{}, err
		}
		return Receipt{}, errors.New("activation: candidate is not activating")
	}
	if intent.Completion.Baseline != nil {
		baseline := intent.Completion.Baseline
		result, err = tx.ExecContext(ctx, `INSERT INTO baselines(id,binding_id,source_id,resolved_ref,tree_hash,created_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET id=excluded.id
			WHERE baselines.binding_id=excluded.binding_id AND baselines.source_id=excluded.source_id AND baselines.resolved_ref=excluded.resolved_ref AND baselines.tree_hash=excluded.tree_hash`,
			baseline.ID, intent.BindingID, baseline.SourceID, baseline.ResolvedRef, baseline.TreeHash, nowText)
		if err != nil {
			return Receipt{}, fmt.Errorf("activation: insert completion baseline: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return Receipt{}, err
			}
			return Receipt{}, errors.New("activation: baseline identity changed")
		}
	}
	if oldCandidate := intent.Completion.RolledBackCandidateID; oldCandidate != "" && oldCandidate != intent.CandidateID {
		result, err = tx.ExecContext(ctx, `UPDATE candidates SET status='rolled_back',failure_code='',failure_summary='',updated_at=?
			WHERE id=? AND binding_id=? AND status IN ('active','rolled_back')`, nowText, oldCandidate, intent.BindingID)
		if err != nil {
			return Receipt{}, fmt.Errorf("activation: mark previous candidate rolled back: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return Receipt{}, err
			}
			return Receipt{}, errors.New("activation: previous active candidate changed")
		}
	}

	action := normalizedCompletionAction(intent.Completion.Action)
	summaryValue := receiptSummary{GenerationHash: receipt.GenerationHash, Recovered: receipt.Recovered}
	if action == CompletionRollback {
		summaryValue.ActivationIntent = intent.ID
		summaryValue.PreservedGeneration = intent.OldGeneration
	}
	summary, err := json.Marshal(summaryValue)
	if err != nil {
		return Receipt{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipts(id,binding_id,candidate_id,action,old_generation_id,new_generation_id,from_ref,to_ref,candidate_hash,status,summary_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'succeeded',?,?)`, intentID, intent.BindingID, intent.CandidateID, action,
		nullable(intent.OldGeneration), intent.NewGeneration, intent.Completion.FromRef, intent.Completion.ToRef,
		intent.CandidateHash, string(summary), nowText); err != nil {
		return Receipt{}, fmt.Errorf("activation: insert receipt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE activation_intents SET phase='committed',error_code='',updated_at=? WHERE id=?`, nowText, intentID); err != nil {
		return Receipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return Receipt{}, err
	}
	receipt.ID = intentID
	receipt.ActivatedAt = now
	return receipt, nil
}

func (s *SQLStore) Pending(ctx context.Context) ([]Intent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ai.id,ai.binding_id,ai.candidate_id,ai.old_generation_id,ai.new_generation_id,ai.expected_pointer,ai.phase,c.candidate_hash
		FROM activation_intents ai JOIN candidates c ON c.id=ai.candidate_id WHERE ai.phase IN ('prepared','pointer_switched') ORDER BY ai.started_at,ai.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Intent
	for rows.Next() {
		var intent Intent
		var old sql.NullString
		var encoded, phase string
		if err := rows.Scan(&intent.ID, &intent.BindingID, &intent.CandidateID, &old, &intent.NewGeneration, &encoded, &phase, &intent.CandidateHash); err != nil {
			return nil, err
		}
		intent.OldGeneration = old.String
		if err := decodeExpectedPointer(encoded, &intent); err != nil {
			return nil, fmt.Errorf("activation: decode intent %s: %w", intent.ID, err)
		}
		intent.Phase = PhasePrepared
		if phase == string(PhasePointerSwitched) {
			intent.Phase = PhasePointerSwitched
		}
		result = append(result, intent)
	}
	return result, rows.Err()
}

func (s *SQLStore) markRolledBack(ctx context.Context, intentID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	intent, found, err := loadIntent(ctx, tx, intentID)
	if err != nil {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	var phase string
	if err := tx.QueryRowContext(ctx, `SELECT phase FROM activation_intents WHERE id=?`, intentID).Scan(&phase); err != nil {
		return err
	}
	if phase == "committed" {
		return errors.New("activation: committed intent cannot roll back through recovery")
	}
	if phase == "rolled_back" {
		return tx.Commit()
	}
	now := s.nowText()
	if _, err := tx.ExecContext(ctx, `UPDATE activation_intents SET phase='rolled_back',error_code='activation_rolled_back',updated_at=? WHERE id=?`, now, intentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE candidates SET status='rolled_back',failure_code='activation_rolled_back',failure_summary='',updated_at=? WHERE id=? AND status='activating'`, now, intent.CandidateID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE generations SET state='failed' WHERE id=? AND binding_id=? AND state!='active'`, intent.NewGeneration, intent.BindingID); err != nil {
		return err
	}
	summary := `{"reason_code":"activation_rolled_back"}`
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO receipts(id,binding_id,candidate_id,action,old_generation_id,new_generation_id,from_ref,to_ref,candidate_hash,status,summary_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,'rolled_back',?,?)`, intentID+"-rollback", intent.BindingID, intent.CandidateID,
		normalizedCompletionAction(intent.Completion.Action), nullable(intent.OldGeneration), intent.NewGeneration,
		intent.Completion.FromRef, intent.Completion.ToRef, intent.CandidateHash, summary, now); err != nil {
		return err
	}
	return tx.Commit()
}

type expectedPointer struct {
	Target            string     `json:"target"`
	GenerationHash    string     `json:"generation_hash"`
	OldGenerationHash string     `json:"old_generation_hash,omitempty"`
	CandidateHash     string     `json:"candidate_hash"`
	Completion        Completion `json:"completion"`
}

type receiptSummary struct {
	GenerationHash      string `json:"generation_hash"`
	Recovered           bool   `json:"recovered,omitempty"`
	ActivationIntent    string `json:"activation_intent,omitempty"`
	PreservedGeneration string `json:"preserved_generation,omitempty"`
}

func encodeExpectedPointer(intent Intent) (string, error) {
	completion := intent.Completion
	completion.Action = normalizedCompletionAction(completion.Action)
	if intent.OldGeneration != "" && intent.ExpectedOldGenerationHash == "" {
		return "", errors.New("activation: expected old generation hash is required")
	}
	if err := validateCompletion(completion); err != nil {
		return "", err
	}
	value := expectedPointer{
		Target:            "generations/" + intent.NewGeneration,
		GenerationHash:    intent.ExpectedGenerationHash,
		OldGenerationHash: intent.ExpectedOldGenerationHash,
		CandidateHash:     intent.CandidateHash,
		Completion:        completion,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func decodeExpectedPointer(encoded string, intent *Intent) error {
	var value expectedPointer
	if err := json.Unmarshal([]byte(encoded), &value); err != nil {
		return err
	}
	if value.Target != "generations/"+intent.NewGeneration || value.GenerationHash == "" || value.CandidateHash == "" {
		return errors.New("invalid expected pointer payload")
	}
	if intent.CandidateHash != "" && intent.CandidateHash != value.CandidateHash {
		return errors.New("expected pointer candidate hash mismatch")
	}
	intent.ExpectedGenerationHash = value.GenerationHash
	intent.ExpectedOldGenerationHash = value.OldGenerationHash
	intent.CandidateHash = value.CandidateHash
	intent.Completion = value.Completion
	intent.Completion.Action = normalizedCompletionAction(intent.Completion.Action)
	if intent.OldGeneration != "" && intent.ExpectedOldGenerationHash == "" {
		return errors.New("expected pointer old generation hash is missing")
	}
	if err := validateCompletion(intent.Completion); err != nil {
		return err
	}
	return nil
}

func loadIntent(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Intent, bool, error) {
	var intent Intent
	var old sql.NullString
	var encoded, databasePhase, errorCode string
	err := query.QueryRowContext(ctx, `SELECT ai.id,ai.binding_id,ai.candidate_id,ai.old_generation_id,ai.new_generation_id,ai.expected_pointer,ai.phase,ai.error_code,c.candidate_hash
		FROM activation_intents ai JOIN candidates c ON c.id=ai.candidate_id WHERE ai.id=?`, id).
		Scan(&intent.ID, &intent.BindingID, &intent.CandidateID, &old, &intent.NewGeneration, &encoded, &databasePhase, &errorCode, &intent.CandidateHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, false, nil
	}
	if err != nil {
		return Intent{}, false, err
	}
	intent.OldGeneration = old.String
	if err := decodeExpectedPointer(encoded, &intent); err != nil {
		return Intent{}, false, err
	}
	switch databasePhase {
	case "prepared":
		intent.Phase = PhasePrepared
	case "pointer_switched":
		intent.Phase = PhasePointerSwitched
	case "committed":
		intent.Phase = PhaseCommitted
	case "rolled_back":
		intent.Phase = PhaseRolledBack
	default:
		return Intent{}, false, fmt.Errorf("activation: invalid database phase %q", databasePhase)
	}
	return intent, true, nil
}

func loadReceipt(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (Receipt, bool, error) {
	var receipt Receipt
	var old sql.NullString
	var created, summaryJSON string
	err := query.QueryRowContext(ctx, `SELECT id,binding_id,candidate_id,action,old_generation_id,new_generation_id,from_ref,to_ref,candidate_hash,summary_json,created_at FROM receipts WHERE id=? AND status='succeeded'`, id).
		Scan(&receipt.ID, &receipt.BindingID, &receipt.CandidateID, &receipt.Action, &old, &receipt.NewGeneration, &receipt.FromRef, &receipt.ToRef, &receipt.CandidateHash, &summaryJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, err
	}
	receipt.IntentID = id
	receipt.OldGeneration = old.String
	var summary receiptSummary
	if err := json.Unmarshal([]byte(summaryJSON), &summary); err != nil {
		return Receipt{}, false, err
	}
	receipt.GenerationHash = summary.GenerationHash
	receipt.Recovered = summary.Recovered
	receipt.ActivatedAt, err = time.Parse(time.RFC3339Nano, created)
	return receipt, true, err
}

func requireGeneration(ctx context.Context, tx *sql.Tx, generationID, bindingID string) error {
	var found string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM generations WHERE id=? AND binding_id=?`, generationID, bindingID).Scan(&found); err != nil {
		return fmt.Errorf("activation: generation %s unavailable: %w", generationID, err)
	}
	return nil
}

func sameIntent(left, right Intent) bool {
	return left.ID == right.ID && left.BindingID == right.BindingID && left.CandidateID == right.CandidateID &&
		left.CandidateHash == right.CandidateHash && left.OldGeneration == right.OldGeneration &&
		left.NewGeneration == right.NewGeneration && left.ExpectedGenerationHash == right.ExpectedGenerationHash &&
		left.ExpectedOldGenerationHash == right.ExpectedOldGenerationHash && sameCompletion(left.Completion, right.Completion)
}

func sameReceiptIntent(receipt Receipt, intent Intent) bool {
	return receipt.IntentID == intent.ID && receipt.BindingID == intent.BindingID && receipt.CandidateID == intent.CandidateID &&
		receipt.CandidateHash == intent.CandidateHash && receipt.OldGeneration == intent.OldGeneration &&
		receipt.NewGeneration == intent.NewGeneration && receipt.GenerationHash == intent.ExpectedGenerationHash &&
		receipt.Action == normalizedCompletionAction(intent.Completion.Action) && receipt.FromRef == intent.Completion.FromRef && receipt.ToRef == intent.Completion.ToRef
}

func sameCompletion(left, right Completion) bool {
	left.Action = normalizedCompletionAction(left.Action)
	right.Action = normalizedCompletionAction(right.Action)
	if left.Action != right.Action || left.FromRef != right.FromRef || left.ToRef != right.ToRef || left.RolledBackCandidateID != right.RolledBackCandidateID {
		return false
	}
	if left.Baseline == nil || right.Baseline == nil {
		return left.Baseline == nil && right.Baseline == nil
	}
	return *left.Baseline == *right.Baseline
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *SQLStore) nowText() string { return s.now().UTC().Format(time.RFC3339Nano) }
