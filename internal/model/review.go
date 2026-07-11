package model

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	reviewRiskPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	reviewSecretAssignment  = regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?key|token|secret|password|authorization)\s*[:=]\s*["']?[^\s"']{8,}`)
	reviewCredentialToken   = regexp.MustCompile(`(?i)(AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
	reviewEncodedCredential = regexp.MustCompile(`[A-Za-z0-9+/_=-]{64,}`)
	reviewCredentialURL     = regexp.MustCompile(`(?i)https?://[^\s/@:]+:[^\s/@]+@`)
)

func (r Review) Validate() error {
	if r.ID == "" || r.CandidateID == "" || r.CandidateHash == "" {
		return errors.New("review: id and candidate identity are required")
	}
	if err := r.ActorType.Validate(); err != nil {
		return err
	}
	if err := r.Verdict.Validate(); err != nil {
		return err
	}
	if !reviewRiskPattern.MatchString(r.RiskType) {
		return errors.New("review: invalid risk type")
	}
	if !utf8.ValidString(r.Summary) || strings.TrimSpace(r.Summary) == "" || len(r.Summary) > 1024 {
		return errors.New("review: summary must be 1-1024 bytes of UTF-8 text")
	}
	for _, value := range r.Summary {
		if unicode.IsControl(value) {
			return errors.New("review: summary contains control characters")
		}
	}
	data := []byte(r.Summary)
	if reviewSecretAssignment.Match(data) || reviewCredentialToken.Match(data) || reviewEncodedCredential.Match(data) || reviewCredentialURL.Match(data) {
		return errors.New("review: summary appears to contain a credential")
	}
	return nil
}
