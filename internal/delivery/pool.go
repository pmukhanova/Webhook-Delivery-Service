package delivery

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pmukhanova/webhook-delivery-service/internal/model"
)

type Pool struct {
	dispatcher *Dispatcher
	workers    []*Worker
	jobs       chan model.WebhookJob
}

func NewPool(dispatcher *Dispatcher, store resultStore, deliverer jobDeliverer, workerCount int, logger *slog.Logger) *Pool {
	workers := make([]*Worker, 0, workerCount)
	for id := 1; id <= workerCount; id++ {
		workers = append(workers, NewWorker(id, store, deliverer, logger))
	}
	return &Pool{
		dispatcher: dispatcher,
		workers:    workers,
		jobs:       make(chan model.WebhookJob),
	}
}

func (p *Pool) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(len(p.workers))
	for _, worker := range p.workers {
		go func() {
			defer workers.Done()
			worker.Run(ctx, p.jobs)
		}()
	}

	p.dispatcher.Run(ctx, p.jobs)
	close(p.jobs)
	workers.Wait()
}
