package jobs

import (
	"net/http"
	"time"
)

type RetryPolicy struct {
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
}

func (p RetryPolicy) ShouldRetry(attempt int, idempotent bool, status int, err error) bool {
	if !idempotent || attempt >= p.maxAttempts() {
		return false
	}
	return err != nil || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func (p RetryPolicy) Delay(attempt int) time.Duration {
	delay := p.baseDelay()
	for i := 1; i < attempt; i++ {
		delay *= 2
		if max := p.maxDelay(); delay >= max {
			return max
		}
	}
	return delay
}

func (p RetryPolicy) baseDelay() time.Duration {
	if p.BaseDelay <= 0 {
		return time.Second
	}
	return p.BaseDelay
}

func (p RetryPolicy) maxDelay() time.Duration {
	if p.MaxDelay <= 0 {
		return 30 * time.Second
	}
	return p.MaxDelay
}

func (p RetryPolicy) maxAttempts() int {
	if p.MaxAttempts <= 0 {
		return 4
	}
	return p.MaxAttempts
}
