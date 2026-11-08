package rollup

import (
	"context"
	"errors"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestHoursIn(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		want     int
		first    string
	}{
		{"aligned", "2026-01-01T10:00:00Z", "2026-01-01T12:00:00Z", 2, "2026-01-01T10:00:00Z"},
		{"widened both ends", "2026-01-01T10:30:00Z", "2026-01-01T12:10:00Z", 3, "2026-01-01T10:00:00Z"},
		{"non-UTC offset", "2026-01-01T10:30:00+02:00", "2026-01-01T10:30:00Z", 3, "2026-01-01T08:00:00Z"},
		{"empty", "2026-01-01T10:00:00Z", "2026-01-01T10:00:00Z", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hoursIn(ts(tt.from), ts(tt.to))
			if len(got) != tt.want {
				t.Fatalf("got %d hours, want %d: %v", len(got), tt.want, got)
			}
			if tt.want > 0 && !got[0].Equal(ts(tt.first)) {
				t.Errorf("first = %v, want %v", got[0], tt.first)
			}
		})
	}
}

func TestRetrySucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := retry(context.Background(), 5, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err = %v, calls = %d; want nil, 3", err, calls)
	}
}

func TestRetryGivesUp(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	err := retry(context.Background(), 3, time.Millisecond, func() error { calls++; return boom })
	if !errors.Is(err, boom) || calls != 3 {
		t.Fatalf("err = %v, calls = %d; want boom, 3", err, calls)
	}
}

func TestRetryStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retry(ctx, 5, time.Hour, func() error { calls++; cancel(); return errors.New("x") })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err = %v, calls = %d; want canceled, 1", err, calls)
	}
}
