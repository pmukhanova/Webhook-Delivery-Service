package delivery

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

type fakeResultStore struct {
	mu        sync.Mutex
	delivered int
	retried   int
	failed    int
	next      time.Time
	updated   chan struct{}
}

func (s *fakeResultStore) MarkDelivered(context.Context, uuid.UUID, uuid.UUID) error {
	s.mu.Lock()
	s.delivered++
	s.mu.Unlock()
	s.notify()
	return nil
}

func (s *fakeResultStore) ScheduleRetry(_ context.Context, _ uuid.UUID, _ uuid.UUID, next time.Time, _ string) error {
	s.mu.Lock()
	s.retried++
	s.next = next
	s.mu.Unlock()
	s.notify()
	return nil
}

func (s *fakeResultStore) MarkFailed(context.Context, uuid.UUID, uuid.UUID, string) error {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
	s.notify()
	return nil
}

func (s *fakeResultStore) notify() {
	if s.updated != nil {
		select {
		case s.updated <- struct{}{}:
		default:
		}
	}
}

type staticDeliverer struct{ result Result }

func (d staticDeliverer) Deliver(context.Context, model.WebhookJob) Result { return d.result }

func TestWorkerPersistsDeliveryResult(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		result        Result
		attempts      int
		maxAttempts   int
		wantDelivered int
		wantRetried   int
		wantFailed    int
		wantNext      time.Time
	}{
		{name: "delivered", result: Result{Outcome: OutcomeDelivered, HTTPStatus: 200}, maxAttempts: 5, wantDelivered: 1},
		{name: "retry", result: Result{Outcome: OutcomeRetry, Error: "network error"}, attempts: 1, maxAttempts: 5, wantRetried: 1, wantNext: now.Add(4 * time.Second)},
		{name: "permanent failure", result: Result{Outcome: OutcomeFailed, Error: "HTTP 400"}, maxAttempts: 5, wantFailed: 1},
		{name: "max attempts reached", result: Result{Outcome: OutcomeRetry, Error: "HTTP 500"}, attempts: 4, maxAttempts: 5, wantFailed: 1},
		{name: "zero value outcome", result: Result{}, maxAttempts: 5, wantRetried: 1, wantNext: now.Add(2 * time.Second)},
		{name: "unexpected outcome", result: Result{Outcome: Outcome(99)}, maxAttempts: 5, wantRetried: 1, wantNext: now.Add(2 * time.Second)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeResultStore{}
			worker := NewWorker(1, store, staticDeliverer{result: test.result}, testLogger())
			worker.now = func() time.Time { return now }
			job := deliveryTestJob()
			job.Attempts = test.attempts
			job.MaxAttempts = test.maxAttempts
			worker.process(context.Background(), job)

			if store.delivered != test.wantDelivered || store.retried != test.wantRetried || store.failed != test.wantFailed {
				t.Fatalf("updates delivered/retried/failed = %d/%d/%d", store.delivered, store.retried, store.failed)
			}
			if !test.wantNext.IsZero() && !store.next.Equal(test.wantNext) {
				t.Errorf("next attempt = %v, want %v", store.next, test.wantNext)
			}
		})
	}
}

func TestWorkerReceivesJobAndStopsWhenChannelCloses(t *testing.T) {
	store := &fakeResultStore{updated: make(chan struct{}, 1)}
	worker := NewWorker(1, store, staticDeliverer{result: Result{Outcome: OutcomeDelivered}}, testLogger())
	jobs := make(chan model.WebhookJob, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(context.Background(), jobs)
	}()
	jobs <- deliveryTestJob()
	close(jobs)

	select {
	case <-store.updated:
	case <-time.After(time.Second):
		t.Fatal("worker did not process job")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after channel close")
	}
}

func TestWorkerStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	worker := NewWorker(1, &fakeResultStore{}, staticDeliverer{}, testLogger())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Run(ctx, make(chan model.WebhookJob))
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
