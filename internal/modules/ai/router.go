package ai

import (
	"errors"
	"sort"
)

var ErrAIQuotaExceeded = errors.New("workers ai neuron soft limit exceeded")

type AccountState struct {
	ID               string
	Healthy          bool
	Disabled         bool
	AllowOverflow    bool
	EstimatedNeurons float64
	RecentErrorRatio float64
	LatencyRatio     float64
}

type Router struct {
	NeuronSoftLimit float64
}

func (r Router) Select(accounts []AccountState) (AccountState, error) {
	limit := r.NeuronSoftLimit
	if limit <= 0 {
		limit = 9_000
	}
	type scored struct {
		account AccountState
		score   float64
	}
	var available []scored
	for _, account := range accounts {
		if account.Disabled || !account.Healthy {
			continue
		}
		if !account.AllowOverflow && account.EstimatedNeurons >= limit {
			continue
		}
		usageRatio := account.EstimatedNeurons / limit
		score := usageRatio*.75 + account.RecentErrorRatio*.20 + account.LatencyRatio*.05
		available = append(available, scored{account: account, score: score})
	}
	if len(available) == 0 {
		return AccountState{}, ErrAIQuotaExceeded
	}
	sort.SliceStable(available, func(i, j int) bool {
		if available[i].score == available[j].score {
			return available[i].account.ID < available[j].account.ID
		}
		return available[i].score < available[j].score
	})
	return available[0].account, nil
}
