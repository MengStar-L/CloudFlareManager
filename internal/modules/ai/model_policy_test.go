package ai

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
)

func TestModelPolicyFiltersBuiltinAndLearnedPaidModels(t *testing.T) {
	t.Parallel()
	policy := newTestModelPolicy(t)
	if err := policy.LearnPaid(context.Background(), "@cf/vendor/learned-paid", "requires a Workers Paid plan"); err != nil {
		t.Fatal(err)
	}
	models := []map[string]any{
		{"name": "@cf/zai-org/glm-5.2"},
		{"name": "@cf/vendor/free"},
		{"id": "@cf/vendor/learned-paid"},
		{"name": "@cf/vendor/free"},
		{"name": ""},
	}
	filtered, err := policy.Filter(context.Background(), models)
	if err != nil {
		t.Fatal(err)
	}
	if got := modelIDs(filtered); !reflect.DeepEqual(got, []string{"@cf/vendor/free"}) {
		t.Fatalf("models = %#v", got)
	}
}

func TestModelPolicyPersistsAndLearnsConcurrently(t *testing.T) {
	t.Parallel()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	policy := NewModelPolicy(db)
	var group sync.WaitGroup
	for index := 0; index < 20; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := policy.LearnPaid(context.Background(), "@cf/vendor/concurrent", "requires a Workers Paid plan"); err != nil {
				t.Errorf("LearnPaid: %v", err)
			}
		}()
	}
	group.Wait()
	blocked, err := NewModelPolicy(db).IsBlocked(context.Background(), "@cf/vendor/concurrent")
	if err != nil || !blocked {
		t.Fatalf("blocked = %v, err = %v", blocked, err)
	}
	var rows int
	if err := db.QueryRow("SELECT COUNT(*) FROM ai_paid_models WHERE model_id = ?", "@cf/vendor/concurrent").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows = %d", rows)
	}
}

func TestPaidPlanReason(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		status     int
		body       string
		wantReason string
		want       bool
	}{
		{name: "errors envelope", status: 403, body: `{"errors":[{"message":"Model x requires a Workers Paid plan"}]}`, wantReason: "Model x requires a Workers Paid plan", want: true},
		{name: "error envelope", status: 403, body: `{"error":{"message":"Workers Paid plan is required for this model"}}`, wantReason: "Workers Paid plan is required for this model", want: true},
		{name: "invalid token", status: 403, body: `{"error":{"message":"invalid API token"}}`},
		{name: "content blocked", status: 403, body: `{"error":{"message":"content blocked"}}`},
		{name: "ignore non-message JSON field", status: 403, body: `{"detail":"requires a Workers Paid plan"}`},
		{name: "plain text response", status: 403, body: `Model x requires a Workers Paid plan`, wantReason: "Model x requires a Workers Paid plan", want: true},
		{name: "wrong status", status: 400, body: `{"error":{"message":"requires a Workers Paid plan"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			reason, matched := PaidPlanReason(test.status, []byte(test.body))
			if matched != test.want || reason != test.wantReason {
				t.Fatalf("reason = %q, matched = %v", reason, matched)
			}
		})
	}
}

func newTestModelPolicy(t *testing.T) *ModelPolicy {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewModelPolicy(db)
}

func modelIDs(models []map[string]any) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, catalogModelID(model))
	}
	return ids
}
