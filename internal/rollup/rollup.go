// Package rollup aggregates events into hourly usage rows.
//
// Every rollup row is recomputed from the raw events table, never incremented,
// so running a window twice yields the same rows and backfill is always safe.
package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/CheeseBurger7/llm-usage-meter/internal/storage/gen"
)

// Roller recomputes hourly usage rows.
type Roller struct {
	q       *gen.Queries
	log     *slog.Logger
	retries int
	backoff time.Duration
}

func New(db gen.DBTX, log *slog.Logger) *Roller {
	return &Roller{q: gen.New(db), log: log, retries: 5, backoff: 200 * time.Millisecond}
}

// Rollup recomputes every hour overlapping [from, to). Both bounds are widened
// to whole UTC hours. Each hour is one atomic statement, retried with
// exponential backoff, so an interrupted run leaves only whole hours behind.
func (r *Roller) Rollup(ctx context.Context, from, to time.Time) error {
	for _, h := range hoursIn(from, to) {
		if err := r.rollupHour(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

// BackfillStale recomputes every hour that has events but a missing or outdated
// rollup row, and returns how many hours it processed.
func (r *Roller) BackfillStale(ctx context.Context) (int, error) {
	stale, err := r.q.ListStaleHours(ctx)
	if err != nil {
		return 0, fmt.Errorf("list stale hours: %w", err)
	}
	for i, h := range stale {
		if err := r.rollupHour(ctx, h.Time.UTC()); err != nil {
			return i, err
		}
	}
	return len(stale), nil
}

// Run re-rolls the trailing window every interval until ctx is cancelled. The
// previous hour is included so late-arriving events are picked up.
func (r *Roller) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cur := now.UTC().Truncate(time.Hour)
			if err := r.Rollup(ctx, cur.Add(-time.Hour), cur.Add(time.Hour)); err != nil && ctx.Err() == nil {
				r.log.Error("scheduled rollup failed", "err", err)
			}
		}
	}
}

func (r *Roller) rollupHour(ctx context.Context, hour time.Time) error {
	params := gen.RollupWindowParams{
		WindowStart: pgtype.Timestamptz{Time: hour, Valid: true},
		WindowEnd:   pgtype.Timestamptz{Time: hour.Add(time.Hour), Valid: true},
	}
	return retry(ctx, r.retries, r.backoff, func() error {
		n, err := r.q.RollupWindow(ctx, params)
		if err != nil {
			return err
		}
		r.log.Debug("rolled up hour", "hour", hour, "buckets", n)
		return nil
	})
}

// hoursIn returns the start of every UTC hour overlapping [from, to).
func hoursIn(from, to time.Time) []time.Time {
	var out []time.Time
	for h := from.UTC().Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		out = append(out, h)
	}
	return out
}

// retry runs fn up to attempts times, doubling the wait between tries.
func retry(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	var err error
	wait := base
	for i := 0; i < attempts; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait *= 2
	}
	return fmt.Errorf("after %d attempts: %w", attempts, err)
}
