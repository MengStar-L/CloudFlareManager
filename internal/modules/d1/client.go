package d1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cf-r2-manager/cf-r2-manager/internal/modules/r2"
	"github.com/cf-r2-manager/cf-r2-manager/internal/platform/accounts"
	"github.com/google/uuid"
)

type Database struct {
	UUID      string    `json:"uuid"`
	Name      string    `json:"name"`
	Version   string    `json:"version,omitempty"`
	NumTables int       `json:"num_tables,omitempty"`
	FileSize  int64     `json:"file_size,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

type QueryInput struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

type QueryMeta struct {
	Duration    float64 `json:"duration"`
	RowsRead    int64   `json:"rows_read"`
	RowsWritten int64   `json:"rows_written"`
	Changes     int64   `json:"changes,omitempty"`
	LastRowID   int64   `json:"last_row_id,omitempty"`
}

type QueryResult struct {
	Success bool             `json:"success"`
	Results []map[string]any `json:"results"`
	Meta    QueryMeta        `json:"meta"`
}

type HistoryEntry struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	DatabaseID  string    `json:"database_id"`
	SQL         string    `json:"sql"`
	Class       SQLClass  `json:"class"`
	Success     bool      `json:"success"`
	RowsRead    int64     `json:"rows_read"`
	RowsWritten int64     `json:"rows_written"`
	DurationMS  float64   `json:"duration_ms"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
	Accounts   *accounts.Store
	DB         *sql.DB
	Backups    BackupWriter
}

type BackupWriter interface {
	Put(context.Context, r2.PutRequest) (r2.Object, error)
}

