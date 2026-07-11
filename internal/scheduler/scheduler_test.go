package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hyperliquid-builder-code-bot/internal/funding"
)

func TestNextUTCMidnight(t *testing.T) {
	now := time.Date(2026, 7, 10, 23, 59, 59, 0, time.FixedZone("local", 8*60*60))
	want := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	if got := NextUTCMidnight(now); !got.Equal(want) {
		t.Fatalf("NextUTCMidnight() = %s, want %s", got, want)
	}
}

func TestNextUTCMidnightIgnoresDSTBoundaries(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{
		time.Date(2026, 3, 8, 1, 59, 59, 0, location),
		time.Date(2026, 11, 1, 1, 59, 59, 0, location),
	} {
		utc := now.UTC()
		want := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
		if got := NextUTCMidnight(now); !got.Equal(want) {
			t.Errorf("NextUTCMidnight(%s) = %s, want %s", now, got, want)
		}
	}
}

func TestRunRecomputesDelayAfterEveryCallbackAndContinuesAfterError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	times := []time.Time{
		time.Date(2026, 7, 10, 23, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 0, 0, 30, 0, time.UTC),
	}
	var nowMu sync.Mutex
	nowIndex := 0
	now := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		value := times[nowIndex]
		if nowIndex < len(times)-1 {
			nowIndex++
		}
		return value
	}
	timers := make(chan *fakeTimer, 2)
	var delaysMu sync.Mutex
	var delays []time.Duration
	s := &Scheduler{
		now: now,
		newTimer: func(delay time.Duration) Timer {
			timer := newFakeTimer()
			delaysMu.Lock()
			delays = append(delays, delay)
			delaysMu.Unlock()
			timers <- timer
			return timer
		},
	}
	callbackErr := errors.New("run failed")
	errorsSeen := make(chan error, 1)
	s.onError = func(err error) { errorsSeen <- err }
	calls := make(chan funding.Trigger, 2)
	callCount := 0
	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, func(_ context.Context, trigger funding.Trigger) error {
			callCount++
			calls <- trigger
			if callCount == 1 {
				return callbackErr
			}
			cancel()
			return nil
		})
	}()

	first := <-timers
	first.fire()
	if got := <-calls; got != funding.TriggerUTC {
		t.Fatalf("first trigger = %q", got)
	}
	if got := <-errorsSeen; !errors.Is(got, callbackErr) {
		t.Fatalf("onError = %v", got)
	}
	second := <-timers
	second.fire()
	if got := <-calls; got != funding.TriggerUTC {
		t.Fatalf("second trigger = %q", got)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}

	delaysMu.Lock()
	defer delaysMu.Unlock()
	want := []time.Duration{time.Hour, 23*time.Hour + 59*time.Minute + 30*time.Second}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %s, want %s", i, delays[i], want[i])
		}
	}
}

func TestRunStopsActiveTimerWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := newFakeTimer()
	s := &Scheduler{
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
		newTimer: func(time.Duration) Timer {
			close(timer.created)
			return timer
		},
	}
	done := make(chan error, 1)
	callbackCalled := make(chan struct{}, 1)
	go func() {
		done <- s.Run(ctx, func(context.Context, funding.Trigger) error {
			callbackCalled <- struct{}{}
			return nil
		})
	}()
	<-timer.created
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	select {
	case <-callbackCalled:
		t.Fatal("callback ran after cancellation")
	default:
	}
	if !timer.stopped() {
		t.Fatal("active timer was not stopped")
	}
}

type fakeTimer struct {
	ch      chan time.Time
	created chan struct{}
	mu      sync.Mutex
	stop    bool
}

func newFakeTimer() *fakeTimer {
	return &fakeTimer{ch: make(chan time.Time, 1), created: make(chan struct{})}
}

func (t *fakeTimer) Chan() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stop = true
	return true
}

func (t *fakeTimer) fire() { t.ch <- time.Now() }

func (t *fakeTimer) stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stop
}
