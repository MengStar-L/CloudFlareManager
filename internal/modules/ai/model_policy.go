package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const paidPlanSource = "cloudflare_paid_plan_403"

var builtinPaidModels = map[string]string{
	"@cf/zai-org/glm-5.2": "requires a Workers Paid plan",
}

type ModelPolicy struct {
	DB *sql.DB
}

type ModelBlockedError struct {
	Model string
}

func (e *ModelBlockedError) Error() string {
	return "model " + e.Model + " requires a Workers Paid plan and is disabled"
}

func NewModelPolicy(db *sql.DB) *ModelPolicy {
	return &ModelPolicy{DB: db}
}

func (p *ModelPolicy) IsBlocked(ctx context.Context, model string) (bool, error) {
	model = strings.TrimSpace(model)
	if _, ok := builtinPaidModels[model]; ok {
		return true, nil
	}
	if p == nil || p.DB == nil || model == "" {
		return false, nil
	}
	var exists int
	if err := p.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM ai_paid_models WHERE model_id = ?", model).Scan(&exists); err != nil {
		return false, err
	}
	return exists != 0, nil
}

func (p *ModelPolicy) Filter(ctx context.Context, models []map[string]any) ([]map[string]any, error) {
	blocked := make(map[string]struct{}, len(builtinPaidModels))
	for model := range builtinPaidModels {
		blocked[model] = struct{}{}
	}
	if p != nil && p.DB != nil {
		rows, err := p.DB.QueryContext(ctx, "SELECT model_id FROM ai_paid_models")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return nil, err
			}
			blocked[model] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	byID := make(map[string]map[string]any, len(models))
	for _, model := range models {
		id := catalogModelID(model)
		if id == "" {
			continue
		}
		if _, denied := blocked[id]; denied {
			continue
		}
		if _, exists := byID[id]; !exists {
			byID[id] = model
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	filtered := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		filtered = append(filtered, byID[id])
	}
	return filtered, nil
}

func (p *ModelPolicy) LearnPaid(ctx context.Context, model, reason string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model ID is required")
	}
	if p == nil || p.DB == nil {
		return errors.New("AI model policy is not configured")
	}
	now := time.Now().Unix()
	_, err := p.DB.ExecContext(ctx, `INSERT INTO ai_paid_models(model_id, source, reason, detected_at, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(model_id) DO UPDATE SET
		source = excluded.source, reason = excluded.reason, updated_at = excluded.updated_at`,
		model, paidPlanSource, strings.TrimSpace(reason), now, now)
	return err
}

func PaidPlanReason(status int, body []byte) (string, bool) {
	if status != 403 {
		return "", false
	}
	var payload any
	if json.Unmarshal(body, &payload) == nil {
		for _, message := range errorMessages(payload) {
			if isPaidPlanMessage(message) {
				return message, true
			}
		}
		return "", false
	}
	message := strings.TrimSpace(string(body))
	if isPaidPlanMessage(message) {
		return message, true
	}
	return "", false
}

func catalogModelID(model map[string]any) string {
	if value, ok := model["name"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if value, ok := model["id"].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func errorMessages(value any) []string {
	var messages []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, item := range typed {
				if key == "message" {
					if message, ok := item.(string); ok {
						messages = append(messages, strings.TrimSpace(message))
					}
					continue
				}
				walk(item)
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	return messages
}

func isPaidPlanMessage(message string) bool {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "workers paid plan") {
		return false
	}
	return strings.Contains(lower, "require") || strings.Contains(lower, "need") || strings.Contains(lower, "upgrade")
}
