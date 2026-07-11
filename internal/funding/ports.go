package funding

import (
	"context"
	"log/slog"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

type StateStore interface {
	Load(context.Context) (*RunState, error)
	Save(context.Context, RunState) error
	Archive(context.Context, RunState, string) error
	Clear(context.Context) error
}

// StateLoadMetadata exposes recovery observability without expanding the
// minimal StateStore contract required by tests and alternative stores.
type StateLoadMetadata struct {
	RecoveredFromBackup bool
	PrimaryInvalid      bool
}

type StateStoreWithLoadMetadata interface {
	LoadWithMetadata(context.Context) (*RunState, StateLoadMetadata, error)
}

type Repository interface {
	ListPending(context.Context) ([]Record, error)
	Complete(context.Context, []uint64) error
}

type Chain interface {
	CanonicalUSDC(context.Context) (info.Token, error)
	AvailableSpotBalance(context.Context, string, info.Token) (decimal.Decimal, error)
	PrepareClaim(string, uint64) (exchange.PreparedAction, error)
	PrepareSpotSend(string, string, info.Token, decimal.Decimal, uint64) (exchange.PreparedAction, error)
	Submit(context.Context, exchange.PreparedAction) (exchange.SubmitResult, error)
	FindSpotTransfer(context.Context, info.TransferQuery) (*info.LedgerUpdate, bool, error)
}

type Clock interface{ Now() time.Time }

type NonceSource interface{ Next() uint64 }

type Notifier interface {
	Alert(context.Context, string, string) error
}

// Logger is the smallest structured logging surface needed by funding. The
// internal/logging.Logger implementation satisfies it without making the
// funding package depend on a concrete logger.
type Logger interface {
	Info(context.Context, string, ...slog.Attr)
	Warn(context.Context, string, ...slog.Attr)
	Error(context.Context, string, ...slog.Attr)
}
