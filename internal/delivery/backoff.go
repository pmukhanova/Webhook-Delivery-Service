package delivery

import "time"

const (
	baseBackoff = 2 * time.Second
	maxBackoff  = time.Minute
)

func Backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := baseBackoff
	for current := 1; current < attempt; current++ {
		if delay >= maxBackoff/2 {
			return maxBackoff
		}
		delay *= 2
	}
	if delay > maxBackoff {
		return maxBackoff
	}
	return delay
}
