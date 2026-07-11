package model

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func NewID(prefix string) (string, error) {
	if prefix == "" || len(prefix) > 12 {
		return "", fmt.Errorf("model: invalid id prefix %q", prefix)
	}
	for _, r := range prefix {
		if (r < 'a' || r > 'z') && r != '_' {
			return "", fmt.Errorf("model: invalid id prefix %q", prefix)
		}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("model: generate id: %w", err)
	}
	return prefix + "_" + strings.ToLower(idEncoding.EncodeToString(b)), nil
}
