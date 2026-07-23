package funding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

type OrchestratorConfig struct {
	Repository   Repository
	Store        StateStore
	Chain        Chain
	Notifier     Notifier
	Logger       Logger
	Builders     []string
	BuilderNames map[string]string
	Settlement   string
	Recipient    string
	Network      string
	Clock        Clock
	Nonce        NonceSource
	Sleeper      Sleeper
}

type Orchestrator struct {
	repository   Repository
	store        StateStore
	chain        Chain
	notifier     Notifier
	logger       Logger
	builders     []string
	builderNames map[string]string
	settlement   string
	recipient    string
	network      string
	clock        Clock
	nonce        NonceSource
	sleeper      Sleeper
	alertMu      sync.Mutex
	alerts       map[string]struct{}
	lowLimits    map[string]struct{}
}

const (
	builderConvergenceAttempts = 5
	builderConvergenceInterval = time.Second
	claimRewardThresholdUSDC   = int64(1)
	zeroEthereumAddress        = "0x0000000000000000000000000000000000000000"
)

func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Repository == nil || cfg.Store == nil || cfg.Chain == nil || cfg.Clock == nil || cfg.Nonce == nil {
		return nil, fmt.Errorf("funding orchestrator dependencies are required")
	}
	if len(cfg.Builders) == 0 || strings.TrimSpace(cfg.Settlement) == "" || strings.TrimSpace(cfg.Recipient) == "" {
		return nil, fmt.Errorf("funding accounts are required")
	}
	seen := make(map[string]struct{}, len(cfg.Builders))
	for _, builder := range cfg.Builders {
		address := strings.ToLower(strings.TrimSpace(builder))
		if address == "" {
			return nil, fmt.Errorf("builder address is required")
		}
		if strings.EqualFold(address, strings.TrimSpace(cfg.Settlement)) {
			return nil, fmt.Errorf("settlement account must not be a builder")
		}
		if _, duplicate := seen[address]; duplicate {
			return nil, fmt.Errorf("duplicate builder address %q", builder)
		}
		seen[address] = struct{}{}
	}
	recipient := strings.TrimSpace(cfg.Recipient)
	if strings.EqualFold(recipient, zeroEthereumAddress) {
		return nil, fmt.Errorf("recipient account must not be the zero address")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Settlement), recipient) {
		return nil, fmt.Errorf("settlement and recipient accounts must differ")
	}
	if _, controlled := seen[strings.ToLower(recipient)]; controlled {
		return nil, fmt.Errorf("recipient account must not be a builder")
	}
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	return &Orchestrator{
		repository: cfg.Repository, store: cfg.Store, chain: cfg.Chain,
		notifier: cfg.Notifier, logger: cfg.Logger,
		builders:     append([]string(nil), cfg.Builders...),
		builderNames: cloneBuilderNames(cfg.BuilderNames),
		settlement:   cfg.Settlement, recipient: cfg.Recipient, network: cfg.Network,
		clock: cfg.Clock, nonce: cfg.Nonce, sleeper: sleeper,
		alerts:    make(map[string]struct{}),
		lowLimits: make(map[string]struct{}),
	}, nil
}

