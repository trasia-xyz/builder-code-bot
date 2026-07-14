package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	NonfatalRetryDelay = time.Minute
	MaxNonfatalRetries = 5
)

// RetryExhaustedError terminates the service after a scheduled funding task
// has used all automatic retries without succeeding.
type RetryExhaustedError struct {
	Retries int
	Err     error
}

func (e *RetryExhaustedError) Error() string {
	return fmt.Sprintf("scheduled funding task failed after %d retries: %v", e.Retries, e.Err)
}

func (e *RetryExhaustedError) Unwrap() error { return e.Err }

type Timer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type Scheduler struct {
	now      func() time.Time
	newTimer func(time.Duration) Timer
	onError  func(error)
	onWait   func(time.Time)
}

func New(onError func(error), onWait func(time.Time)) *Scheduler {
	return &Scheduler{
		now:      time.Now,
		newTimer: func(delay time.Duration) Timer { return systemTimer{Timer: time.NewTimer(delay)} },
		onError:  onError,
		onWait:   onWait,
	}
}

func NextUTCMidnight(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}

func (s *Scheduler) Run(ctx context.Context, run func(context.Context) error) error {
	if ctx == nil {
		return fmt.Errorf("scheduler context is nil")
	}
	if s == nil || s.now == nil || s.newTimer == nil {
		return fmt.Errorf("scheduler is not initialized")
	}
	if run == nil {
		return fmt.Errorf("scheduler callback is nil")
	}
	retries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := s.now()
		nextRunAt := NextUTCMidnight(now)
		if retries > 0 {
			nextRunAt = now.UTC().Add(NonfatalRetryDelay)
		}
		delay := nextRunAt.Sub(now)
		if delay < 0 {
			return fmt.Errorf("next scheduled run precedes current time")
		}
		timer := s.newTimer(delay)
		if timer == nil || timer.Chan() == nil {
			return fmt.Errorf("scheduler timer is not initialized")
		}
		if s.onWait != nil {
			s.onWait(nextRunAt)
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.Chan():
			timer.Stop()
		}
		if err := run(ctx); err != nil {
			if s.onError != nil {
				s.onError(err)
			}
			if isFatal(err) {
				return err
			}
			if retries >= MaxNonfatalRetries {
				return &RetryExhaustedError{Retries: retries, Err: err}
			}
			retries++
			continue
		}
		retries = 0
	}
}

type fatalError interface{ Fatal() }

func isFatal(err error) bool {
	var fatal fatalError
	return errors.As(err, &fatal)
}

type systemTimer struct{ *time.Timer }

func (t systemTimer) Chan() <-chan time.Time { return t.C }
