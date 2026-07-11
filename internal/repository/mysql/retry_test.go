package mysql

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

type retryEvent struct {
	kind           string
	operation      string
	attempts       int
	unavailableFor time.Duration
}

type recordingRetryObserver struct {
	events []retryEvent
}

func (o *recordingRetryObserver) RetryStarted(operation string, _ error) {
	o.events = append(o.events, retryEvent{kind: "started", operation: operation})
}

func (o *recordingRetryObserver) RetryProgress(operation string, attempts int, unavailableFor time.Duration, _ error) {
	o.events = append(o.events, retryEvent{
		kind:           "progress",
		operation:      operation,
		attempts:       attempts,
		unavailableFor: unavailableFor,
	})
}

func (o *recordingRetryObserver) Recovered(operation string, attempts int, unavailableFor time.Duration) {
	o.events = append(o.events, retryEvent{
		kind:           "recovered",
		operation:      operation,
		attempts:       attempts,
		unavailableFor: unavailableFor,
	})
}

func TestRetryUsesLongLivedBackoffAndReportsRecovery(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	observer := &recordingRetryObserver{}
	retryer := Retryer{
		observer: observer,
		now:      func() time.Time { return now },
		sleep: func(_ context.Context, delay time.Duration) error {
			sleeps = append(sleeps, delay)
			now = now.Add(delay)
			return nil
		},
		jitter: func(delay time.Duration) time.Duration { return delay },
	}

	attempts := 0
	err := retryer.Do(context.Background(), "list_pending", func(context.Context) error {
		attempts++
		if attempts <= 10 {
			return driver.ErrBadConn
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	wantSleeps := []time.Duration{
		time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second,
		30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
		30 * time.Second, 30 * time.Second,
	}
	if len(sleeps) != len(wantSleeps) {
		t.Fatalf("sleep count = %d, want %d (%v)", len(sleeps), len(wantSleeps), sleeps)
	}
	for i := range wantSleeps {
		if sleeps[i] != wantSleeps[i] {
			t.Errorf("sleep[%d] = %s, want %s", i, sleeps[i], wantSleeps[i])
		}
	}

	if got := observer.events[0]; got.kind != "started" || got.operation != "list_pending" {
		t.Fatalf("first event = %+v, want retry started", got)
	}
	last := observer.events[len(observer.events)-1]
	if last.kind != "recovered" || last.attempts != 10 || last.unavailableFor != 198*time.Second {
		t.Fatalf("last event = %+v, want recovery after 10 failures and 3m18s", last)
	}
}

func TestRetryStopsImmediatelyWhenContextIsCanceledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := &recordingRetryObserver{}
	retryer := Retryer{
		observer: observer,
		now:      time.Now,
		sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		},
		jitter: func(delay time.Duration) time.Duration { return delay },
	}

	attempts := 0
	err := retryer.Do(ctx, "complete", func(context.Context) error {
		attempts++
		return driver.ErrBadConn
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do() error = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if len(observer.events) != 1 || observer.events[0].kind != "started" {
		t.Fatalf("events = %+v, want only retry started", observer.events)
	}
}

func TestRetryReturnsPermanentErrorWithoutSleeping(t *testing.T) {
	wantErr := errors.New("scan amount into string")
	observer := &recordingRetryObserver{}
	retryer := NewRetryer(observer)

	attempts := 0
	err := retryer.Do(context.Background(), "list_pending", func(context.Context) error {
		attempts++
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Do() error = %v, want %v", err, wantErr)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if len(observer.events) != 0 {
		t.Fatalf("events = %+v, want none", observer.events)
	}
}
