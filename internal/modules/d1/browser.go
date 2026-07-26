package d1

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type SchemaObject struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	TableName string `json:"table_name"`
	SQL       string `json:"sql,omitempty"`
}

type TablePage struct {
	Table   string           `json:"table"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
	HasMore bool             `json:"has_more"`
}

type QueryInsight struct {
	HistoryID string  `json:"history_id"`
	Severity  string  `json:"severity"`
	Category  string  `json:"category"`
	Message   string  `json:"message"`
	SQL       string  `json:"sql"`
	RowsRead  int64   `json:"rows_read"`
	Duration  float64 `json:"duration_ms"`
	CreatedAt string  `json:"created_at"`
}

func (c Client) Schema(ctx context.Context, accountID, databaseID string) ([]SchemaObject, error) {
	results, err := c.Query(ctx, accountID, databaseID, QueryInput{
		SQL: `SELECT name, type, tbl_name, sql FROM sqlite_schema
WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`,
	})
	if err != nil {
		return nil, err
	}
	var schema []SchemaObject
	for _, result := range results {
		for _, row := range result.Results {
			schema = append(schema, SchemaObject{
				Name: stringValue(row["name"]), Type: stringValue(row["type"]),
				TableName: stringValue(row["tbl_name"]), SQL: stringValue(row["sql"]),
			})
		}
	}
	return schema, nil
}

func (c Client) TableRows(ctx context.Context, accountID, databaseID, table string, limit, offset int) (TablePage, error) {
	if strings.TrimSpace(table) == "" {
		return TablePage{}, errors.New("table name is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		return TablePage{}, errors.New("offset cannot be negative")
	}
	exists, err := c.Query(ctx, accountID, databaseID, QueryInput{
		SQL:    "SELECT name FROM sqlite_schema WHERE type IN ('table', 'view') AND name = ? LIMIT 1",
		Params: []any{table},
	})
	if err != nil {
		return TablePage{}, err
	}
	if resultRowCount(exists) == 0 {
		return TablePage{}, ErrTableNotFound
	}
	identifier := quoteIdentifier(table)
	columnResults, err := c.Query(ctx, accountID, databaseID, QueryInput{SQL: "PRAGMA table_xinfo(" + identifier + ")"})
	if err != nil {
		return TablePage{}, err
	}
	columns := orderedColumns(columnResults)
	results, err := c.Query(ctx, accountID, databaseID, QueryInput{
		SQL:    fmt.Sprintf("SELECT * FROM %s LIMIT ? OFFSET ?", identifier),
		Params: []any{limit + 1, offset},
	})
	if err != nil {
		return TablePage{}, err
	}
	var rows []map[string]any
	for _, result := range results {
		rows = append(rows, result.Results...)
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	return TablePage{Table: table, Columns: columns, Rows: rows, Limit: limit, Offset: offset, HasMore: hasMore}, nil
}

func (c Client) Insights(ctx context.Context, accountID, databaseID string, limit int) ([]QueryInsight, error) {
	history, err := c.History(ctx, accountID, databaseID, 500)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var insights []QueryInsight
	for _, entry := range history {
		category, severity, message := analyzeHistoryEntry(entry)
		if category == "" {
			continue
		}
		insights = append(insights, QueryInsight{
			HistoryID: entry.ID, Severity: severity, Category: category, Message: message,
			SQL: entry.SQL, RowsRead: entry.RowsRead, Duration: entry.DurationMS,
			CreatedAt: entry.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
		if len(insights) == limit {
			break
		}
	}
	return insights, nil
}

func analyzeHistoryEntry(entry HistoryEntry) (category, severity, message string) {
	switch {
	case !entry.Success:
		return "failed", "high", "Query failed; review its error and parameters before retrying."
	case entry.DurationMS >= 2000:
		return "latency", "high", "Query duration exceeded 2 seconds; inspect its query plan and indexes."
	case entry.RowsRead >= 100000:
		return "rows_read", "high", "Query scanned at least 100,000 rows; add a selective filter or index."
	case entry.DurationMS >= 500:
		return "latency", "medium", "Query duration exceeded 500 ms; inspect its query plan."
	case entry.RowsRead >= 10000:
		return "rows_read", "medium", "Query scanned at least 10,000 rows; verify filtering and indexes."
	case strings.Contains(strings.ToUpper(entry.SQL), "SELECT *") && entry.RowsRead >= 1000:
		return "projection", "low", "Large SELECT * result; request only the columns the caller needs."
	default:
		return "", "", ""
	}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func resultRowCount(results []QueryResult) int {
	count := 0
	for _, result := range results {
		count += len(result.Results)
	}
	return count
}

func orderedColumns(results []QueryResult) []string {
	type column struct {
		position int
		name     string
	}
	var values []column
	for _, result := range results {
		for _, row := range result.Results {
			values = append(values, column{position: intValue(row["cid"]), name: stringValue(row["name"])})
		}
	}
	sort.SliceStable(values, func(i, j int) bool { return values[i].position < values[j].position })
	columns := make([]string, 0, len(values))
	for _, value := range values {
		if value.name != "" {
			columns = append(columns, value.name)
		}
	}
	return columns
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func intValue(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	case int64:
		return int(number)
	default:
		return 0
	}
}

var ErrTableNotFound = errors.New("D1 table or view not found")
