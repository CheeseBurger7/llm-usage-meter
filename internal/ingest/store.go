package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CheeseBurger7/llm-usage-meter/internal/storage/gen"
)

// Result reports what happened to an ingested event.
type Result int

const (
	// Created means the event was stored for the first time.
	Created Result = iota
	// Duplicate means an identical event was already stored; nothing changed.
	Duplicate
)

// Store persists events in Postgres.
type Store struct {
	q *gen.Queries
}

func NewStore(db gen.DBTX) *Store {
	return &Store{q: gen.New(db)}
}

// Ingest stores the event idempotently on event_id. It returns Created for a new
// event, Duplicate for an identical re-send, and ErrConflict when the event_id
// exists with any different field (including timestamp).
func (s *Store) Ingest(ctx context.Context, ev Event) (Result, error) {
	if err := ev.Validate(); err != nil {
		return 0, err
	}
	ev = ev.normalized()

	_, err := s.q.InsertEvent(ctx, gen.InsertEventParams{
		EventID:      ev.EventID,
		TenantID:     ev.TenantID,
		Model:        ev.Model,
		InputTokens:  ev.InputTokens,
		OutputTokens: ev.OutputTokens,
		LatencyMs:    ev.LatencyMs,
		EventTime:    pgtype.Timestamptz{Time: ev.Timestamp, Valid: true},
	})
	if err == nil {
		return Created, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("insert event: %w", err)
	}

	// ON CONFLICT DO NOTHING waits for a concurrent inserter to commit, so the
	// winning row is visible to this read.
	existing, err := s.q.GetEvent(ctx, ev.EventID)
	if err != nil {
		return 0, fmt.Errorf("load existing event: %w", err)
	}
	stored := Event{
		EventID:      existing.EventID,
		TenantID:     existing.TenantID,
		Model:        existing.Model,
		InputTokens:  existing.InputTokens,
		OutputTokens: existing.OutputTokens,
		LatencyMs:    existing.LatencyMs,
		Timestamp:    existing.EventTime.Time.UTC(),
	}
	if !stored.sameAs(ev) {
		return 0, ErrConflict
	}
	return Duplicate, nil
}
