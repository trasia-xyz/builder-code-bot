package mysql

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"
)

func TestIntegrationRetryerRecoversAfterMultiMinuteOutage(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	started := now
	observer := &recordingRetryObserver{}
	retryer := Retryer{
		observer: observer,
		now:      func() time.Time { return now },
		sleep: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
		jitter: func(delay time.Duration) time.Duration { return delay },
	}

	attempts := 0
	err := retryer.Do(context.Background(), "complete_funding_records", func(context.Context) error {
		attempts++
		if attempts <= 11 {
			return driver.ErrBadConn
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 12 || now.Sub(started) != 228*time.Second {
		t.Fatalf("attempts = %d, simulated outage = %s", attempts, now.Sub(started))
	}
	last := observer.events[len(observer.events)-1]
	if last.kind != "recovered" || last.operation != "complete_funding_records" || last.attempts != 11 || last.unavailableFor != 228*time.Second {
		t.Fatalf("recovery event = %+v", last)
	}
}
