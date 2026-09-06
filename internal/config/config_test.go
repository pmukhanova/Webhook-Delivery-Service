package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadBackgroundDefaults(t *testing.T) {
	setConfigEnvironment(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.WorkerCount != 4 || cfg.JobBatchSize != 4 {
		t.Fatalf("worker defaults = %d/%d, want 4/4", cfg.WorkerCount, cfg.JobBatchSize)
	}
	if cfg.DispatchInterval != time.Second || cfg.DeliveryTimeout != 5*time.Second || cfg.ProcessingTimeout != time.Minute {
		t.Fatalf("duration defaults = %v/%v/%v", cfg.DispatchInterval, cfg.DeliveryTimeout, cfg.ProcessingTimeout)
	}
}

func TestLoadRejectsInvalidBackgroundConfig(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		wantErr string
	}{
		{name: "worker count", key: "WORKER_COUNT", value: "0", wantErr: "WORKER_COUNT"},
		{name: "batch size", key: "JOB_BATCH_SIZE", value: "many", wantErr: "JOB_BATCH_SIZE"},
		{name: "dispatch interval", key: "DISPATCH_INTERVAL", value: "soon", wantErr: "DISPATCH_INTERVAL"},
		{name: "delivery timeout", key: "DELIVERY_TIMEOUT", value: "-1s", wantErr: "DELIVERY_TIMEOUT"},
		{name: "processing timeout", key: "PROCESSING_TIMEOUT", value: "5s", wantErr: "PROCESSING_TIMEOUT"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setConfigEnvironment(t)
			t.Setenv(test.key, test.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load() error = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadRejectsProcessingTimeoutShorterThanBatchWorstCase(t *testing.T) {
	setConfigEnvironment(t)
	t.Setenv("WORKER_COUNT", "1")
	t.Setenv("JOB_BATCH_SIZE", "8")
	t.Setenv("DELIVERY_TIMEOUT", "5s")
	t.Setenv("PROCESSING_TIMEOUT", "30s")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PROCESSING_TIMEOUT") {
		t.Fatalf("Load() error = %v, want unsafe processing timeout error", err)
	}
}

func setConfigEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test")
	for _, key := range []string{"WORKER_COUNT", "JOB_BATCH_SIZE", "DISPATCH_INTERVAL", "DELIVERY_TIMEOUT", "PROCESSING_TIMEOUT"} {
		t.Setenv(key, "")
	}
}
