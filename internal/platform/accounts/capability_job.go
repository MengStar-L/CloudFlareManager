package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	capabilities, err := h.Verifier.Detect(ctx, account.CloudflareAccountID, account.APIToken)
	if err != nil {
		current, updateErr := h.Store.setHealthIfAPITokenCurrent(ctx, account.ID, account.apiTokenSecretID, "error", err.Error())
		if errors.Is(updateErr, ErrNotFound) {
			return nil
		}
		if updateErr != nil {
			return updateErr
		}
		if !current {
			return nil
		}
		return err
	}
	health := "healthy"
	detail := ""
	var unavailable []string
	for _, capability := range capabilities {
		if capability.Name == "api_token" && !capability.Available {
			health, detail = "error", capability.Detail
			break
		}
		if !capability.Available {
			unavailable = append(unavailable, capabilityLabel(capability.Name))
		}
	}
	if health == "healthy" && len(unavailable) != 0 {
		health, detail = "degraded", "以下检测未通过："+strings.Join(unavailable, "、")+"。请查看各项失败详情。"
	}
	_, err = h.Store.setVerificationResultIfAPITokenCurrent(
		ctx, account.ID, account.apiTokenSecretID, capabilities, health, detail,
	)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
