package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/z2z23n0/tooltend/internal/lifecycle"
	"github.com/z2z23n0/tooltend/internal/model"
	"github.com/z2z23n0/tooltend/internal/objectstore"
	"github.com/z2z23n0/tooltend/internal/plan"
	"github.com/z2z23n0/tooltend/internal/store"
)

type pendingReview struct {
	ComponentID   string                `json:"component_id"`
	ComponentName string                `json:"component_name"`
	BindingID     string                `json:"binding_id"`
	Candidate     model.UpdateCandidate `json:"candidate"`
	BundleID      string                `json:"bundle_id,omitempty"`
	BundleObject  string                `json:"bundle_object_hash,omitempty"`
	RiskTypesJSON string                `json:"risk_types_json,omitempty"`
	Bundle        json.RawMessage       `json:"bundle,omitempty"`
}

type reviewOptions struct {
	CandidateID   string
	CandidateHash string
	Verdict       string
	RiskType      string
	Summary       string
	Actor         string
}

func (a *App) newReviewCommand() *cobra.Command {
	var options reviewOptions
	command := &cobra.Command{Use: "review [component]", Short: "List pending reviews or submit a candidate-bound verdict", Args: cobra.MaximumNArgs(1)}
	command.Flags().StringVar(&options.CandidateID, "candidate-id", "", "candidate ID from the review bundle")
	command.Flags().StringVar(&options.CandidateHash, "candidate-hash", "", "exact candidate content hash")
	command.Flags().StringVar(&options.Verdict, "verdict", "", "safe, conflict, or uncertain")
	command.Flags().StringVar(&options.RiskType, "risk-type", "", "reviewed risk category")
	command.Flags().StringVar(&options.Summary, "summary", "", "short review evidence summary")
	command.Flags().StringVar(&options.Actor, "actor", "", "human or agent")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		return a.run("review", func(ctx context.Context) (any, error) {
			paths, err := a.paths()
			if err != nil {
				return nil, err
			}
			submitting, err := validateReviewOptions(options)
			if err != nil {
				return nil, err
			}
			database, err := a.openReadOnly(paths)
			if err != nil {
				return nil, err
			}
			defer database.Close()
			var componentID string
			if len(args) == 1 {
				component, resolveErr := resolveComponent(ctx, database, args[0])
				if resolveErr != nil {
					return nil, resolveErr
				}
				componentID = component.ID
			}
			if !submitting {
				objects, objectErr := objectstore.New(paths.ObjectsDir)
				if objectErr != nil {
					return nil, objectErr
				}
				return listPendingReviews(ctx, database, objects, componentID)
			}
			if options.Actor == string(model.ActorHuman) && !a.global.DryRun && !a.humanReviewAuthorized() {
				return nil, cliError("human_presence_required", "human review requires an interactive terminal without --json or --yes", nil)
			}
			candidate, err := database.GetCandidate(ctx, options.CandidateID)
			if err != nil {
				return nil, err
			}
			if candidate.CandidateHash != options.CandidateHash {
				return nil, cliError("candidate_hash_mismatch", "candidate hash does not match the stored candidate", nil)
			}
			bundle, err := database.GetReviewBundle(ctx, candidate.ID)
			if err != nil {
				return nil, err
			}
			if err := validateReviewRisk(bundle, candidate, options.RiskType); err != nil {
				return nil, err
			}
			if componentID != "" {
				binding, getErr := database.GetBinding(ctx, candidate.BindingID)
				if getErr != nil {
					return nil, getErr
				}
				if binding.ComponentID != componentID {
					return nil, cliError("candidate_component_mismatch", "candidate does not belong to the selected component", nil)
				}
			}
			database.Close()
			id, err := model.NewID("review")
			if err != nil {
				return nil, err
			}
			review := model.Review{
				ID: id, CandidateID: options.CandidateID, CandidateHash: options.CandidateHash,
				ActorType: model.ActorType(options.Actor), Verdict: model.ReviewVerdict(options.Verdict),
				RiskType: options.RiskType, Summary: options.Summary, CreatedAt: time.Now().UTC(),
			}
			if err := review.Validate(); err != nil {
				return nil, cliError("invalid_argument", err.Error(), err)
			}
			value := plan.Plan{ID: "review-submit-v1", Title: "Submit a candidate-bound review verdict", Operations: []plan.Operation{
				plan.FuncOperation{
					Description: plan.OperationPreview{
						ID: "submit-review", Kind: plan.OperationDatabase, Target: options.CandidateID,
						Summary:              "Record a hash-bound review without changing the active generation",
						RequiresConfirmation: true,
						Details: map[string]string{
							"candidate_hash": options.CandidateHash, "bundle_object_hash": bundle.ObjectHash,
							"verdict": options.Verdict, "risk_type": options.RiskType, "actor": options.Actor,
						},
					},
					ApplyFunc: func(ctx context.Context) error {
						return withLifecycleStateLock(ctx, paths, func(db *store.Store) error {
							currentBundle, bundleErr := db.GetReviewBundle(ctx, candidate.ID)
							if bundleErr != nil {
								return bundleErr
							}
							if currentBundle.ObjectHash != bundle.ObjectHash || currentBundle.RiskTypesJSON != bundle.RiskTypesJSON {
								return cliError("review_bundle_changed", "review bundle changed after confirmation; inspect the new bundle before submitting", nil)
							}
							if riskErr := validateReviewRisk(currentBundle, candidate, options.RiskType); riskErr != nil {
								return riskErr
							}
							return db.SubmitReview(ctx, review)
						})
					},
				},
			}}
			return a.applyPlan(ctx, value, func() any { return review })
		})(cmd, args)
	}
	return command
}

