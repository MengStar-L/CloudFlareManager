package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID        string         `json:"id"`
	Actor     string         `json:"actor"`
	Protocol  string         `json:"protocol"`
	Action    string         `json:"action"`
	Resource  string         `json:"resource"`
	Result    string         `json:"result"`
	RequestID string         `json:"request_id"`
	Detail    map[string]any `json:"detail"`
	CreatedAt time.Time      `json:"created_at"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Record(ctx context.Context, event Event) (Event, error) {
	if event.Action == "" || event.Result == "" {
		return Event{}, errors.New("audit action and result are required")
	}
	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if event.Detail == nil {
		event.Detail = map[string]any{}
	}
	detail, err := json.Marshal(event.Detail)
	if err != nil {
		return Event{}, fmt.Errorf("encode audit detail: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO audit_events(
		id, actor, protocol, action, resource, result, request_id, detail_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.ID, event.Actor, event.Protocol, event.Action,
		event.Resource, event.Result, event.RequestID, string(detail), event.CreatedAt.UnixNano())
	if err != nil {
		return Event{}, fmt.Errorf("record audit event: %w", err)
	}
	return event, nil
}

func (s *Store) List(ctx context.Context, limit int, action string) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := `SELECT id, actor, protocol, action, resource, result, request_id, detail_json, created_at
		FROM audit_events`
	args := []any{}
	if action != "" {
		query += " WHERE action = ?"
		args = append(args, action)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var event Event
		var detail string
		var created int64
		if err := rows.Scan(&event.ID, &event.Actor, &event.Protocol, &event.Action, &event.Resource,
			&event.Result, &event.RequestID, &detail, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detail), &event.Detail); err != nil {
			return nil, fmt.Errorf("decode audit detail: %w", err)
		}
		event.CreatedAt = time.Unix(0, created)
		events = append(events, event)
	}
	return events, rows.Err()
}
