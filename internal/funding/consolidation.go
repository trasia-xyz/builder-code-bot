package funding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"builder-code-bot/internal/logging"
)

type ConsolidatorConfig struct {
	Chain        Chain
	Notifier     Notifier
	Logger       Logger
	Builders     []string
	BuilderNames map[string]string
	Settlement   string
	Network      string
	Clock        Clock
	Nonce        NonceSource
	Sleeper      Sleeper
}

type Consolidator struct {
	orchestrator *Orchestrator
}

func NewConsolidator(cfg ConsolidatorConfig) (*Consolidator, error) {
	if cfg.Chain == nil || cfg.Clock == nil || cfg.Nonce == nil {
		return nil, fmt.Errorf("funding consolidator dependencies are required")
	}
	settlement := strings.TrimSpace(cfg.Settlement)
	if len(cfg.Builders) == 0 || settlement == "" {
		return nil, fmt.Errorf("funding consolidation accounts are required")
	}
	seen := make(map[string]struct{}, len(cfg.Builders))
	for _, builder := range cfg.Builders {
		address := strings.ToLower(strings.TrimSpace(builder))
		if address == "" {
			return nil, fmt.Errorf("builder address is required")
		}
		if strings.EqualFold(address, settlement) {
			return nil, fmt.Errorf("settlement account must not be a builder")
		}
		if _, duplicate := seen[address]; duplicate {
			return nil, fmt.Errorf("duplicate builder address %q", builder)
		}
		seen[address] = struct{}{}
	}
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	return &Consolidator{orchestrator: &Orchestrator{
		chain:        cfg.Chain,
		notifier:     cfg.Notifier,
		logger:       cfg.Logger,
		builders:     append([]string(nil), cfg.Builders...),
		builderNames: cloneBuilderNames(cfg.BuilderNames),
		settlement:   settlement,
		network:      cfg.Network,
		clock:        cfg.Clock,
		nonce:        cfg.Nonce,
		sleeper:      sleeper,
		alerts:       make(map[string]struct{}),
		lowLimits:    make(map[string]struct{}),
	}}, nil
}

func (c *Consolidator) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("funding consolidation context is nil")
	}
	if c == nil || c.orchestrator == nil {
		return fmt.Errorf("funding consolidator is not initialized")
	}
	_, err := c.orchestrator.runConsolidation(ctx, TriggerConsolidateOnly, nil)
	return err
}

func (o *Orchestrator) runConsolidation(
	ctx context.Context,
	trigger Trigger,
	onClaimablesCaptured func(*RunReport) error,
) (report RunReport, err error) {
	state, err := o.newState(trigger)
	if err != nil {
		return report, err
	}
	defer o.forgetAlerts(state.RunID)
	state.Manifest.Settlement = o.settlement
	report = newRunReport(
		o.clock.Now(), trigger, ReportExecution{}, o.network,
		o.builders, o.builderNames, o.settlement, "",
	)
	report.Outcome = "completed"
	report.syncState(&state)
	report.setStage("snapshot", ReportStepSkipped, "仅执行内部资金归集")
	report.setStage("payout", ReportStepSkipped, "仅归集，不执行 Payout")
	report.setStage("database", ReportStepSkipped, "不读取或更新 funding records")
	report.Payout.Status = ReportStepSkipped
	defer func() {
		if !errors.Is(err, context.Canceled) {
			o.observeUserRateLimits(rateLimitObservationContext(ctx), &report)
		}
	}()
	defer func() {
		if !errors.Is(err, context.Canceled) {
			o.observeSettlementFinalBalance(rateLimitObservationContext(ctx), &report)
		}
	}()
	o.info(ctx, "funding consolidation task started",
		logging.ConsoleSeparator(),
		logging.ConsolePrefix(fundingTaskConsolePrefix),
		slog.String("event", "funding_consolidation_started"),
		slog.String("run_id", state.RunID),
		slog.Int("builder_count", len(o.builders)))

	token, err := o.queryConsolidationToken(ctx, &state)
	if err != nil {
		report.fail("snapshot", "Canonical USDC metadata 查询失败。", "")
		return report, err
	}
	state.Manifest.Token = &token
	report.token = cloneToken(&token)
	claimables, err := o.captureBuilderClaimables(ctx, &state, &report, token)
	if err != nil {
		return report, err
	}
	report.ClaimablesCapturedAt = o.clock.Now().UTC()
	if onClaimablesCaptured != nil {
		if err := onClaimablesCaptured(&report); err != nil {
			return report, err
		}
	}
	claimBalanceTargets, err := o.claimBuilderRewards(
		ctx, &state, &report, claimables,
	)
	if err != nil {
		return report, err
	}
	initialBalances, unconvergedClaims, err := o.observeClaimBalances(
		ctx, &state, &report, token, claimBalanceTargets,
	)
	if err != nil {
		return report, err
	}
	if err := o.sweepBuilderBalances(
		ctx, &state, &report, token,
		initialBalances, claimBalanceTargets, unconvergedClaims,
	); err != nil {
		return report, err
	}
	report.setStage("sweep", aggregateBuilderStep(report.Builders, false), builderStepSummary(report.Builders, false))
	o.info(ctx, "funding consolidation task completed",
		slog.String("event", "funding_consolidation_completed"),
		slog.String("run_id", state.RunID),
		slog.Int("builder_count", len(o.builders)),
		slog.Int("warning_count", len(report.Warnings)))
	return report, nil
}
