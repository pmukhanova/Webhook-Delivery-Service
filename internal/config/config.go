package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr          string
	DatabaseURL       string
	WorkerCount       int
	JobBatchSize      int
	DispatchInterval  time.Duration
	DeliveryTimeout   time.Duration
	ProcessingTimeout time.Duration
}

func Load() (Config, error) {
	workerCount, err := positiveInt("WORKER_COUNT", 4)
	if err != nil {
		return Config{}, err
	}
	batchSize, err := positiveInt("JOB_BATCH_SIZE", workerCount)
	if err != nil {
		return Config{}, err
	}
	dispatchInterval, err := positiveDuration("DISPATCH_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	deliveryTimeout, err := positiveDuration("DELIVERY_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	processingTimeout, err := positiveDuration("PROCESSING_TIMEOUT", 60*time.Second)
	if err != nil {
		return Config{}, err
	}
	if processingTimeout <= deliveryTimeout {
		return Config{}, errors.New("PROCESSING_TIMEOUT must be greater than DELIVERY_TIMEOUT")
	}
	processingWaves := (batchSize + workerCount - 1) / workerCount
	minimumProcessingTimeout := time.Duration(processingWaves+1) * deliveryTimeout
	if processingTimeout <= minimumProcessingTimeout {
		return Config{}, fmt.Errorf(
			"PROCESSING_TIMEOUT must exceed %s for the configured batch size, worker count, and delivery timeout",
			minimumProcessingTimeout,
		)
	}

	cfg := Config{
		HTTPAddr:          envOrDefault("HTTP_ADDR", ":8080"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		WorkerCount:       workerCount,
		JobBatchSize:      batchSize,
		DispatchInterval:  dispatchInterval,
		DeliveryTimeout:   deliveryTimeout,
		ProcessingTimeout: processingTimeout,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	return cfg, nil
}

func positiveInt(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return value, nil
}

func positiveDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
