package delivery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

type blockingDeliverer struct {
	mu      sync.Mutex
	active  int
	maximum int
	started chan struct{}
	release <-chan struct{}
}

func (d *blockingDeliverer) Deliver(ctx context.Context, _ model.WebhookJob) Result {
	d.mu.Lock()
	d.active++
	if d.active > d.maximum {
		d.maximum = d.active
	}
	d.mu.Unlock()
	d.started <- struct{}{}
	select {
	case <-ctx.Done():
	case <-d.release:
	}
	d.mu.Lock()
	d.active--
	d.mu.Unlock()
	return Result{Outcome: OutcomeDelivered}
}

func TestPoolLimitsConcurrencyAndStops(t *testing.T) {
	jobs := []model.WebhookJob{deliveryTestJob(), deliveryTestJob(), deliveryTestJob()}
	dispatchStore := &fakeDispatchStore{jobs: jobs}
	resultStore := &fakeResultStore{updated: make(chan struct{}, len(jobs))}
	release := make(chan struct{})
	deliverer := &blockingDeliverer{
		started: make(chan struct{}, len(jobs)),
		release: release,
	}
	dispatcher := NewDispatcher(dispatchStore, time.Hour, len(jobs), time.Minute, testLogger())
	pool := NewPool(dispatcher, resultStore, deliverer, 2, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()

	for range 2 {
		select {
		case <-deliverer.started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start jobs")
		}
	}
	select {
	case <-deliverer.started:
		t.Fatal("third job started while both workers were blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-deliverer.started:
	case <-time.After(time.Second):
		t.Fatal("third job did not start after a worker became available")
	}
	for range jobs {
		select {
		case <-resultStore.updated:
		case <-time.After(time.Second):
			t.Fatal("job result was not persisted")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pool did not stop after cancellation")
	}

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if deliverer.maximum != 2 {
		t.Fatalf("maximum delivery concurrency = %d, want 2", deliverer.maximum)
	}
}
