package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusRetry      Status = "retry"
	StatusDelivered  Status = "delivered"
	StatusFailed     Status = "failed"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusProcessing, StatusRetry, StatusDelivered, StatusFailed:
		return true
	default:
		return false
	}
}

type WebhookJob struct {
	ID                  uuid.UUID       `json:"id"`
	TargetURL           string          `json:"target_url"`
	EventType           string          `json:"event_type"`
	Payload             json.RawMessage `json:"payload"`
	Status              Status          `json:"status"`
	Attempts            int             `json:"attempts"`
	MaxAttempts         int             `json:"max_attempts"`
	NextAttemptAt       *time.Time      `json:"next_attempt_at,omitempty"`
	LastError           *string         `json:"last_error,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	DeliveredAt         *time.Time      `json:"delivered_at,omitempty"`
	IdempotencyKey      string          `json:"-"`
	ClaimToken          *uuid.UUID      `json:"-"`
	ProcessingStartedAt *time.Time      `json:"-"`
}

type CreateWebhook struct {
	ID             uuid.UUID
	IdempotencyKey string
	TargetURL      string
	EventType      string
	Payload        json.RawMessage
}
