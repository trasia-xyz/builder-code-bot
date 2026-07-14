package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"hyperliquid-builder-code-bot/internal/config"
	"hyperliquid-builder-code-bot/internal/funding"
	"hyperliquid-builder-code-bot/internal/hyperliquid"
	httpclient "hyperliquid-builder-code-bot/internal/hyperliquid/client"
	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
	"hyperliquid-builder-code-bot/internal/hyperliquid/signing"
	"hyperliquid-builder-code-bot/internal/logging"
	"hyperliquid-builder-code-bot/internal/notification"
	"hyperliquid-builder-code-bot/internal/notification/mail/ses"
	"hyperliquid-builder-code-bot/internal/repository/mysql"
	"hyperliquid-builder-code-bot/internal/scheduler"
	"hyperliquid-builder-code-bot/internal/state"
)

const defaultConfigPath = "./config.toml"

type Options struct {
	ConfigPath string
	RunOnStart bool
}

type scheduledRunner interface {
	Run(context.Context, func(context.Context) error) error
}

type App struct {
	orchestrator *funding.Orchestrator
	scheduler    scheduledRunner
	db           *sql.DB
	processLock  *state.ProcessLock
}

func New(ctx context.Context, opts Options) (_ *App, err error) {
	if ctx == nil {
		return nil, fmt.Errorf("app context is nil")
	}
	configPath := strings.TrimSpace(opts.ConfigPath)
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("load configuration: %w", err)
	}

	logger := logging.New(logging.Config{
		Format:    cfg.Logging.Format,
		Level:     cfg.Logging.Level,
		Color:     cfg.Logging.Color,
		AddSource: cfg.Logging.AddSource,
		SensitiveKeys: []string{
			"decrypt_password", "private_key", "encrypted_private_key", "signature", "signed_request",
		},
	})
	logging.SetDefault(logger)

	notifier, err := buildNotifier(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("initialize notifier: %w", err)
	}
	dispatcher := notification.NewDispatcher(notifier, logger)
	fundingNotifier := dispatcherFundingNotifier{dispatcher: dispatcher}

	password, err := resolvePassword(cfg.Signing.DecryptPassword, newTerminalPromptFile())
	if err != nil {
		dispatcher.Alert(ctx, "startup_signing", notification.Message{
			Subject: "Funding service startup failed", Body: "Private key password resolution failed.",
		})
		return nil, err
	}
	logger.Info(ctx, "private key initialization started",
		slog.String("event", "startup_signer_initialization_started"),
		slog.Int("signer_count", len(cfg.Builders)+1))
	signers, err := buildSigners(cfg, password)
	if err != nil {
		dispatcher.Alert(ctx, "startup_signing", notification.Message{
			Subject: "Funding service startup failed", Body: "Private key decryption or address validation failed.",
		})
		return nil, err
	}
	logger.Info(ctx, "private keys initialized",
		slog.String("event", "startup_signer_initialization_completed"),
		slog.Int("signer_count", len(signers)))

	db, err := mysql.Open(cfg.MySQL)
	if err != nil {
		return nil, fmt.Errorf("initialize MySQL pool: %w", err)
	}
	logger.Info(ctx, "MySQL pool initialized",
		slog.String("event", "startup_mysql_pool_initialized"))
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()

	processLock, err := state.AcquireProcessLock(state.DataDir)
	if err != nil {
		return nil, err
	}
	logger.Info(ctx, "funding process lock acquired",
		slog.String("event", "startup_process_lock_acquired"),
		slog.String("data_dir", state.DataDir))
	defer func() {
		if err != nil {
			_ = processLock.Close()
		}
	}()

	orchestrator, err := assembleOrchestrator(cfg, signers, db, dispatcher, fundingNotifier, logger)
	if err != nil {
		return nil, err
	}
	logger.Info(ctx, "funding runtime starting",
		slog.String("event", "startup_funding_runtime_started"),
		slog.Bool("run_on_start", opts.RunOnStart))
	if err := startRuntime(ctx, orchestrator, opts.RunOnStart); err != nil {
		return nil, err
	}

	utcScheduler := scheduler.New(
		func(runErr error) {
			logger.Error(context.Background(), "scheduled funding run failed",
				slog.String("event", "scheduled_funding_run_failed"),
				slog.String("error", sanitizeRuntimeError(runErr)),
			)
		},
		func(nextRunAt time.Time) {
			logger.Info(context.Background(), "next funding run scheduled",
				slog.String("event", "funding_next_run_scheduled"),
				slog.Time("next_run_at", nextRunAt),
			)
		},
	)
	return &App{
		orchestrator: orchestrator, scheduler: utcScheduler,
		db: db, processLock: processLock,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("app run context is nil")
	}
	if a == nil || a.scheduler == nil || a.orchestrator == nil {
		return fmt.Errorf("app is not initialized")
	}
	return a.scheduler.Run(ctx, func(runCtx context.Context) error {
		return a.orchestrator.Run(runCtx, funding.TriggerUTC)
	})
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	var dbErr, lockErr error
	if a.db != nil {
		dbErr = a.db.Close()
	}
	if a.processLock != nil {
		lockErr = a.processLock.Close()
	}
	return errors.Join(dbErr, lockErr)
}