func (o *Orchestrator) runNew(ctx context.Context, trigger Trigger, report *RunReport) error {
	o.info(ctx, "pending funding records query started",
		slog.String("event", "funding_pending_records_query_started"))
	records, err := o.repository.ListPending(ctx)
	if err != nil {
		return err
	}
	o.info(ctx, "pending funding records query completed",
		slog.String("event", "funding_pending_records_query_completed"),
		slog.Int("record_count", len(records)))
	report.RecordCount, report.RecordsRead = len(records), true
	if len(records) == 0 {
		report.Outcome = "no_data"
		report.setStage("snapshot", ReportStepSuccess, "没有待处理记录")
		for _, key := range []string{"rewards", "sweep", "payout", "database"} {
			report.setStage(key, ReportStepSkipped, "无需执行")
		}
		markBuilderStepsSkipped(report, "无待处理 records")
		o.info(ctx, "no pending funding records", slog.String("event", "funding_no_data"))
		return nil
	}
	run, err := o.newState(trigger)
	if err != nil {
		return err
	}
	report.syncState(&run)
	o.info(ctx, "funding run started",
		slog.String("event", "funding_run_started"), slog.String("run_id", run.RunID),
		slog.String("trigger", string(trigger)), slog.Int("record_count", len(records)),
		slog.Int("builder_count", len(o.builders)))

	input := ManifestInput{Records: records, Settlement: o.settlement, Recipient: o.recipient}
	manifest, validationErr := buildManifest(input, false)
	run.Manifest = manifest
	if validationErr != nil {
		report.Outcome = "failed_validation"
		report.fail("snapshot", "资金记录校验失败。", "请修复数据库中的异常 funding record 后再启动服务。")
		if archiveErr := o.store.Archive(ctx, run, "failed_validation"); archiveErr != nil {
			return &FatalError{Err: fmt.Errorf("validate records: %w; archive failure: %v", validationErr, archiveErr)}
		}
		o.alertOnce(
			ctx, run.RunID, "validation_failed",
			AlertSeverityCritical, "funding snapshot validation failed",
		)
		return &FatalError{Err: validationErr}
	}
	payout := run.Manifest.PayoutTotal
	if payout != "0" {
		token, tokenErr := o.chain.CanonicalUSDC(ctx)
		if tokenErr != nil {
			report.fail("snapshot", "Canonical USDC metadata 查询失败。", "")
			return tokenErr
		}
		run.Manifest.Token = &token
	}
	o.info(ctx, "funding snapshot calculated",
		slog.String("event", "funding_snapshot_calculated"),
		slog.String("run_id", run.RunID), slog.Int("record_count", len(records)),
		slog.String("raw_total", run.Manifest.RawTotal),
		slog.String("payout_total", run.Manifest.PayoutTotal))
	report.Outcome = "completed"
	report.syncState(&run)
	report.setStage("snapshot", ReportStepSuccess, fmt.Sprintf("%d 条记录，Payout %s USDC", len(records), run.Manifest.PayoutTotal))
	if err := o.save(ctx, &run, PhasePrepared); err != nil {
		report.fail("snapshot", "资金快照持久化失败。", "请检查本地 data 目录和文件系统状态。")
		return err
	}
	report.syncState(&run)
	if payout == "0" {
		report.setStage("rewards", ReportStepSkipped, "Payout 为 0")
		report.setStage("sweep", ReportStepSkipped, "Payout 为 0")
		report.setStage("payout", ReportStepSkipped, "无需付款")
		report.Payout.Status = ReportStepSkipped
		return o.completeDatabase(ctx, &run, report)
	}
	return o.runPositive(ctx, &run, report)
}