type Backup struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	DatabaseID  string    `json:"database_id"`
	R2ObjectKey string    `json:"r2_object_key"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (c Client) ListDatabases(ctx context.Context, accountID string) ([]Database, error) {
	account, err := c.account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return doJSON[[]Database](ctx, c, account.APIToken, http.MethodGet,
		"/accounts/"+url.PathEscape(account.CloudflareAccountID)+"/d1/database?per_page=100", nil)
}

func (c Client) CreateDatabase(ctx context.Context, accountID, name string) (Database, error) {
	if strings.TrimSpace(name) == "" {
		return Database{}, errors.New("database name is required")
	}
	account, err := c.account(ctx, accountID)
	if err != nil {
		return Database{}, err
	}
	return doJSON[Database](ctx, c, account.APIToken, http.MethodPost,
		"/accounts/"+url.PathEscape(account.CloudflareAccountID)+"/d1/database", map[string]string{"name": name})
}

func (c Client) DeleteDatabase(ctx context.Context, accountID, databaseID string) error {
	account, err := c.account(ctx, accountID)
	if err != nil {
		return err
	}
	_, err = doJSON[json.RawMessage](ctx, c, account.APIToken, http.MethodDelete,
		"/accounts/"+url.PathEscape(account.CloudflareAccountID)+"/d1/database/"+url.PathEscape(databaseID), nil)
	return err
}

func (c Client) Query(ctx context.Context, accountID, databaseID string, input QueryInput) ([]QueryResult, error) {
	if strings.TrimSpace(input.SQL) == "" {
		return nil, errors.New("SQL is required")
	}
	account, err := c.account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	results, queryErr := doJSON[[]QueryResult](ctx, c, account.APIToken, http.MethodPost,
		"/accounts/"+url.PathEscape(account.CloudflareAccountID)+"/d1/database/"+url.PathEscape(databaseID)+"/query", input)
	entry := HistoryEntry{
		ID: uuid.NewString(), AccountID: accountID, DatabaseID: databaseID, SQL: input.SQL,
		Class: ClassifySQL(input.SQL), Success: queryErr == nil, DurationMS: float64(time.Since(started).Microseconds()) / 1000,
		CreatedAt: time.Now(),
	}
	for _, result := range results {
		entry.RowsRead += result.Meta.RowsRead
		entry.RowsWritten += result.Meta.RowsWritten
		if result.Meta.Duration > 0 {
			entry.DurationMS += result.Meta.Duration
		}
	}
	if queryErr != nil {
		entry.Error = queryErr.Error()
	}
	if historyErr := c.recordHistory(ctx, entry); historyErr != nil && queryErr == nil {
		return nil, historyErr
	}
	return results, queryErr
}

func (c Client) TimeTravelRestore(ctx context.Context, accountID, databaseID, bookmark string) (json.RawMessage, error) {
	if bookmark == "" {
		return nil, errors.New("time travel bookmark is required")
	}
	account, err := c.account(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if c.Backups == nil {
		return nil, errors.New("D1 restore requires the unified R2 backup service")
	}
	if _, err := c.BackupDatabase(ctx, accountID, databaseID); err != nil {
		return nil, fmt.Errorf("backup D1 database before restore: %w", err)
	}
	return doJSON[json.RawMessage](ctx, c, account.APIToken, http.MethodPost,
		"/accounts/"+url.PathEscape(account.CloudflareAccountID)+"/d1/database/"+url.PathEscape(databaseID)+"/time_travel/restore",
		map[string]string{"bookmark": bookmark})
}

func (c Client) BackupDatabase(ctx context.Context, accountID, databaseID string) (Backup, error) {
	if c.Backups == nil || c.DB == nil {
		return Backup{}, errors.New("D1 backup service is not configured")
	}
	account, err := c.account(ctx, accountID)
	if err != nil {
		return Backup{}, err
	}
	backup := Backup{
		ID: uuid.NewString(), AccountID: accountID, DatabaseID: databaseID, Status: "exporting",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	backup.R2ObjectKey = fmt.Sprintf("d1-backups/%s/%s/%s-%s.sql", accountID, databaseID,
		backup.CreatedAt.UTC().Format("20060102T150405Z"), backup.ID)
	if err := c.insertBackup(ctx, backup); err != nil {
		return Backup{}, err
	}
	export, err := c.pollExport(ctx, account.APIToken, account.CloudflareAccountID, databaseID)
	if err != nil {
		_ = c.updateBackup(ctx, backup.ID, "error")
		return Backup{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, export.SignedURL, nil)
	if err != nil {
		_ = c.updateBackup(ctx, backup.ID, "error")
		return Backup{}, err
	}
	response, err := c.httpClient(0).Do(request)
	if err != nil {
		_ = c.updateBackup(ctx, backup.ID, "error")
		return Backup{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = c.updateBackup(ctx, backup.ID, "error")
		return Backup{}, fmt.Errorf("download D1 export returned HTTP %d", response.StatusCode)
	}
	if _, err := c.Backups.Put(ctx, r2.PutRequest{
		Key: backup.R2ObjectKey, Body: response.Body, Size: response.ContentLength,
		ContentType: "application/sql", Metadata: map[string]string{
			"d1-account-id": accountID, "d1-database-id": databaseID, "d1-bookmark": export.Bookmark,
		}, PayloadHash: "UNSIGNED-PAYLOAD",
	}); err != nil {
		_ = c.updateBackup(ctx, backup.ID, "error")
		return Backup{}, err
	}
	backup.Status = "completed"
	backup.UpdatedAt = time.Now()
	if err := c.updateBackup(ctx, backup.ID, backup.Status); err != nil {
		return Backup{}, err
	}
	return backup, nil
}

func (c Client) ListBackups(ctx context.Context, accountID, databaseID string, limit int) ([]Backup, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := c.DB.QueryContext(ctx, `SELECT id, account_id, database_id, r2_object_key, status, created_at, updated_at
		FROM d1_backups WHERE account_id = ? AND (? = '' OR database_id = ?)
		ORDER BY created_at DESC, id DESC LIMIT ?`, accountID, databaseID, databaseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var backups []Backup
	for rows.Next() {
		var backup Backup
		var created, updated int64
		if err := rows.Scan(&backup.ID, &backup.AccountID, &backup.DatabaseID, &backup.R2ObjectKey,
			&backup.Status, &created, &updated); err != nil {
			return nil, err
		}
		backup.CreatedAt, backup.UpdatedAt = time.Unix(0, created), time.Unix(0, updated)
		backups = append(backups, backup)
	}
	return backups, rows.Err()
}

type exportResult struct {
	Bookmark  string `json:"at_bookmark"`
	SignedURL string `json:"signed_url"`
	Error     string `json:"error"`
	Result    struct {
		SignedURL string `json:"signed_url"`
	} `json:"result"`
}

func (c Client) pollExport(ctx context.Context, token, cloudflareAccountID, databaseID string) (exportResult, error) {
	path := "/accounts/" + url.PathEscape(cloudflareAccountID) + "/d1/database/" + url.PathEscape(databaseID) + "/export"
	bookmark := ""
	for attempt := 0; attempt < 20; attempt++ {
		body := map[string]any{"output_format": "polling"}
		if bookmark != "" {
			body["current_bookmark"] = bookmark
		}
		result, err := doJSON[exportResult](ctx, c, token, http.MethodPost, path, body)
		if err != nil {
			return exportResult{}, err
		}
		if result.SignedURL == "" {
			result.SignedURL = result.Result.SignedURL
		}
		if result.Error != "" {
			return exportResult{}, errors.New(result.Error)
		}
		if result.SignedURL != "" {
			if result.Bookmark == "" {
				result.Bookmark = bookmark
			}
			return result, nil
		}
		if result.Bookmark != "" {
			bookmark = result.Bookmark
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return exportResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	return exportResult{}, errors.New("D1 export did not become ready before the polling limit")
}

func (c Client) insertBackup(ctx context.Context, backup Backup) error {
	_, err := c.DB.ExecContext(ctx, `INSERT INTO d1_backups(id, account_id, database_id, r2_object_key, status, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, backup.ID, backup.AccountID, backup.DatabaseID, backup.R2ObjectKey,
		backup.Status, backup.CreatedAt.UnixNano(), backup.UpdatedAt.UnixNano())
	return err
}

