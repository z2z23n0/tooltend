package hook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"time"
)

type Event struct {
	OccurredAt       time.Time
	Host             string
	EventType        string
	ProjectHash      string
	Installer        string
	PackageIdentity  string
	RequestedVersion string
	CorrelationHash  string
}

type EventSink interface {
	TryRecord(context.Context, Event) error
}

type NotificationSource interface {
	TakePending(context.Context, int) ([]string, error)
}

type Handler struct {
	Host          string
	Events        EventSink
	Notifications NotificationSource
	Now           func() time.Time
}

// Run is deliberately fail-open. Hook observations are an optimization; scans
// remain the source of truth, so malformed input or a busy database must never
// block the coding agent.
func (h Handler) Run(ctx context.Context, r io.Reader, w io.Writer) error {
	input, err := DecodeInput(r)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	if h.Now != nil {
		now = h.Now().UTC()
	}

	if h.Events != nil {
		intent, _ := ParseInstallerIntent(input)
		event := Event{
			OccurredAt:       now,
			Host:             h.Host,
			EventType:        input.HookEventName,
			ProjectHash:      digest(input.CWD),
			Installer:        intent.Installer,
			PackageIdentity:  intent.PackageIdentity,
			RequestedVersion: intent.RequestedVersion,
			CorrelationHash:  digest(input.SessionID + "\x00" + input.TurnID + "\x00" + input.ToolUseID + "\x00" + input.HookEventName),
		}
		_ = h.Events.TryRecord(ctx, event)
	}

	if input.HookEventName == "SessionStart" && h.Notifications != nil {
		messages, _ := h.Notifications.TakePending(ctx, 10)
		if len(messages) > 0 {
			payload := map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "SessionStart",
					"additionalContext": "ToolTend: " + joinMessages(messages),
				},
			}
			_ = json.NewEncoder(w).Encode(payload)
		}
	}
	return nil
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func joinMessages(messages []string) string {
	var result string
	for index, message := range messages {
		if message == "" {
			continue
		}
		if index > 0 && result != "" {
			result += "; "
		}
		result += message
	}
	return result
}