func (o *Orchestrator) runPositive(ctx context.Context, state *RunState, report *RunReport) error {
	token := *state.Manifest.Token
	for _, builder := range o.builders {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimable, err := o.chain.ClaimableUSDC(ctx, builder, token)
		if err != nil {
			o.builderFailure(ctx, state, report, builder, "claimRewards", "query")
			continue
		}
		threshold := decimal.NewFromInt(claimRewardThresholdUSDC)
		eligible := claimable.GreaterThan(threshold)
		if builderReport := report.builder(builder); builderReport != nil {
			builderReport.ClaimableUSDC = claimable.String()
			if !eligible {
				builderReport.ClaimStatus = ReportStepSkipped
				builderReport.ClaimSummary = "未超过 1 USDC 阈值"
			}
		}
		o.info(ctx, "builder claim eligibility checked",
			slog.String("event", "funding_builder_claim_eligibility_checked"),
			slog.String("run_id", state.RunID), slog.String("builder", builder),
			slog.String("claimable_usdc", claimable.String()),
			slog.String("threshold_usdc", threshold.String()),
			slog.Bool("eligible", eligible))
		if !eligible {
			continue
		}
		prepared, err := o.chain.PrepareClaim(builder, o.nonce.Next())
		if err != nil {
			o.builderFailure(ctx, state, report, builder, "claimRewards", "prepare")
			continue
		}
		result, _ := o.chain.Submit(ctx, prepared)
		o.logBuilderSubmitResult(ctx, state, builder, "claimRewards", "", result)
		if result.Accepted {
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimStatus = ReportStepSuccess
				builderReport.ClaimSummary = "Reward 已领取"
			}
		} else {
			o.builderFailure(ctx, state, report, builder, "claimRewards", submitResultName(result))
		}
	}
	report.setStage("rewards", aggregateBuilderStep(report.Builders, true), builderStepSummary(report.Builders, true))
	payout, err := decimalFromString(state.Manifest.PayoutTotal)
	if err != nil {
		report.fail("snapshot", "持久化的 Payout 金额无法解析。", "请保留 current 状态并检查数据完整性。")
		return err
	}
	var settlement info.SpotBalanceAmounts
	for attempt := 1; attempt <= builderConvergenceAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, builder := range o.builders {
			if err := ctx.Err(); err != nil {
				return err
			}
			balance, balanceErr := o.chain.SpotBalance(ctx, builder, token)
			if balanceErr != nil {
				o.builderFailure(ctx, state, report, builder, "spotBalance", "query")
				continue
			}
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.FinalTotal = balance.Total.String()
				builderReport.FinalAvailable = balance.Available.String()
				if !balance.Available.IsPositive() && builderReport.SweepStatus == ReportStepPending {
					builderReport.SweepStatus = ReportStepSkipped
					builderReport.SweepSummary = "无可归集余额"
				}
			}
			o.info(ctx, "builder balance observed",
				slog.String("event", "funding_builder_balance_observed"),
				slog.String("run_id", state.RunID), slog.String("builder", builder),
				slog.Int("attempt", attempt), slog.String("total", balance.Total.String()),
				slog.String("hold", balance.Total.Sub(balance.Available).String()),
				slog.String("available", balance.Available.String()))
			if !balance.Available.IsPositive() {
				continue
			}
			prepared, prepareErr := o.chain.PrepareSpotSend(builder, state.Manifest.Settlement, token, balance.Available, o.nonce.Next())
			if prepareErr != nil {
				o.builderFailure(ctx, state, report, builder, "spotSend", "prepare")
				continue
			}
			result, _ := o.chain.Submit(ctx, prepared)
			o.logBuilderSubmitResult(ctx, state, builder, "spotSend", balance.Available.String(), result)
			if result.Accepted {
				if builderReport := report.builder(builder); builderReport != nil {
					report.addSweepAmount(builderReport, balance.Available.String())
					builderReport.SweepStatus = ReportStepSuccess
					builderReport.SweepSummary = "资金已归集"
				}
			} else {
				o.builderFailure(ctx, state, report, builder, "spotSend", submitResultName(result))
			}
		}
		settlement, err = o.chain.SpotBalance(ctx, state.Manifest.Settlement, token)
		if err == nil {
			sufficient := !settlement.Available.LessThan(payout)
			o.info(ctx, "settlement balance observed",
				slog.String("event", "funding_settlement_balance_observed"),
				slog.String("run_id", state.RunID), slog.Int("attempt", attempt),
				slog.String("total", settlement.Total.String()),
				slog.String("hold", settlement.Total.Sub(settlement.Available).String()),
				slog.String("available", settlement.Available.String()),
				slog.String("payout_total", payout.String()), slog.Bool("sufficient", sufficient))
			if sufficient {
				report.setStage("sweep", aggregateBuilderStep(report.Builders, false), builderStepSummary(report.Builders, false))
				return o.executePayout(ctx, state, settlement.Total, payout, report)
			}
		} else {
			o.warn(ctx, "settlement balance query failed",
				slog.String("event", "funding_settlement_balance_query_failed"),
				slog.String("run_id", state.RunID), slog.Int("attempt", attempt))
		}
		if attempt < builderConvergenceAttempts {
			if sleepErr := o.sleeper.Sleep(ctx, builderConvergenceInterval); sleepErr != nil {
				return sleepErr
			}
		}
	}
	if err != nil {
		report.fail(
			"sweep",
			"Settlement 余额查询失败，无法确认是否满足 Payout。",
			"",
		)
		o.alertOnce(
			ctx, state.RunID, "settlement_balance_query_failed",
			AlertSeverityWarning, "settlement spot balance query failed",
		)
		return fmt.Errorf("query settlement spot balance after %d attempts: %w", builderConvergenceAttempts, err)
	}
	o.alertOnce(
		ctx, state.RunID, "settlement_underfunded",
		AlertSeverityWarning, "settlement available balance is below required payout",
	)
	report.fail(
		"sweep",
		fmt.Sprintf("Settlement 可用余额 %s USDC，低于所需的 %s USDC。", settlement.Available, payout),
		"",
	)
	return fmt.Errorf("settlement available balance %s is below payout %s after %d attempts", settlement.Available, payout, builderConvergenceAttempts)
}