func (c Client) updateBackup(ctx context.Context, id, status string) error {
	_, err := c.DB.ExecContext(ctx, "UPDATE d1_backups SET status = ?, updated_at = ? WHERE id = ?", status, time.Now().UnixNano(), id)
	return err
}

func (c Client) History(ctx context.Context, accountID, databaseID string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := c.DB.QueryContext(ctx, `SELECT id, account_id, database_id, sql_text, sql_class, success,
		rows_read, rows_written, duration_ms, error, created_at FROM d1_query_history
		WHERE account_id = ? AND (? = '' OR database_id = ?) ORDER BY created_at DESC, id DESC LIMIT ?`,
		accountID, databaseID, databaseID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []HistoryEntry
	for rows.Next() {
		var entry HistoryEntry
		var class int
		var created int64
		if err := rows.Scan(&entry.ID, &entry.AccountID, &entry.DatabaseID, &entry.SQL, &class, &entry.Success,
			&entry.RowsRead, &entry.RowsWritten, &entry.DurationMS, &entry.Error, &created); err != nil {
			return nil, err
		}
		entry.Class = SQLClass(class)
		entry.CreatedAt = time.Unix(0, created)
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (c Client) recordHistory(ctx context.Context, entry HistoryEntry) error {
	if c.DB == nil {
		return nil
	}
	_, err := c.DB.ExecContext(ctx, `INSERT INTO d1_query_history(
		id, account_id, database_id, sql_text, sql_class, success, rows_read, rows_written,
		duration_ms, error, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.AccountID, entry.DatabaseID, entry.SQL, int(entry.Class), entry.Success,
		entry.RowsRead, entry.RowsWritten, entry.DurationMS, entry.Error, entry.CreatedAt.UnixNano())
	return err
}

func (c Client) account(ctx context.Context, accountID string) (accounts.Account, error) {
	if c.Accounts == nil {
		return accounts.Account{}, errors.New("D1 account store is not configured")
	}
	return c.Accounts.Get(ctx, accountID, true)
}

type apiEnvelope[T any] struct {
	Success bool `json:"success"`
	Result  T    `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func doJSON[T any](ctx context.Context, client Client, token, method, path string, body any) (T, error) {
	var zero T
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return zero, err
		}
		reader = bytes.NewReader(encoded)
	}
	baseURL := strings.TrimRight(client.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.cloudflare.com/client/v4"
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return zero, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := client.httpClient(30 * time.Second)
	response, err := httpClient.Do(request)
	if err != nil {
		return zero, fmt.Errorf("Cloudflare D1 request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return zero, err
	}
	var envelope apiEnvelope[T]
	if err := json.Unmarshal(data, &envelope); err != nil {
		return zero, fmt.Errorf("decode Cloudflare D1 response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		message := fmt.Sprintf("Cloudflare D1 returned HTTP %d", response.StatusCode)
		if len(envelope.Errors) > 0 && envelope.Errors[0].Message != "" {
			message = envelope.Errors[0].Message
		}
		return zero, errors.New(message)
	}
	return envelope.Result, nil
}

func (c Client) httpClient(timeout time.Duration) *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: timeout}
}
