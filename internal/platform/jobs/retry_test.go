package jobs

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryPolicyOnlyRetriesSafeFailures(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{BaseDelay: time.Second, MaxDelay: 10 * time.Second, MaxAttempts: 4}
	if !p.ShouldRetry(2, true, http.StatusTooManyRequests, nil) {
		t.Fatal("idempotent 429 should retry")
	}
	if p.ShouldRetry(2, false, http.StatusTooManyRequests, nil) {
		t.Fatal("non-idempotent request should not retry")
	}
	if p.ShouldRetry(4, true, http.StatusServiceUnavailable, nil) {
		t.Fatal("max attempts should stop retry")
	}
	if got := p.Delay(3); got < 3*time.Second || got > 10*time.Second {
		t.Fatalf("unexpected delay %v", got)
	}
}
