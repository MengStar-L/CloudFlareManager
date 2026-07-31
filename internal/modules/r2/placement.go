package r2

import (
	"errors"
	"mime"
	"path/filepath"
	"sort"
	"strings"
)

var ErrQuotaExceeded = errors.New("r2 pool soft quota exceeded")

type ObjectInput struct {
	Key         string
	Size        int64
	ContentType string
	Metadata    map[string]string
}

type Candidate struct {
	ID                  string
	AccountID           string
	Healthy             bool
	Writable            bool
	StorageRatio        float64
	AccountStorageRatio float64
	ClassARatio         float64
	ClassBRatio         float64
	LatencyRatio        float64
	AvailableBytes      int64
	AllowOverflow       bool
}

type PlacementRule struct {
	Prefix      string
	Extension   string
	ContentType string
	MinSize     int64
	MaxSize     int64
	TargetID    string
}

type PlacementPolicy struct {
	SoftLimit float64
}

func (p PlacementPolicy) Select(input ObjectInput, candidates []Candidate, rules []PlacementRule) (Candidate, error) {
	byID := make(map[string]Candidate, len(candidates))
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	for _, rule := range rules {
		if rule.matches(input) {
			if candidate, ok := byID[rule.TargetID]; ok && p.eligible(candidate) {
				return candidate, nil
			}
		}
	}

	type scored struct {
		candidate Candidate
		score     float64
	}
	var eligible []scored
	for _, candidate := range candidates {
		if !p.eligible(candidate) {
			continue
		}
		score := (1-candidate.StorageRatio)*.50 + (1-candidate.AccountStorageRatio)*.25 +
			(1-candidate.ClassARatio)*.10 + (1-candidate.ClassBRatio)*.05 +
			(1-candidate.LatencyRatio)*.10
		eligible = append(eligible, scored{candidate: candidate, score: score})
	}
	if len(eligible) == 0 {
		return Candidate{}, ErrQuotaExceeded
	}
	if input.Size < 0 {
		sort.SliceStable(eligible, func(i, j int) bool {
			if eligible[i].candidate.AvailableBytes == eligible[j].candidate.AvailableBytes {
				return eligible[i].score > eligible[j].score
			}
			return eligible[i].candidate.AvailableBytes > eligible[j].candidate.AvailableBytes
		})
		return eligible[0].candidate, nil
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].score == eligible[j].score {
			return eligible[i].candidate.ID < eligible[j].candidate.ID
		}
		return eligible[i].score > eligible[j].score
	})
	return eligible[0].candidate, nil
}

func (p PlacementPolicy) eligible(candidate Candidate) bool {
	if !candidate.Healthy || !candidate.Writable {
		return false
	}
	limit := p.SoftLimit
	if limit <= 0 {
		limit = 1
	}
	storageEligible := candidate.AllowOverflow ||
		(candidate.StorageRatio <= limit && candidate.AccountStorageRatio <= limit)
	return storageEligible && candidate.ClassARatio <= limit && candidate.ClassBRatio <= limit
}

func (r PlacementRule) matches(input ObjectInput) bool {
	if r.Prefix != "" && !strings.HasPrefix(input.Key, r.Prefix) {
		return false
	}
	if r.Extension != "" && !strings.EqualFold(filepath.Ext(input.Key), r.Extension) {
		return false
	}
	contentType := input.ContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(input.Key))
	}
	if r.ContentType != "" && !strings.HasPrefix(contentType, r.ContentType) {
		return false
	}
	if input.Size < 0 && (r.MinSize > 0 || r.MaxSize > 0) {
		return false
	}
	if r.MinSize > 0 && input.Size < r.MinSize {
		return false
	}
	if r.MaxSize > 0 && input.Size > r.MaxSize {
		return false
	}
	return true
}
