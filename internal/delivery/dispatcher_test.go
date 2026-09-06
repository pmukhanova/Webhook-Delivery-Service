package delivery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

type fakeDispatchStore struct {
	mu         sync.Mutex
	claims     int
	recoveries int
	jobs       []model.WebhookJob
}

func (s *fakeDispatchStore) ClaimReadyJobs(context.Context, int) ([]model.WebhookJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	jobs := s.jobs
	s.jobs = nil
	return jobs, nil
}

func (s *fakeDispatchStore) RecoverStaleProcessing(context.Context, time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recoveries++
	return 0, nil
}

func TestDispatcherSendsClaimedJobsAndStops(t *testing.T) {
	job := deliveryTestJob()
	store := &fakeDispatchStore{jobs: []model.WebhookJob{job}}
	dispatcher := NewDispatcher(store, time.Hour, 4, time.Minute, testLogger())
	jobs := make(chan model.WebhookJob, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatcher.Run(ctx, jobs)
	}()

	select {
	case got := <-jobs:
		if got.ID != job.ID {
			t.Errorf("job ID = %s, want %s", got.ID, job.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not send claimed job")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not stop after cancellation")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.claims != 1 || store.recoveries != 1 {
		t.Fatalf("calls claim/recovery = %d/%d, want 1/1; dispatcher may be busy-looping", store.claims, store.recoveries)
	}
}
