package delivery

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: 2 * time.Second},
		{attempt: 2, want: 4 * time.Second},
		{attempt: 3, want: 8 * time.Second},
		{attempt: 4, want: 16 * time.Second},
		{attempt: 6, want: time.Minute},
		{attempt: 20, want: time.Minute},
	}
	for _, test := range tests {
		if got := Backoff(test.attempt); got != test.want {
			t.Errorf("Backoff(%d) = %v, want %v", test.attempt, got, test.want)
		}
	}
}
