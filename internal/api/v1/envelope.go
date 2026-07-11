package v1

import (
	"encoding/json"
	"fmt"
	"io"
)

const SchemaVersion = 1

type Warning struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details,omitempty"`
}

type Envelope struct {
	SchemaVersion int       `json:"schema_version"`
	Command       string    `json:"command"`
	OK            bool      `json:"ok"`
	Data          any       `json:"data,omitempty"`
	Warnings      []Warning `json:"warnings"`
	Error         *Error    `json:"error,omitempty"`
}

func Success(command string, data any, warnings ...Warning) Envelope {
	if warnings == nil {
		warnings = []Warning{}
	}
	return Envelope{SchemaVersion: SchemaVersion, Command: command, OK: true, Data: data, Warnings: warnings}
}

func Failure(command, code, message string, retryable bool, details map[string]any, warnings ...Warning) Envelope {
	if warnings == nil {
		warnings = []Warning{}
	}
	return Envelope{
		SchemaVersion: SchemaVersion,
		Command:       command,
		OK:            false,
		Warnings:      warnings,
		Error:         &Error{Code: code, Message: message, Retryable: retryable, Details: details},
	}
}

func (e Envelope) Validate() error {
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("api/v1: schema version must be %d", SchemaVersion)
	}
	if e.Command == "" {
		return fmt.Errorf("api/v1: command is required")
	}
	if e.OK && e.Error != nil {
		return fmt.Errorf("api/v1: successful envelope cannot contain error")
	}
	if !e.OK && e.Error == nil {
		return fmt.Errorf("api/v1: failed envelope requires error")
	}
	return nil
}

func Write(w io.Writer, envelope Envelope) error {
	if err := envelope.Validate(); err != nil {
		return err
	}
	if envelope.Warnings == nil {
		envelope.Warnings = []Warning{}
	}
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(envelope)
}
