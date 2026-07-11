package model

import "fmt"

var candidateTransitions = map[CandidateStatus]map[CandidateStatus]struct{}{
	CandidateAvailable:   set(CandidateStaging, CandidateSuperseded, CandidateFailed),
	CandidateStaging:     set(CandidateVerified, CandidateFailed, CandidateSuperseded),
	CandidateVerified:    set(CandidateMerging, CandidateFailed, CandidateSuperseded),
	CandidateMerging:     set(CandidateValidating, CandidateNeedsReview, CandidateFailed, CandidateSuperseded),
	CandidateValidating:  set(CandidateNeedsReview, CandidateReady, CandidateFailed, CandidateSuperseded),
	CandidateNeedsReview: set(CandidateReady, CandidateFailed, CandidateSuperseded),
	CandidateReady:       set(CandidateActivating, CandidateFailed, CandidateSuperseded),
	CandidateActivating:  set(CandidateActive, CandidateRolledBack, CandidateFailed),
	CandidateActive:      set(CandidateRolledBack),
}

func set(values ...CandidateStatus) map[CandidateStatus]struct{} {
	result := make(map[CandidateStatus]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func CanTransitionCandidate(from, to CandidateStatus) bool {
	_, ok := candidateTransitions[from][to]
	return ok
}

func ValidateCandidateTransition(from, to CandidateStatus) error {
	if err := from.Validate(); err != nil {
		return err
	}
	if err := to.Validate(); err != nil {
		return err
	}
	if !CanTransitionCandidate(from, to) {
		return fmt.Errorf("model: invalid candidate transition %q -> %q", from, to)
	}
	return nil
}
