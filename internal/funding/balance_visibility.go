package funding

import (
	"context"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

const claimBalanceVisibilityDelay = time.Second

type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

type timerSleeper struct{}

func (timerSleeper) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// builderBalanceAfterClaim gives a claim that may have executed one second to
// become visible through the Info API, then reads the builder balance once.
func (o *Orchestrator) builderBalanceAfterClaim(
	ctx context.Context,
	address string,
	token info.Token,
	claim ActionProgress,
) (decimal.Decimal, error) {
	if claim.Prepared != nil && claim.SubmitAttempts > 0 && claim.Phase != ActionRejected {
		visibleAt := time.UnixMilli(int64(claim.Prepared.Nonce)).Add(claimBalanceVisibilityDelay)
		if delay := visibleAt.Sub(o.clock.Now()); delay > 0 {
			if err := o.sleeper.Sleep(ctx, delay); err != nil {
				return decimal.Zero, err
			}
		}
	}
	return o.chain.AvailableSpotBalance(ctx, address, token)
}
