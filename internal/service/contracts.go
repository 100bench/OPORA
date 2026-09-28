// Package service orchestrates bounded, device-isolated assistant sessions.
package service

import (
	"context"
	"time"

	"github.com/100bench/OPORA/internal/domain"
)

// Clock supplies deterministic wall time and timers.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

// Planner produces a structured response from sanitized input.
type Planner interface {
	Plan(context.Context, domain.PlanInput) (domain.PlanOutput, error)
}

// Transcriber converts a validated WAV payload to text.
type Transcriber interface {
	Transcribe(context.Context, []byte) (string, error)
}

// Assistant defines the application operations exposed to HTTP handlers.
type Assistant interface {
	CreateSession(context.Context, domain.SessionConfig) (domain.Session, error)
	Transcribe(context.Context, domain.Device, domain.TranscriptionRequest) (domain.Transcription, error)
	Turn(context.Context, domain.Device, string, domain.TurnRequest) (domain.Reply, error)
	Confirm(context.Context, domain.Device, string, domain.ConfirmationRequest) (domain.ConfirmationResult, error)
	ReportResult(context.Context, domain.Device, string, domain.ActionResultRequest) error
	Cancel(context.Context, domain.Device, string, domain.CancelRequest) error
	Ready(context.Context) error
}

// Config sets bounded in-memory and provider limits.
type Config struct {
	SessionTTL       time.Duration
	ConfirmationTTL  time.Duration
	MaxSessions      int
	MaxProviderCalls int
	ProviderTimeout  time.Duration
}

// DefaultConfig returns the production safety limits.
func DefaultConfig() Config {
	return Config{30 * time.Minute, 30 * time.Second, 100, 4, 20 * time.Second}
}
