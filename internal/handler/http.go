package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
	"github.com/pmukhanova/webhook-delivery-service/internal/service"
	"github.com/pmukhanova/webhook-delivery-service/internal/targetpolicy"
)

const maxRequestBodySize = 1 << 20

type webhookService interface {
	Create(context.Context, string, string, string, json.RawMessage) (model.WebhookJob, error)
	Get(context.Context, uuid.UUID) (model.WebhookJob, error)
	List(context.Context, *model.Status, int) ([]model.WebhookJob, error)
	Health(context.Context) error
}

type Handler struct {
	service webhookService
	logger  *slog.Logger
}

func New(service webhookService, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

func (h *Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "validation_error", "method not allowed")
	})
	router.Get("/health", h.health)
	router.Post("/api/v1/webhooks", h.createWebhook)
	router.Get("/api/v1/webhooks", h.listWebhooks)
	router.Get("/api/v1/webhooks/{id}", h.getWebhook)
	return router
}

type createWebhookRequest struct {
	TargetURL string          `json:"target_url"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

type createWebhookResponse struct {
	ID     uuid.UUID    `json:"id"`
	Status model.Status `json:"status"`
}

func (h *Handler) createWebhook(w http.ResponseWriter, r *http.Request) {
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "Idempotency-Key header is required")
		return
	}
	if len(idempotencyKey) > 255 {
		writeError(w, http.StatusBadRequest, "validation_error", "Idempotency-Key must not exceed 255 characters")
		return
	}

	var request createWebhookRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBodySize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "request body must be valid JSON")
		return
	}
	if err := ensureEndOfJSON(decoder); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "request body must contain a single JSON object")
		return
	}

	request.TargetURL = strings.TrimSpace(request.TargetURL)
	request.EventType = strings.TrimSpace(request.EventType)
	if err := validateCreateRequest(request); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	job, err := h.service.Create(r.Context(), idempotencyKey, request.TargetURL, request.EventType, request.Payload)
	if err != nil {
		h.handleInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, createWebhookResponse{ID: job.ID, Status: job.Status})
}

func (h *Handler) getWebhook(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "id must be a valid UUID")
		return
	}

	job, err := h.service.Get(r.Context(), id)
	if errors.Is(err, service.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "webhook job not found")
		return
	}
	if err != nil {
		h.handleInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (h *Handler) listWebhooks(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
		parsedLimit, err := strconv.Atoi(rawLimit)
		if err != nil || parsedLimit < 1 {
			writeError(w, http.StatusBadRequest, "validation_error", "limit must be a positive integer")
			return
		}
		if parsedLimit > 100 {
			writeError(w, http.StatusBadRequest, "validation_error", "limit must not exceed 100")
			return
		}
		limit = parsedLimit
	}

	var status *model.Status
	if rawStatus := r.URL.Query().Get("status"); rawStatus != "" {
		parsedStatus := model.Status(rawStatus)
		if !parsedStatus.Valid() {
			writeError(w, http.StatusBadRequest, "validation_error", "status is invalid")
			return
		}
		status = &parsedStatus
	}

	jobs, err := h.service.List(r.Context(), status, limit)
	if err != nil {
		h.handleInternalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Health(r.Context()); err != nil {
		h.logger.Error("database health check failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "internal_error", "database is unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleInternalError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, service.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "request conflicts with the current state")
		return
	}
	h.logger.Error("unexpected request error", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
}

func validateCreateRequest(request createWebhookRequest) error {
	if len(request.TargetURL) > 2048 {
		return errors.New("target_url must not exceed 2048 characters")
	}
	if err := targetpolicy.ValidateURL(request.TargetURL); err != nil {
		return errors.New("target_url is invalid")
	}
	if request.EventType == "" {
		return errors.New("event_type must not be empty")
	}
	if strings.ContainsAny(request.EventType, "\r\n") {
		return errors.New("event_type must not contain line breaks")
	}
	if len(request.EventType) > 255 {
		return errors.New("event_type must not exceed 255 characters")
	}
	if len(request.Payload) == 0 || !json.Valid(request.Payload) {
		return errors.New("payload must be valid JSON")
	}
	return nil
}

func ensureEndOfJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected second JSON value")
		}
		return err
	}
	return nil
}

type errorEnvelope struct {
	Error errorResponse `json:"error"`
}

type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorResponse{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("failed to encode HTTP response", "error", fmt.Errorf("encode response: %w", err))
	}
}
