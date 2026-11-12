package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
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

// Ingest stores the event for the org, idempotently on (org, event_id). It
// returns Created for a new event, Duplicate for an identical re-send, and
// ErrConflict when the org already has that event_id with any different field
// (including timestamp). Another org's events are never read or compared.
//
// The org must come from the verified API key, never from the request payload.
func (s *Store) Ingest(ctx context.Context, org auth.OrgID, ev Event) (Result, error) {
	if err := ev.Validate(); err != nil {
		return 0, err
	}
	ev = ev.normalized()
	orgID := pgtype.UUID{Bytes: org, Valid: true}

	_, err := s.q.InsertEvent(ctx, gen.InsertEventParams{
		OrgID:            orgID,
		EventID:          ev.EventID,
		TenantID:         ev.TenantID,
		Provider:         ev.Provider,
		Model:            ev.Model,
		Feature:          ev.Feature,
		InputTokens:      ev.InputTokens,
		OutputTokens:     ev.OutputTokens,
		CacheReadTokens:  ev.CacheReadTokens,
		CacheWriteTokens: ev.CacheWriteTokens,
		LatencyMs:        ev.LatencyMs,
		EventTime:        pgtype.Timestamptz{Time: ev.Timestamp, Valid: true},
	})
	if err == nil {
		return Created, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("insert event: %w", err)
	}

	// ON CONFLICT DO NOTHING waits for a concurrent inserter to commit, so the
	// winning row is visible to this read.
	existing, err := s.q.GetEvent(ctx, gen.GetEventParams{OrgID: orgID, EventID: ev.EventID})
	if err != nil {
		return 0, fmt.Errorf("load existing event: %w", err)
	}
	stored := Event{
		EventID:          existing.EventID,
		TenantID:         existing.TenantID,
		Provider:         existing.Provider,
		Model:            existing.Model,
		Feature:          existing.Feature,
		InputTokens:      existing.InputTokens,
		OutputTokens:     existing.OutputTokens,
		CacheReadTokens:  existing.CacheReadTokens,
		CacheWriteTokens: existing.CacheWriteTokens,
		LatencyMs:        existing.LatencyMs,
		Timestamp:        existing.EventTime.Time.UTC(),
	}
	if !stored.sameAs(ev) {
		return 0, ErrConflict
	}
	return Duplicate, nil
}
