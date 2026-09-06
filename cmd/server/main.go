package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pmukhanova/webhook-delivery-service/internal/config"
	"github.com/pmukhanova/webhook-delivery-service/internal/delivery"
	"github.com/pmukhanova/webhook-delivery-service/internal/handler"
	"github.com/pmukhanova/webhook-delivery-service/internal/repository"
	"github.com/pmukhanova/webhook-delivery-service/internal/service"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load configuration", "error", err)
		os.Exit(1)
	}

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStartup()
	pool, err := pgxpool.New(startupCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(startupCtx); err != nil {
		logger.Error("connect to database", "error", err)
		os.Exit(1)
	}

	webhookRepository := repository.NewWebhookRepository(pool)
	webhookService := service.NewWebhookService(webhookRepository)
	httpHandler := handler.New(webhookService, logger)
	deliverer := delivery.NewDeliverer(delivery.NewHTTPClient(cfg.DeliveryTimeout))
	dispatcher := delivery.NewDispatcher(
		webhookRepository,
		cfg.DispatchInterval,
		cfg.JobBatchSize,
		cfg.ProcessingTimeout,
		logger,
	)
	workerPool := delivery.NewPool(dispatcher, webhookRepository, deliverer, cfg.WorkerCount, logger)

	appCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpHandler.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return appCtx
		},
	}

	serverErrors := make(chan error, 1)
	backgroundDone := make(chan struct{})
	go func() {
		defer close(backgroundDone)
		workerPool.Run(appCtx)
	}()
	go func() {
		logger.Info("server started",
			"address", cfg.HTTPAddr,
			"worker_count", cfg.WorkerCount,
			"dispatch_interval", cfg.DispatchInterval,
			"delivery_timeout", cfg.DeliveryTimeout,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	select {
	case <-appCtx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErrors:
		logger.Error("HTTP server failed", "error", err)
		stop()
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	} else {
		logger.Info("server stopped")
	}

	select {
	case <-backgroundDone:
		logger.Info("background delivery stopped")
	case <-time.After(10 * time.Second):
		logger.Error("background delivery shutdown timed out")
	}
}
