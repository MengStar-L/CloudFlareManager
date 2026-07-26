package r2

import (
	"errors"
	"testing"
)

func TestPlacementPrefersHealthyHeadroomAndRules(t *testing.T) {
	t.Parallel()

	p := PlacementPolicy{}
	candidates := []Candidate{
		{ID: "a", Healthy: true, Writable: true, StorageRatio: .8, ClassARatio: .1, ClassBRatio: .1, LatencyRatio: .2},
		{ID: "b", Healthy: true, Writable: true, StorageRatio: .2, ClassARatio: .2, ClassBRatio: .2, LatencyRatio: .3},
	}

	got, err := p.Select(ObjectInput{Key: "docs/report.pdf", Size: 1024}, candidates, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" {
		t.Fatalf("selected %q", got.ID)
	}

	rules := []PlacementRule{{Prefix: "docs/", TargetID: "a"}}
	got, err = p.Select(ObjectInput{Key: "docs/report.pdf", Size: 1024}, candidates, rules)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a" {
		t.Fatalf("rule selected %q", got.ID)
	}
}

func TestPlacementRejectsExhaustedPool(t *testing.T) {
	t.Parallel()

	p := PlacementPolicy{SoftLimit: .9}
	_, err := p.Select(ObjectInput{Key: "file.bin"}, []Candidate{{ID: "a", Healthy: true, Writable: true, StorageRatio: .95}}, nil)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("error = %v", err)
	}
}
