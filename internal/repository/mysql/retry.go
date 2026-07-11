package mysql

import (
	"context"
	"math/rand/v2"
	"time"
)

var retryDelays = [...]time.Duration{
	time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
}

type RetryObserver interface {
	RetryStarted(operation string, err error)
	RetryProgress(operation string, attempts int, unavailableFor time.Duration, err error)
	Recovered(operation string, attempts int, unavailableFor time.Duration)
}

type Retryer struct {
	observer RetryObserver
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	jitter   func(time.Duration) time.Duration
}

func NewRetryer(observer RetryObserver) Retryer {
	return Retryer{
		observer: observer,
		now:      time.Now,
		sleep:    sleepContext,
		jitter:   jitterDelay,
	}
}

// Do retries known transient failures without an attempt limit. A task context
// is therefore the sole lifetime bound, including while waiting in backoff.
func (r Retryer) Do(ctx context.Context, operation string, fn func(context.Context) error) error {
	now := r.now
	if now == nil {
		now = time.Now
	}
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	jitter := r.jitter
	if jitter == nil {
		jitter = jitterDelay
	}

	var (
		startedAt time.Time
		failures  int
	)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := fn(ctx)
		if err == nil {
			if failures > 0 && r.observer != nil {
				r.observer.Recovered(operation, failures, now().Sub(startedAt))
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !IsRetryable(err) {
			return err
		}

		failures++
		if failures == 1 {
			startedAt = now()
			if r.observer != nil {
				r.observer.RetryStarted(operation, err)
			}
		} else if r.observer != nil {
			r.observer.RetryProgress(operation, failures, now().Sub(startedAt), err)
		}

		delayIndex := failures - 1
		if delayIndex >= len(retryDelays) {
			delayIndex = len(retryDelays) - 1
		}
		if err := sleep(ctx, jitter(retryDelays[delayIndex])); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
	}
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func jitterDelay(delay time.Duration) time.Duration {
	// Pick an inclusive percentage in [80, 120]. The backoff schedule remains
	// capped at a 30-second base while avoiding synchronized reconnect storms.
	return delay * time.Duration(80+rand.IntN(41)) / 100
}
