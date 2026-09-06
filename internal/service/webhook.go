package service

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/pmukhanova/webhook-delivery-service/internal/model"
	"github.com/pmukhanova/webhook-delivery-service/internal/repository"
)

var (
	ErrNotFound = repository.ErrNotFound
	ErrConflict = repository.ErrIdempotencyConflict
)

type WebhookService struct {
	repository *repository.WebhookRepository
}

func NewWebhookService(repository *repository.WebhookRepository) *WebhookService {
	return &WebhookService{repository: repository}
}

func (s *WebhookService) Create(ctx context.Context, idempotencyKey, targetURL, eventType string, payload json.RawMessage) (model.WebhookJob, error) {
	return s.repository.Create(ctx, model.CreateWebhook{
		ID:             uuid.New(),
		IdempotencyKey: idempotencyKey,
		TargetURL:      targetURL,
		EventType:      eventType,
		Payload:        payload,
	})
}

func (s *WebhookService) Get(ctx context.Context, id uuid.UUID) (model.WebhookJob, error) {
	return s.repository.GetByID(ctx, id)
}

func (s *WebhookService) List(ctx context.Context, status *model.Status, limit int) ([]model.WebhookJob, error) {
	return s.repository.List(ctx, status, limit)
}

func (s *WebhookService) Health(ctx context.Context) error {
	return s.repository.Ping(ctx)
}
