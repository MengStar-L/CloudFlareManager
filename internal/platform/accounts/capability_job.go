package accounts

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/jobs"
)

const CapabilityJobType = "account.capabilities.detect"

type CapabilityJobHandler struct {
	Store    *Store
	Verifier Verifier
}

func (h CapabilityJobHandler) Handle(ctx context.Context, job jobs.Job) error {
	var payload struct {
		AccountID string `json:"account_id"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("decode capability job: %w", err)
	}
	account, err := h.Store.Get(ctx, payload.AccountID, true)
	if err != nil {
		return err
	}
	capabilities, err := h.Verifier.Detect(ctx, account.CloudflareAccountID, account.APIToken)
	if err != nil {
		_ = h.Store.SetHealth(ctx, account.ID, "error", err.Error())
		return err
	}
	if err := h.Store.SetCapabilities(ctx, account.ID, capabilities); err != nil {
		return err
	}
	health := "healthy"
	detail := ""
	for _, capability := range capabilities {
		if capability.Name == "api_token" && !capability.Available {
			health, detail = "error", capability.Detail
			break
		}
		if !capability.Available && health == "healthy" {
			health, detail = "degraded", "one or more Cloudflare capabilities are unavailable"
		}
	}
	return h.Store.SetHealth(ctx, account.ID, health, detail)
}