func submitResultName(result exchange.SubmitResult) string {
	if result.Rejected {
		return "rejected"
	}
	return "unknown"
}

func (o *Orchestrator) logBuilderSubmitResult(
	ctx context.Context,
	state *RunState,
	builder, action, amount string,
	result exchange.SubmitResult,
) {
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_submit_result"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.String("action_kind", action), slog.Bool("accepted", result.Accepted),
		slog.Bool("rejected", result.Rejected),
	}
	if amount != "" {
		attrs = append(attrs, slog.String("amount", amount))
	}
	if len(result.Response) != 0 {
		attrs = append(attrs, slog.String("response", string(result.Response)))
	}
	o.info(ctx, "builder submission returned", attrs...)
}

func (o *Orchestrator) builderFailure(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	builder, action, outcome string,
) {
	o.warn(ctx, "builder action did not complete",
		slog.String("event", "funding_builder_action_failed"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.String("action_kind", action),
		slog.String("outcome", outcome))
	if builderReport := report.builder(builder); builderReport != nil {
		switch action {
		case "claimRewards":
			builderReport.ClaimStatus = ReportStepWarning
			builderReport.ClaimSummary = builderActionSummary(action, outcome)
		default:
			builderReport.SweepStatus = ReportStepWarning
			builderReport.SweepSummary = builderActionSummary(action, outcome)
		}
		report.addWarning(builderReport.Name + "：" + builderActionSummary(action, outcome))
	}
	o.alertOnce(
		ctx, state.RunID, "builder_"+action+"_failed",
		AlertSeverityWarning, action+" action did not complete",
	)
}

func (o *Orchestrator) newState(trigger Trigger) (RunState, error) {
	runID, err := NewRunID()
	if err != nil {
		return RunState{}, err
	}
	now := o.clock.Now().UTC()
	return RunState{RunID: runID, Trigger: trigger, UTCDate: now.Format(time.DateOnly), CreatedAt: now, UpdatedAt: now}, nil
}

func (o *Orchestrator) save(ctx context.Context, state *RunState, phase Phase) error {
	state.Phase = phase
	state.UpdatedAt = o.clock.Now().UTC()
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.info(ctx, "funding phase persisted",
		slog.String("event", "funding_phase_persisted"), slog.String("run_id", state.RunID),
		slog.String("phase", string(phase)))
	return nil
}

func (o *Orchestrator) completeDatabase(ctx context.Context, state *RunState, report *RunReport) error {
	ids := make([]uint64, len(state.Manifest.Records))
	for i, record := range state.Manifest.Records {
		ids[i] = record.ID
	}
	o.info(ctx, "funding database completion started",
		slog.String("event", "funding_database_completion_started"),
		slog.String("run_id", state.RunID), slog.Int("record_count", len(ids)))
	if err := o.repository.Complete(ctx, ids); err != nil {
		o.alertOnce(
			ctx, state.RunID, "database_completion_failed",
			AlertSeverityWarning, "funding database completion failed",
		)
		report.fail(
			"database",
			"MySQL funding records 未能标记完成。",
			"Payout 状态是安全的；服务会持续重试数据库操作，请检查 MySQL 可用性。",
		)
		return err
	}
	if err := o.store.Archive(ctx, *state, "completed"); err != nil {
		report.fail("database", "运行历史归档失败。", "请检查 data/history 目录和文件系统状态。")
		return err
	}
	if err := o.store.Clear(ctx); err != nil {
		report.fail("database", "已完成运行的 current 状态清理失败。", "请检查 data 目录并保留现场。")
		return err
	}
	report.setStage("database", ReportStepSuccess, fmt.Sprintf("%d 条记录已完成", len(ids)))
	report.syncState(state)
	o.info(ctx, "funding run completed",
		slog.String("event", "funding_run_completed"), slog.String("run_id", state.RunID),
		slog.Int("record_count", len(ids)), slog.String("raw_total", state.Manifest.RawTotal),
		slog.String("payout_total", state.Manifest.PayoutTotal))
	o.forgetAlerts(state.RunID)
	return nil
}

func (o *Orchestrator) alert(ctx context.Context, key string, severity AlertSeverity, message string) {
	if o.notifier != nil {
		o.notifier.Alert(ctx, key, severity, message)
	}
}

func (o *Orchestrator) alertOnce(
	ctx context.Context,
	runID, key string,
	severity AlertSeverity,
	message string,
) {
	dedupeKey := runID + ":" + key
	o.alertMu.Lock()
	if _, exists := o.alerts[dedupeKey]; exists {
		o.alertMu.Unlock()
		return
	}
	o.alerts[dedupeKey] = struct{}{}
	o.alertMu.Unlock()
	o.alert(ctx, dedupeKey, severity, message)
}

func (o *Orchestrator) forgetAlerts(runID string) {
	o.alertMu.Lock()
	defer o.alertMu.Unlock()
	for key := range o.alerts {
		if strings.HasPrefix(key, runID+":") {
			delete(o.alerts, key)
		}
	}
}

func (o *Orchestrator) info(ctx context.Context, message string, attrs ...slog.Attr) {
	if o.logger != nil {
		o.logger.Info(ctx, message, attrs...)
	}
}
func (o *Orchestrator) warn(ctx context.Context, message string, attrs ...slog.Attr) {
	if o.logger != nil {
		o.logger.Warn(ctx, message, attrs...)
	}
}
func (o *Orchestrator) error(ctx context.Context, message string, attrs ...slog.Attr) {
	if o.logger != nil {
		o.logger.Error(ctx, message, attrs...)
	}
}

func (o *Orchestrator) loadCurrent(ctx context.Context) (*RunState, StateLoadMetadata, error) {
	current, metadata, err := o.store.LoadWithMetadata(ctx)
	if err == nil {
		attrs := []slog.Attr{
			slog.String("event", "funding_current_state_checked"),
			slog.Bool("current_exists", current != nil),
			slog.Bool("recovered_from_backup", metadata.RecoveredFromBackup),
		}
		if current != nil {
			attrs = append(attrs,
				slog.String("run_id", current.RunID),
				slog.String("phase", string(current.Phase)))
		}
		o.info(ctx, "funding current state checked", attrs...)
	}
	if err == nil && metadata.RecoveredFromBackup {
		runID := "state-store"
		if current != nil && current.RunID != "" {
			runID = current.RunID
		}
		o.warn(ctx, "funding state recovered from backup",
			slog.String("event", "state_snapshot_recovered_from_backup"),
			slog.String("run_id", runID), slog.Bool("primary_invalid", metadata.PrimaryInvalid))
		o.alertOnce(
			ctx, runID, "state_corruption",
			AlertSeverityWarning, "funding state recovered from backup",
		)
	}
	return current, metadata, err
}

func decimalFromString(value string) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse persisted payout total: %w", err)
	}
	return parsed, nil
}

func clonePrepared(action exchange.PreparedAction) exchange.PreparedAction {
	action.RequestBody = append([]byte(nil), action.RequestBody...)
	return action
}
