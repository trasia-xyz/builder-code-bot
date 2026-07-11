package scheduler

import (
	"context"
	"fmt"
	"time"

	"hyperliquid-builder-code-bot/internal/funding"
)

type Timer interface {
	Chan() <-chan time.Time
	Stop() bool
}

type Scheduler struct {
	now      func() time.Time
	newTimer func(time.Duration) Timer
	onError  func(error)
}

func New(onError func(error)) *Scheduler {
	return &Scheduler{
		now:      time.Now,
		newTimer: func(delay time.Duration) Timer { return systemTimer{Timer: time.NewTimer(delay)} },
		onError:  onError,
	}
}

func NextUTCMidnight(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
}

func (s *Scheduler) Run(ctx context.Context, run func(context.Context, funding.Trigger) error) error {
	if ctx == nil {
		return fmt.Errorf("scheduler context is nil")
	}
	if s == nil || s.now == nil || s.newTimer == nil {
		return fmt.Errorf("scheduler is not initialized")
	}
	if run == nil {
		return fmt.Errorf("scheduler callback is nil")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := s.now()
		delay := NextUTCMidnight(now).Sub(now)
		if delay < 0 {
			return fmt.Errorf("next UTC midnight precedes current time")
		}
		timer := s.newTimer(delay)
		if timer == nil || timer.Chan() == nil {
			return fmt.Errorf("scheduler timer is not initialized")
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.Chan():
			timer.Stop()
		}
		if err := run(ctx, funding.TriggerUTC); err != nil && s.onError != nil {
			s.onError(err)
		}
	}
}

type systemTimer struct{ *time.Timer }

func (t systemTimer) Chan() <-chan time.Time { return t.C }