func buildNotifier(ctx context.Context, cfg config.Config) (notification.Notifier, error) {
	if !cfg.Notification.Enabled {
		return notification.Noop{}, nil
	}
	return ses.New(ctx, cfg.AWS, cfg.Notification.SES)
}

func assembleOrchestrator(
	cfg config.Config,
	signers map[string]signing.PrivateKey,
	db *sql.DB,
	dispatcher *notification.Dispatcher,
	notifier funding.Notifier,
	logger logging.Logger,
) (*funding.Orchestrator, error) {
	network := hyperliquid.Network(cfg.Hyperliquid.Network)
	baseURL, err := config.ResolveBaseURL(cfg.Hyperliquid)
	if err != nil {
		return nil, err
	}
	transport, err := httpclient.New(httpclient.Config{Network: network, BaseURL: baseURL})
	if err != nil {
		return nil, fmt.Errorf("initialize Hyperliquid transport: %w", err)
	}
	infoClient := info.New(transport)
	exchangeClient, err := exchange.New(transport, network, signers)
	if err != nil {
		return nil, fmt.Errorf("initialize Hyperliquid exchange client: %w", err)
	}
	retryObserver := notification.NewMySQLRetryObserver(dispatcher, logger)
	repository := mysql.NewRepository(db, mysql.NewRetryer(retryObserver))
	builders := make([]string, len(cfg.Builders))
	for index, builder := range cfg.Builders {
		builders[index] = builder.Address
	}
	return funding.NewOrchestrator(funding.OrchestratorConfig{
		Repository: repository,
		Store:      state.NewStore(state.DataDir),
		Chain:      hyperliquidChain{info: infoClient, exchange: exchangeClient},
		Notifier:   notifier,
		Logger:     logger,
		Builders:   builders,
		Settlement: cfg.Settlement.Address,
		Recipient:  cfg.Payout.RecipientAddress,
		Clock:      systemClock{},
		Nonce:      signing.NewNonceGenerator(),
	})
}

type hyperliquidChain struct {
	info     *info.Client
	exchange *exchange.Client
}

func (c hyperliquidChain) CanonicalUSDC(ctx context.Context) (info.Token, error) {
	return c.info.CanonicalUSDC(ctx)
}

func (c hyperliquidChain) SpotBalance(ctx context.Context, address string, token info.Token) (info.SpotBalanceAmounts, error) {
	return c.info.SpotBalance(ctx, address, token)
}

func (c hyperliquidChain) PrepareClaim(address string, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareClaim(address, nonce)
}

func (c hyperliquidChain) PrepareSpotSend(address, destination string, token info.Token, amount decimal.Decimal, nonce uint64) (exchange.PreparedAction, error) {
	return c.exchange.PrepareSpotSend(address, destination, token, amount, nonce)
}

func (c hyperliquidChain) Submit(ctx context.Context, action exchange.PreparedAction) (exchange.SubmitResult, error) {
	return c.exchange.Submit(ctx, action)
}

type dispatcherFundingNotifier struct{ dispatcher *notification.Dispatcher }

func (n dispatcherFundingNotifier) Alert(ctx context.Context, key, message string) {
	if n.dispatcher != nil {
		n.dispatcher.Alert(ctx, key, notification.Message{Subject: "Funding service alert", Body: message})
	}
}

func (n dispatcherFundingNotifier) Report(ctx context.Context, subject, message string) {
	if n.dispatcher != nil {
		n.dispatcher.Report(ctx, notification.Message{Subject: subject, Body: message})
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func sanitizeRuntimeError(err error) string {
	if err == nil {
		return ""
	}
	return "funding run failed"
}
