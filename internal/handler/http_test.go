package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
	"github.com/pmukhanova/webhook-delivery-service/internal/service"
)

type fakeWebhookService struct {
	createFunc func(context.Context, string, string, string, json.RawMessage) (model.WebhookJob, error)
	getFunc    func(context.Context, uuid.UUID) (model.WebhookJob, error)
	listFunc   func(context.Context, *model.Status, int) ([]model.WebhookJob, error)
	healthFunc func(context.Context) error
}

func (f *fakeWebhookService) Create(ctx context.Context, key, targetURL, eventType string, payload json.RawMessage) (model.WebhookJob, error) {
	return f.createFunc(ctx, key, targetURL, eventType, payload)
}

func (f *fakeWebhookService) Get(ctx context.Context, id uuid.UUID) (model.WebhookJob, error) {
	return f.getFunc(ctx, id)
}

func (f *fakeWebhookService) List(ctx context.Context, status *model.Status, limit int) ([]model.WebhookJob, error) {
	return f.listFunc(ctx, status, limit)
}

func (f *fakeWebhookService) Health(ctx context.Context) error {
	return f.healthFunc(ctx)
}

func TestCreateWebhook(t *testing.T) {
	jobID := uuid.New()
	validBody := `{"target_url":"https://example.com/hook","event_type":"payment.completed","payload":{"id":"123"}}`

	tests := []struct {
		name       string
		body       string
		key        string
		wantStatus int
		wantCode   string
	}{
		{name: "valid request", body: validBody, key: "request-1", wantStatus: http.StatusAccepted},
		{name: "missing idempotency key", body: validBody, wantStatus: http.StatusBadRequest, wantCode: "validation_error"},
		{name: "invalid target URL", body: `{"target_url":"ftp://example.com","event_type":"payment.completed","payload":{}}`, key: "request-2", wantStatus: http.StatusBadRequest, wantCode: "validation_error"},
		{name: "empty event type", body: `{"target_url":"https://example.com","event_type":" ","payload":{}}`, key: "request-3", wantStatus: http.StatusBadRequest, wantCode: "validation_error"},
		{name: "event type with newline", body: `{"target_url":"https://example.com","event_type":"created\ninvalid","payload":{}}`, key: "request-3b", wantStatus: http.StatusBadRequest, wantCode: "validation_error"},
		{name: "malformed request", body: `{"target_url":`, key: "request-4", wantStatus: http.StatusBadRequest, wantCode: "validation_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeWebhookService{
				createFunc: func(context.Context, string, string, string, json.RawMessage) (model.WebhookJob, error) {
					return model.WebhookJob{ID: jobID, Status: model.StatusPending}, nil
				},
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(test.body))
			request.Header.Set("Idempotency-Key", test.key)
			response := httptest.NewRecorder()

			newTestHandler(fake).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantCode != "" {
				assertErrorCode(t, response, test.wantCode)
			}
		})
	}
}

func TestCreateWebhookDuplicateIdempotencyKey(t *testing.T) {
	existingID := uuid.New()
	jobs := make(map[string]model.WebhookJob)
	fake := &fakeWebhookService{
		createFunc: func(_ context.Context, key, _, _ string, _ json.RawMessage) (model.WebhookJob, error) {
			if job, ok := jobs[key]; ok {
				return job, nil
			}
			job := model.WebhookJob{ID: existingID, Status: model.StatusPending}
			jobs[key] = job
			return job, nil
		},
	}
	router := newTestHandler(fake)
	body := `{"target_url":"https://example.com/hook","event_type":"created","payload":{}}`

	var ids []uuid.UUID
	for range 2 {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(body))
		request.Header.Set("Idempotency-Key", "same-key")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusAccepted)
		}
		var result createWebhookResponse
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		ids = append(ids, result.ID)
	}
	if ids[0] != ids[1] || len(jobs) != 1 {
		t.Fatalf("duplicate request produced different jobs: ids=%v count=%d", ids, len(jobs))
	}
}

func TestCreateWebhookIdempotencyConflict(t *testing.T) {
	fake := &fakeWebhookService{
		createFunc: func(context.Context, string, string, string, json.RawMessage) (model.WebhookJob, error) {
			return model.WebhookJob{}, service.ErrConflict
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(
		`{"target_url":"https://example.com/hook","event_type":"created","payload":{}}`,
	))
	request.Header.Set("Idempotency-Key", "conflicting-key")
	response := httptest.NewRecorder()

	newTestHandler(fake).ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	assertErrorCode(t, response, "conflict")
}

func TestGetWebhook(t *testing.T) {
	jobID := uuid.New()
	tests := []struct {
		name       string
		path       string
		serviceErr error
		wantStatus int
		wantCode   string
	}{
		{name: "found", path: "/api/v1/webhooks/" + jobID.String(), wantStatus: http.StatusOK},
		{name: "not found", path: "/api/v1/webhooks/" + jobID.String(), serviceErr: service.ErrNotFound, wantStatus: http.StatusNotFound, wantCode: "not_found"},
		{name: "invalid id", path: "/api/v1/webhooks/not-a-uuid", wantStatus: http.StatusBadRequest, wantCode: "validation_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeWebhookService{
				getFunc: func(context.Context, uuid.UUID) (model.WebhookJob, error) {
					return sampleJob(jobID), test.serviceErr
				},
			}
			response := httptest.NewRecorder()
			newTestHandler(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantCode != "" {
				assertErrorCode(t, response, test.wantCode)
			}
		})
	}
}

func TestListWebhooks(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantLimit  int
		wantFilter *model.Status
	}{
		{name: "valid limit", query: "?limit=42", wantStatus: http.StatusOK, wantLimit: 42},
		{name: "limit too large", query: "?limit=101", wantStatus: http.StatusBadRequest},
		{name: "invalid limit", query: "?limit=abc", wantStatus: http.StatusBadRequest},
		{name: "status filter", query: "?status=pending", wantStatus: http.StatusOK, wantLimit: 20, wantFilter: statusPointer(model.StatusPending)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeWebhookService{
				listFunc: func(_ context.Context, status *model.Status, limit int) ([]model.WebhookJob, error) {
					if limit != test.wantLimit {
						t.Errorf("limit = %d, want %d", limit, test.wantLimit)
					}
					if !equalStatus(status, test.wantFilter) {
						t.Errorf("status = %v, want %v", status, test.wantFilter)
					}
					return []model.WebhookJob{}, nil
				},
			}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks"+test.query, nil)
			newTestHandler(fake).ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

func TestHealthDatabaseUnavailable(t *testing.T) {
	fake := &fakeWebhookService{healthFunc: func(context.Context) error { return errors.New("database down") }}
	response := httptest.NewRecorder()
	newTestHandler(fake).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	assertErrorCode(t, response, "internal_error")
}

func newTestHandler(service webhookService) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(service, logger).Router()
}

func sampleJob(id uuid.UUID) model.WebhookJob {
	return model.WebhookJob{
		ID: id, TargetURL: "https://example.com", EventType: "created",
		Payload: json.RawMessage(`{}`), Status: model.StatusPending,
		MaxAttempts: 5, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func assertErrorCode(t *testing.T, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	var result errorEnvelope
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if result.Error.Code != want {
		t.Errorf("error code = %q, want %q", result.Error.Code, want)
	}
}

func statusPointer(status model.Status) *model.Status { return &status }

func equalStatus(left, right *model.Status) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