func (a *App) humanReviewAuthorized() bool {
	if a.global.JSON || a.global.Yes {
		return false
	}
	in, inOK := a.in.(*os.File)
	out, outOK := a.out.(*os.File)
	if !inOK || !outOK {
		return false
	}
	inputTerminal := isatty.IsTerminal(in.Fd()) || isatty.IsCygwinTerminal(in.Fd())
	outputTerminal := isatty.IsTerminal(out.Fd()) || isatty.IsCygwinTerminal(out.Fd())
	return inputTerminal && outputTerminal
}

func validateReviewOptions(options reviewOptions) (bool, error) {
	values := []string{options.CandidateID, options.CandidateHash, options.Verdict, options.RiskType, options.Summary, options.Actor}
	count := 0
	for _, value := range values {
		if value != "" {
			count++
		}
	}
	if count == 0 {
		return false, nil
	}
	if count != len(values) {
		return false, cliError("invalid_argument", "review submission requires --candidate-id, --candidate-hash, --verdict, --risk-type, --summary, and --actor", nil)
	}
	verdict := model.ReviewVerdict(options.Verdict)
	if err := verdict.Validate(); err != nil {
		return false, cliError("invalid_argument", err.Error(), err)
	}
	actor := model.ActorType(options.Actor)
	if err := actor.Validate(); err != nil {
		return false, cliError("invalid_argument", err.Error(), err)
	}
	if !validReviewRiskType(options.RiskType) {
		return false, cliError("invalid_argument", "review risk type must be 1-64 lowercase ASCII letters, digits, or underscores and start with a letter", nil)
	}
	if !utf8.ValidString(options.Summary) || strings.TrimSpace(options.Summary) == "" || len(options.Summary) > 1024 {
		return false, cliError("invalid_argument", "review summary must be valid UTF-8 text between 1 and 1024 bytes", nil)
	}
	for _, value := range options.Summary {
		if unicode.IsControl(value) {
			return false, cliError("invalid_argument", "review summary must not contain control characters", nil)
		}
	}
	return true, nil
}

func validReviewRiskType(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, char := range value[1:] {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func validateReviewRisk(bundle model.ReviewBundle, candidate model.UpdateCandidate, requested string) error {
	if bundle.CandidateID != candidate.ID || bundle.CandidateHash != candidate.CandidateHash {
		return cliError("review_bundle_mismatch", "review bundle identity does not match the stored candidate", nil)
	}
	if len(bundle.RiskTypesJSON) > lifecycle.MaxReviewBundleBytes {
		return cliError("review_bundle_invalid", "review bundle risk list exceeds the bounded evidence size", nil)
	}
	var allowed []string
	if err := json.Unmarshal([]byte(bundle.RiskTypesJSON), &allowed); err != nil {
		return cliError("review_bundle_invalid", "review bundle risk list is not valid JSON", err)
	}
	for _, risk := range allowed {
		if risk == requested {
			return nil
		}
	}
	return cliError("invalid_argument", "review risk type is not present in the candidate review bundle", nil)
}

func listPendingReviews(ctx context.Context, database *store.Store, objects *objectstore.Store, componentID string) ([]pendingReview, error) {
	stored, err := database.ListPendingReviews(ctx, componentID)
	if err != nil {
		return nil, err
	}
	result := make([]pendingReview, 0, len(stored))
	for _, storedItem := range stored {
		item := pendingReview{
			ComponentID: storedItem.ComponentID, ComponentName: storedItem.ComponentName,
			BindingID: storedItem.BindingID, Candidate: storedItem.Candidate,
		}
		if storedItem.Bundle != nil {
			if storedItem.Bundle.CandidateID != storedItem.Candidate.ID || storedItem.Bundle.CandidateHash != storedItem.Candidate.CandidateHash {
				return nil, fmt.Errorf("stored review bundle identity does not match candidate %s", storedItem.Candidate.ID)
			}
			item.BundleID = storedItem.Bundle.ID
			item.BundleObject = storedItem.Bundle.ObjectHash
			item.RiskTypesJSON = storedItem.Bundle.RiskTypesJSON
		}
		if item.BundleObject != "" {
			reader, openErr := objects.OpenBlob(item.BundleObject)
			if openErr != nil {
				return nil, openErr
			}
			payload, readErr := io.ReadAll(io.LimitReader(reader, lifecycle.MaxReviewBundleBytes+1))
			closeErr := reader.Close()
			if readErr != nil {
				return nil, readErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if len(payload) > lifecycle.MaxReviewBundleBytes {
				return nil, fmt.Errorf("review bundle exceeds %d bytes", lifecycle.MaxReviewBundleBytes)
			}
			var identity struct {
				Version       int    `json:"version"`
				CandidateID   string `json:"candidate_id"`
				CandidateHash string `json:"candidate_hash"`
			}
			if decodeErr := json.Unmarshal(payload, &identity); decodeErr != nil {
				return nil, fmt.Errorf("decode review bundle: %w", decodeErr)
			}
			if identity.Version != 1 || identity.CandidateID != item.Candidate.ID || identity.CandidateHash != item.Candidate.CandidateHash {
				return nil, fmt.Errorf("review bundle identity does not match candidate %s", item.Candidate.ID)
			}
			item.Bundle = append(json.RawMessage(nil), payload...)
		}
		result = append(result, item)
	}
	return result, nil
}
