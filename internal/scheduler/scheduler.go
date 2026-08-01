package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	dailyConsolidationHourUTC = 0
	dailyRunHourUTC           = 1
	NonfatalRetryDelay        = time.Minute
	MaxNonfatalRetries        = 5
)

type Task string

const (
	TaskConsolidation Task = "consolidation"
	TaskFunding       Task = "funding"
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
	onError  func(Task, error)
	onWait   func(Task, time.Time)
}

func New(onError func(Task, error), onWait func(Task, time.Time)) *Scheduler {
	return &Scheduler{
		now:      time.Now,
		newTimer: func(delay time.Duration) Timer { return systemTimer{Timer: time.NewTimer(delay)} },
		onError:  onError,
		onWait:   onWait,
	}
}

func NextScheduledTaskUTC(now time.Time) (Task, time.Time) {
	utc := now.UTC()
	consolidationAt := time.Date(
		utc.Year(), utc.Month(), utc.Day(), dailyConsolidationHourUTC, 0, 0, 0, time.UTC,
	)
	if !consolidationAt.Before(utc) {
		return TaskConsolidation, consolidationAt
	}
	runAt := time.Date(utc.Year(), utc.Month(), utc.Day(), dailyRunHourUTC, 0, 0, 0, time.UTC)
	if !runAt.Before(utc) {
		return TaskFunding, runAt
	}
	return TaskConsolidation, consolidationAt.AddDate(0, 0, 1)
}

func NextDailyRunUTC(now time.Time) time.Time {
	utc := now.UTC()
	nextRunAt := time.Date(utc.Year(), utc.Month(), utc.Day(), dailyRunHourUTC, 0, 0, 0, time.UTC)
	if !nextRunAt.After(utc) {
		nextRunAt = nextRunAt.AddDate(0, 0, 1)
	}
	return nextRunAt
}

func (s *Scheduler) Run(
	ctx context.Context,
	consolidate func(context.Context) error,
	runFunding func(context.Context) error,
) error {
	if ctx == nil {
		return fmt.Errorf("scheduler context is nil")
	}
	if s == nil || s.now == nil || s.newTimer == nil {
		return fmt.Errorf("scheduler is not initialized")
	}
	if consolidate == nil || runFunding == nil {
		return fmt.Errorf("scheduler callbacks are required")
	}
	retries := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := s.now()
		task, nextRunAt := NextScheduledTaskUTC(now)
		if retries > 0 {
			task = TaskFunding
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
			s.onWait(task, nextRunAt)
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.Chan():
			timer.Stop()
		}
		if task == TaskConsolidation {
			if err := consolidate(ctx); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if s.onError != nil {
					s.onError(task, err)
				}
			}
			continue
		}
		if err := runFunding(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if s.onError != nil {
				s.onError(task, err)
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
