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
	var nextRuns []time.Time
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
		onWait: func(nextRunAt time.Time) {
			delaysMu.Lock()
			nextRuns = append(nextRuns, nextRunAt)
			delaysMu.Unlock()
		},
	}
	callbackErr := errors.New("run failed")
	errorsSeen := make(chan error, 1)
	s.onError = func(err error) { errorsSeen <- err }
	calls := make(chan struct{}, 2)
	callCount := 0
	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, func(context.Context) error {
			callCount++
			calls <- struct{}{}
			if callCount == 1 {
				return callbackErr
			}
			cancel()
			return nil
		})
	}()

	first := <-timers
	first.fire()
	<-calls
	if got := <-errorsSeen; !errors.Is(got, callbackErr) {
		t.Fatalf("onError = %v", got)
	}
	second := <-timers
	second.fire()
	<-calls
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}

	delaysMu.Lock()
	defer delaysMu.Unlock()
	want := []time.Duration{time.Hour, NonfatalRetryDelay}
	if len(delays) != len(want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Errorf("delay[%d] = %s, want %s", i, delays[i], want[i])
		}
	}
	wantNextRuns := []time.Time{
		time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 0, 1, 30, 0, time.UTC),
	}
	if len(nextRuns) != len(wantNextRuns) {
		t.Fatalf("next runs = %v, want %v", nextRuns, wantNextRuns)
	}
	for i := range wantNextRuns {
		if !nextRuns[i].Equal(wantNextRuns[i]) {
			t.Errorf("next run[%d] = %s, want %s", i, nextRuns[i], wantNextRuns[i])
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
		done <- s.Run(ctx, func(context.Context) error {
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

func TestRunReturnsFatalFundingError(t *testing.T) {
	timer := newFakeTimer()
	s := &Scheduler{
		now:      func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
		newTimer: func(time.Duration) Timer { return timer },
	}
	want := &funding.FatalError{Err: errors.New("payout blocked")}
	done := make(chan error, 1)
	go func() {
		done <- s.Run(context.Background(), func(context.Context) error { return want })
	}()
	timer.fire()
	if err := <-done; !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want fatal payout error", err)
	}
}

func TestRunExitsAfterFiveNonfatalRetries(t *testing.T) {
	timers := make(chan *fakeTimer, MaxNonfatalRetries+1)
	var (
		mu     sync.Mutex
		delays []time.Duration
	)
	s := &Scheduler{
		now: func() time.Time { return time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC) },
		newTimer: func(delay time.Duration) Timer {
			timer := newFakeTimer()
			mu.Lock()
			delays = append(delays, delay)
			mu.Unlock()
			timers <- timer
			return timer
		},
	}
	want := errors.New("temporary funding failure")
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- s.Run(context.Background(), func(context.Context) error {
			calls++
			return want
		})
	}()
	for range MaxNonfatalRetries + 1 {
		(<-timers).fire()
	}
	err := <-done
	var exhausted *RetryExhaustedError
	if !errors.As(err, &exhausted) || !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want retry exhausted wrapping callback error", err)
	}
	if exhausted.Retries != MaxNonfatalRetries || calls != MaxNonfatalRetries+1 {
		t.Fatalf("retries = %d, calls = %d", exhausted.Retries, calls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) != MaxNonfatalRetries+1 || delays[0] != 12*time.Hour {
		t.Fatalf("delays = %v", delays)
	}
	for index, delay := range delays[1:] {
		if delay != NonfatalRetryDelay {
			t.Errorf("retry delay[%d] = %s", index, delay)
		}
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
