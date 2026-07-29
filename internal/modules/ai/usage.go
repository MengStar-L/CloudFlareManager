package ai

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
)

const DailyFreeNeurons = 10_000.0

var ErrInvalidUsageDate = errors.New("usage date must be a valid YYYY-MM-DD date")

type CredentialUsage struct {
	CredentialID  string  `json:"credential_id"`
	Name          string  `json:"name"`
	Status        string  `json:"status"`
	EstimatedUsed float64 `json:"estimated_used_neurons"`
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
}

type AccountUsage struct {
	AccountID          string            `json:"account_id"`
	AccountName        string            `json:"account_name"`
	EstimatedUsed      float64           `json:"estimated_used_neurons"`
	EstimatedRemaining float64           `json:"estimated_remaining_neurons"`
	Requests           int64             `json:"requests"`
	Errors             int64             `json:"errors"`
	Credentials        []CredentialUsage `json:"credentials"`
}

type DailyUsageReport struct {
	Date       string         `json:"date"`
	Timezone   string         `json:"timezone"`
	DailyLimit float64        `json:"daily_limit_neurons"`
	Estimated  bool           `json:"estimated"`
	Accounts   []AccountUsage `json:"accounts"`
}

type UsageService struct {
	DB       *sql.DB
	Accounts *accounts.Store
}

func ParseUsageDate(raw string, now time.Time) (time.Time, error) {
	if raw == "" {
		now = now.UTC()
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	}
	if raw != strings.TrimSpace(raw) || len(raw) != len("2006-01-02") {
		return time.Time{}, ErrInvalidUsageDate
	}
	day, err := time.Parse("2006-01-02", raw)
	if err != nil || day.Format("2006-01-02") != raw {
		return time.Time{}, ErrInvalidUsageDate
	}
	return day.UTC(), nil
}

func (s UsageService) Daily(ctx context.Context, day time.Time) (DailyUsageReport, error) {
	if s.DB == nil || s.Accounts == nil {
		return DailyUsageReport{}, errors.New("AI usage service is not configured")
	}
	day = day.UTC()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	report := DailyUsageReport{
		Date: day.Format("2006-01-02"), Timezone: "UTC", DailyLimit: DailyFreeNeurons,
		Estimated: true, Accounts: []AccountUsage{},
	}
	items, err := s.Accounts.List(ctx)
	if err != nil {
		return DailyUsageReport{}, err
	}
	eligible := make([]accounts.Account, 0, len(items))
	for _, account := range items {
		if account.Enabled && hasAICapability(account) {
			eligible = append(eligible, account)
		}
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Name == eligible[j].Name {
			return eligible[i].ID < eligible[j].ID
		}
		return eligible[i].Name < eligible[j].Name
	})
	for _, account := range eligible {
		usage, err := s.accountUsage(ctx, account, day)
		if err != nil {
			return DailyUsageReport{}, fmt.Errorf("load AI usage for account %s: %w", account.ID, err)
		}
		report.Accounts = append(report.Accounts, usage)
	}
	return report, nil
}

func (s UsageService) accountUsage(ctx context.Context, account accounts.Account, day time.Time) (AccountUsage, error) {
	result := AccountUsage{
		AccountID: account.ID, AccountName: account.Name, EstimatedRemaining: DailyFreeNeurons,
		Credentials: []CredentialUsage{},
	}
	err := s.DB.QueryRowContext(ctx, `SELECT estimated_neurons, requests, errors FROM ai_usage_daily
		WHERE account_id = ? AND usage_date = ?`, account.ID, day.Format("2006-01-02")).Scan(
		&result.EstimatedUsed, &result.Requests, &result.Errors)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AccountUsage{}, err
	}
	result.EstimatedRemaining = maxFloat(DailyFreeNeurons-result.EstimatedUsed, 0)

	start := day.UnixNano()
	end := day.Add(24 * time.Hour).UnixNano()
	rows, err := s.DB.QueryContext(ctx, `SELECT COALESCE(log.protocol_credential_id, ''), credential.id,
		credential.name, credential.disabled, SUM(log.estimated_neurons), COUNT(*),
		SUM(CASE WHEN log.status_code >= 400 OR log.error_class <> '' THEN 1 ELSE 0 END)
		FROM ai_request_logs AS log
		LEFT JOIN protocol_credentials AS credential ON credential.id = log.protocol_credential_id
		WHERE log.account_id = ? AND log.created_at >= ? AND log.created_at < ?
		GROUP BY log.protocol_credential_id, credential.id, credential.name, credential.disabled`,
		account.ID, start, end)
	if err != nil {
		return AccountUsage{}, err
	}
	defer rows.Close()
	byID := make(map[string]CredentialUsage)
	for rows.Next() {
		var loggedID string
		var currentID, name sql.NullString
		var disabled sql.NullBool
		var used float64
		var requests, errorsCount int64
		if err := rows.Scan(&loggedID, &currentID, &name, &disabled, &used, &requests, &errorsCount); err != nil {
			return AccountUsage{}, err
		}
		item := credentialUsage(loggedID, currentID, name, disabled)
		if existing, ok := byID[item.CredentialID]; ok {
			item.EstimatedUsed += existing.EstimatedUsed
			item.Requests += existing.Requests
			item.Errors += existing.Errors
		}
		item.EstimatedUsed += used
		item.Requests += requests
		item.Errors += errorsCount
		byID[item.CredentialID] = item
	}
	if err := rows.Err(); err != nil {
		return AccountUsage{}, err
	}
	for _, item := range byID {
		result.Credentials = append(result.Credentials, item)
	}
	sort.Slice(result.Credentials, func(i, j int) bool {
		if result.Credentials[i].EstimatedUsed == result.Credentials[j].EstimatedUsed {
			if result.Credentials[i].Name == result.Credentials[j].Name {
				return result.Credentials[i].CredentialID < result.Credentials[j].CredentialID
			}
			return result.Credentials[i].Name < result.Credentials[j].Name
		}
		return result.Credentials[i].EstimatedUsed > result.Credentials[j].EstimatedUsed
	})
	return result, nil
}

func credentialUsage(loggedID string, currentID, name sql.NullString, disabled sql.NullBool) CredentialUsage {
	if loggedID == "" || loggedID == "admin" {
		return CredentialUsage{CredentialID: "unattributed", Name: "面板及未归属调用", Status: "unattributed"}
	}
	if !currentID.Valid {
		shortID := loggedID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		return CredentialUsage{CredentialID: loggedID, Name: "已删除密钥 " + shortID, Status: "deleted"}
	}
	status := "active"
	if disabled.Valid && disabled.Bool {
		status = "revoked"
	}
	return CredentialUsage{CredentialID: loggedID, Name: name.String, Status: status}
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
