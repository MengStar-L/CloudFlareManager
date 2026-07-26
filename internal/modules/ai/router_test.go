package ai

import (
	"errors"
	"testing"
)

func TestRouterChoosesHealthyAccountBelowLimit(t *testing.T) {
	t.Parallel()

	r := Router{NeuronSoftLimit: 9000}
	got, err := r.Select([]AccountState{
		{ID: "unhealthy", Healthy: false, EstimatedNeurons: 100},
		{ID: "near-limit", Healthy: true, EstimatedNeurons: 8800, RecentErrorRatio: .2},
		{ID: "available", Healthy: true, EstimatedNeurons: 2000, RecentErrorRatio: .1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "available" {
		t.Fatalf("selected %q", got.ID)
	}
}

func TestRouterRejectsQuotaExhaustion(t *testing.T) {
	t.Parallel()

	r := Router{NeuronSoftLimit: 9000}
	_, err := r.Select([]AccountState{{ID: "a", Healthy: true, EstimatedNeurons: 9000}})
	if !errors.Is(err, ErrAIQuotaExceeded) {
		t.Fatalf("error = %v", err)
	}
}
