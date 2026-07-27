package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"builder-code-bot/internal/config"
	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/hyperliquid"
	httpclient "builder-code-bot/internal/hyperliquid/client"
	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"
	"builder-code-bot/internal/hyperliquid/signing"
	"builder-code-bot/internal/notification"
	"builder-code-bot/internal/state"
)

func ConsolidateFunds(ctx context.Context, configPath string) (err error) {
	if ctx == nil {
		return fmt.Errorf("funding consolidation context is nil")
	}
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	logger := configureLogger(cfg)
	notifier, err := buildNotifier(ctx, cfg)
	if err != nil {
		return fmt.Errorf("initialize notifier: %w", err)
	}
	dispatcher := notification.NewDispatcher(notifier, logger)
	fundingNotifier := dispatcherFundingNotifier{dispatcher: dispatcher}

	password, err := resolvePassword(cfg.Signing.DecryptPassword, newTerminalPromptFile())
	if err != nil {
		return err
	}
	logger.Info(ctx, "consolidation private key initialization started",
		slog.String("event", "startup_consolidation_signer_initialization_started"),
		slog.Int("signer_count", len(cfg.Builders)))
	signers, err := buildBuilderSigners(cfg, password)
	if err != nil {
		return fmt.Errorf("initialize consolidation signers: %w", err)
	}
	logger.Info(ctx, "consolidation private keys initialized",
		slog.String("event", "startup_consolidation_signer_initialization_completed"),
		slog.Int("signer_count", len(signers)))

	processLock, err := state.AcquireProcessLock(state.DataDir)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, processLock.Close()) }()
	logger.Info(ctx, "funding process lock acquired",
		slog.String("event", "startup_process_lock_acquired"),
		slog.String("data_dir", state.DataDir))

	consolidator, err := assembleConsolidator(cfg, signers, fundingNotifier, logger)
	if err != nil {
		return err
	}
	return consolidator.Run(ctx)
}

func assembleConsolidator(
	cfg config.Config,
	signers map[string]signing.PrivateKey,
	notifier funding.Notifier,
	logger funding.Logger,
) (*funding.Consolidator, error) {
	network := hyperliquid.Network(cfg.Hyperliquid.Network)
	baseURL, err := config.ResolveBaseURL(cfg.Hyperliquid)
	if err != nil {
		return nil, err
	}
	transport, err := httpclient.New(httpclient.Config{Network: network, BaseURL: baseURL})
	if err != nil {
		return nil, fmt.Errorf("initialize Hyperliquid transport: %w", err)
	}
	exchangeClient, err := exchange.New(transport, network, signers)
	if err != nil {
		return nil, fmt.Errorf("initialize Hyperliquid exchange client: %w", err)
	}
	builders := make([]string, len(cfg.Builders))
	builderNames := make(map[string]string, len(cfg.Builders))
	for index, builder := range cfg.Builders {
		builders[index] = builder.Address
		builderNames[strings.ToLower(builder.Address)] = builder.Name
	}
	return funding.NewConsolidator(funding.ConsolidatorConfig{
		Chain:        hyperliquidChain{info: info.New(transport), exchange: exchangeClient},
		Notifier:     notifier,
		Logger:       logger,
		Builders:     builders,
		BuilderNames: builderNames,
		Settlement:   cfg.Settlement.Address,
		Network:      cfg.Hyperliquid.Network,
		Clock:        systemClock{},
		Nonce:        signing.NewNonceGenerator(),
	})
}
