package funding

import (
	"context"
	"log/slog"
	"time"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

type StateStore interface {
	LoadWithMetadata(context.Context) (*RunState, StateLoadMetadata, error)
	Save(context.Context, RunState) error
	Archive(context.Context, RunState, string) error
	Clear(context.Context) error
}

type StateLoadMetadata struct {
	RecoveredFromBackup bool
	PrimaryInvalid      bool
}

type Repository interface {
	ListPending(context.Context) ([]Record, error)
	Complete(context.Context, []uint64) error
}

type Chain interface {
	CanonicalUSDC(context.Context) (info.Token, error)
	ClaimableUSDC(context.Context, string, info.Token) (decimal.Decimal, error)
	SpotBalance(context.Context, string, info.Token) (info.SpotBalanceAmounts, error)
	UserRateLimit(context.Context, string) (info.UserRateLimit, error)
	PrepareClaim(string, uint64) (exchange.PreparedAction, error)
	PrepareSpotSend(string, string, info.Token, decimal.Decimal, uint64) (exchange.PreparedAction, error)
	Submit(context.Context, exchange.PreparedAction) (exchange.SubmitResult, error)
}

type Clock interface{ Now() time.Time }

type NonceSource interface{ Next() uint64 }

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

type Notifier interface {
	Alert(context.Context, string, string)
	Report(context.Context, string, string)
}

type Logger interface {
	Info(context.Context, string, ...slog.Attr)
	Warn(context.Context, string, ...slog.Attr)
	Error(context.Context, string, ...slog.Attr)
}
