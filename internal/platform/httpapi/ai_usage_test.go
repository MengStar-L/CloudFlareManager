package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/ai"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/database"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/secret"
)

func TestAIUsageReturnsAccountQuotaReport(t *testing.T) {
	t.Parallel()
	service := newAIUsageFixture(t)
	api := &API{deps: Dependencies{AIUsage: service}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ai/usage?date=2026-07-29", nil)
	response := httptest.NewRecorder()

	api.aiUsage(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var report ai.DailyUsageReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Date != "2026-07-29" || report.Timezone != "UTC" || report.DailyLimit != 10_000 || !report.Estimated {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Accounts) != 1 || report.Accounts[0].EstimatedRemaining != 10_000 {
		t.Fatalf("accounts = %#v", report.Accounts)
	}
}

func TestAIUsageRejectsInvalidDate(t *testing.T) {
	t.Parallel()
	api := &API{deps: Dependencies{AIUsage: newAIUsageFixture(t)}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/ai/usage?date=2026-02-29", nil)
	response := httptest.NewRecorder()

	api.aiUsage(response, request)

	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"invalid_date"`)) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func newAIUsageFixture(t *testing.T) *ai.UsageService {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cipher, err := secret.NewCipher(bytes.Repeat([]byte{20}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	accountStore := accounts.NewStore(db, secret.NewRepository(db, cipher))
	account, err := accountStore.Create(context.Background(), accounts.CreateInput{
		Name: "primary", CloudflareAccountID: "cloudflare", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := accountStore.SetCapabilities(context.Background(), account.ID, []accounts.Capability{{Name: "ai", Available: true}}); err != nil {
		t.Fatal(err)
	}
	return &ai.UsageService{DB: db, Accounts: accountStore}
}
