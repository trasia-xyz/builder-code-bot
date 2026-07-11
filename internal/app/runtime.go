package app

import (
	"context"
	"fmt"

	"hyperliquid-builder-code-bot/internal/funding"
)

type fundingRunner interface {
	Recover(context.Context) error
	RunNew(context.Context, funding.Trigger) error
}

func startRuntime(ctx context.Context, runner fundingRunner, runOnStart bool) error {
	if ctx == nil {
		return fmt.Errorf("runtime context is nil")
	}
	if runner == nil {
		return fmt.Errorf("funding runner is nil")
	}
	if err := runner.Recover(ctx); err != nil {
		return fmt.Errorf("recover current funding run: %w", err)
	}
	if runOnStart {
		if err := runner.RunNew(ctx, funding.TriggerRunOnStart); err != nil {
			return fmt.Errorf("run funding on start: %w", err)
		}
	}
	return nil
}
