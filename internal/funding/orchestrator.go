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
	Repository          Repository
	Store               StateStore
	DailyConsolidations DailyConsolidationStore
	Chain               Chain
	Notifier            Notifier
	Logger              Logger
	Builders            []string
	BuilderNames        map[string]string
	Settlement          string
	Recipient           string
	Network             string
	Clock               Clock
	Nonce               NonceSource
	Sleeper             Sleeper
}

type Orchestrator struct {
	repository          Repository
	store               StateStore
	dailyConsolidations DailyConsolidationStore
	chain               Chain
	notifier            Notifier
	logger              Logger
	builders            []string
	builderNames        map[string]string
	settlement          string
	recipient           string
	network             string
	clock               Clock
	nonce               NonceSource
	sleeper             Sleeper
	alertMu             sync.Mutex
	alerts              map[string]struct{}
	lowLimits           map[string]struct{}
}

const (
	claimableQueryAttempts          = 5
	claimableQueryRetryInterval     = 200 * time.Millisecond
	claimSubmitAttempts             = 3
	claimSubmitRetryInterval        = 250 * time.Millisecond
	sweepSubmitAttempts             = 3
	sweepSubmitRetryInterval        = 250 * time.Millisecond
	claimBalanceObservationAttempts = 10
	claimBalanceObservationInterval = time.Second
	claimRewardThresholdUSDC        = int64(1)
	zeroEthereumAddress             = "0x0000000000000000000000000000000000000000"
)

func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Repository == nil || cfg.Store == nil || cfg.DailyConsolidations == nil ||
		cfg.Chain == nil || cfg.Clock == nil || cfg.Nonce == nil {
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
		repository: cfg.Repository, store: cfg.Store,
		dailyConsolidations: cfg.DailyConsolidations, chain: cfg.Chain,
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
		for _, key := range []string{"payout", "database"} {
			report.setStage(key, ReportStepSkipped, "无需执行")
		}
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
		report.setStage("payout", ReportStepSkipped, "无需付款")
		report.Payout.Status = ReportStepSkipped
		return o.completeDatabase(ctx, &run, report)
	}
	return o.runPositive(ctx, &run, report)
}

func (o *Orchestrator) runPositive(ctx context.Context, state *RunState, report *RunReport) error {
	token := *state.Manifest.Token
	payout, err := decimalFromString(state.Manifest.PayoutTotal)
	if err != nil {
		report.fail("snapshot", "持久化的 Payout 金额无法解析。", "请保留 current 状态并检查数据完整性。")
		return err
	}
	settlement, err := o.chain.SpotBalance(ctx, state.Manifest.Settlement, token)
	if err != nil {
		report.fail("payout", "Settlement 余额查询失败，无法确认是否满足 Payout。", "")
		o.alertOnce(
			ctx, state.RunID, "settlement_balance_query_failed",
			AlertSeverityWarning, "settlement spot balance query failed",
		)
		return fmt.Errorf("query settlement spot balance: %w", err)
	}
	sufficient := !settlement.Available.LessThan(payout)
	o.info(ctx, "settlement balance observed",
		slog.String("event", "funding_settlement_balance_observed"),
		slog.String("run_id", state.RunID),
		slog.String("total", settlement.Total.String()),
		slog.String("hold", settlement.Total.Sub(settlement.Available).String()),
		slog.String("available", settlement.Available.String()),
		slog.String("payout_total", payout.String()), slog.Bool("sufficient", sufficient))
	if sufficient {
		return o.executePayout(ctx, state, settlement.Total, payout, report)
	}
	o.alertOnce(
		ctx, state.RunID, "settlement_underfunded",
		AlertSeverityWarning, "settlement available balance is below required payout",
	)
	report.fail(
		"payout",
		fmt.Sprintf("Settlement 可用余额 %s USDC，低于所需的 %s USDC。", settlement.Available, payout),
		"",
	)
	return fmt.Errorf("settlement available balance %s is below payout %s", settlement.Available, payout)
}

func (o *Orchestrator) queryConsolidationToken(
	ctx context.Context,
	state *RunState,
) (info.Token, error) {
	var lastErr error
	for attempt := 1; attempt <= claimableQueryAttempts; attempt++ {
		token, err := o.chain.CanonicalUSDC(ctx)
		if err == nil {
			return token, nil
		}
		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return info.Token{}, ctxErr
		}
		willRetry := attempt < claimableQueryAttempts
		o.warn(ctx, "canonical USDC query for consolidation failed",
			slog.String("event", "funding_consolidation_token_query_failed"),
			slog.String("run_id", state.RunID),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", claimableQueryAttempts),
			slog.Bool("will_retry", willRetry))
		if willRetry {
			if err := o.sleeper.Sleep(ctx, claimableQueryRetryInterval); err != nil {
				return info.Token{}, err
			}
		}
	}
	return info.Token{}, fmt.Errorf(
		"query canonical USDC after %d attempts: %w",
		claimableQueryAttempts, lastErr,
	)
}

type claimableQueryResult struct {
	index  int
	amount decimal.Decimal
	err    error
}

func (o *Orchestrator) captureBuilderClaimables(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
) (map[string]decimal.Decimal, error) {
	claimables := make(map[string]decimal.Decimal, len(o.builders))
	pending := make([]bool, len(o.builders))
	for index := range pending {
		pending[index] = true
	}

	for attempt := 1; attempt <= claimableQueryAttempts; attempt++ {
		indexes := pendingBuilderIndexes(pending)
		results := make(chan claimableQueryResult, len(indexes))
		for _, index := range indexes {
			index := index
			go func() {
				amount, err := o.chain.ClaimableUSDC(ctx, o.builders[index], token)
				if err == nil && amount.IsNegative() {
					err = fmt.Errorf("claimable amount is negative")
				}
				results <- claimableQueryResult{index: index, amount: amount, err: err}
			}()
		}

		round := make([]claimableQueryResult, len(o.builders))
		for range indexes {
			result := <-results
			round[result.index] = result
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, index := range indexes {
			builder := o.builders[index]
			result := round[index]
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimableQueryAttempts = attempt
			}
			if result.err == nil {
				pending[index] = false
				claimables[builder] = result.amount
				if builderReport := report.builder(builder); builderReport != nil {
					builderReport.ClaimableUSDC = result.amount.String()
					builderReport.ClaimableObserved = true
				}
				o.info(ctx, "builder claimable captured",
					slog.String("event", "funding_builder_claimable_captured"),
					slog.String("run_id", state.RunID),
					slog.String("builder", builder),
					slog.Int("attempt", attempt),
					slog.String("claimable_usdc", result.amount.String()))
				continue
			}
			o.warn(ctx, "builder claimable query attempt failed",
				slog.String("event", "funding_builder_claimable_query_failed"),
				slog.String("run_id", state.RunID),
				slog.String("builder", builder),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", claimableQueryAttempts),
				slog.Bool("will_retry", attempt < claimableQueryAttempts))
		}
		if len(pendingBuilderIndexes(pending)) == 0 {
			break
		}
		if attempt < claimableQueryAttempts {
			if err := o.sleeper.Sleep(ctx, claimableQueryRetryInterval); err != nil {
				return nil, err
			}
		}
	}

	for index, stillPending := range pending {
		if stillPending {
			o.builderFailure(ctx, state, report, o.builders[index], "claimable", "query")
		}
	}
	return claimables, nil
}

func pendingBuilderIndexes(pending []bool) []int {
	indexes := make([]int, 0, len(pending))
	for index, isPending := range pending {
		if isPending {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

type claimAttemptResult struct {
	index      int
	prepareErr error
	result     exchange.SubmitResult
}

type claimRetryState struct {
	balanceTarget  decimal.Decimal
	claimableKnown bool
	hadUnknown     bool
}

func (o *Orchestrator) claimBuilderRewards(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	claimables map[string]decimal.Decimal,
) (map[string]decimal.Decimal, error) {
	claimBalanceTargets := make(map[string]decimal.Decimal, len(o.builders))
	threshold := decimal.NewFromInt(claimRewardThresholdUSDC)
	pending := make([]bool, len(o.builders))
	states := make([]claimRetryState, len(o.builders))
	for index, builder := range o.builders {
		claimable, observed := claimables[builder]
		eligible := !observed || claimable.GreaterThan(threshold)
		if !eligible {
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimStatus = ReportStepSkipped
				builderReport.ClaimSummary = "未超过 1 USDC 阈值"
			}
		} else {
			pending[index] = true
			states[index].balanceTarget = claimable
			states[index].claimableKnown = observed
			if !observed {
				states[index].balanceTarget = threshold
			}
		}
		o.info(ctx, "builder claim eligibility checked",
			slog.String("event", "funding_builder_claim_eligibility_checked"),
			slog.String("run_id", state.RunID), slog.String("builder", builder),
			slog.String("claimable_usdc", claimable.String()),
			slog.String("threshold_usdc", threshold.String()),
			slog.Bool("claimable_observed", observed),
			slog.Bool("eligible", eligible))
	}

	for attempt := 1; attempt <= claimSubmitAttempts; attempt++ {
		indexes := pendingBuilderIndexes(pending)
		if len(indexes) == 0 {
			break
		}
		nonces := make(map[int]uint64, len(indexes))
		for _, index := range indexes {
			nonces[index] = o.nonce.Next()
		}
		results := make(chan claimAttemptResult, len(indexes))
		for _, index := range indexes {
			index := index
			go func() {
				prepared, err := o.chain.PrepareClaim(o.builders[index], nonces[index])
				if err != nil {
					results <- claimAttemptResult{index: index, prepareErr: err}
					return
				}
				result, _ := o.chain.Submit(ctx, prepared)
				results <- claimAttemptResult{index: index, result: result}
			}()
		}

		round := make([]claimAttemptResult, len(o.builders))
		for range indexes {
			result := <-results
			round[result.index] = result
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, index := range indexes {
			builder := o.builders[index]
			retry := &states[index]
			result := round[index]
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimAttempts = attempt
			}
			if result.prepareErr != nil {
				if retry.hadUnknown {
					if attempt == claimSubmitAttempts {
						pending[index] = false
						claimBalanceTargets[builder] = retry.balanceTarget
						o.markClaimUncertain(ctx, state, report, builder)
					} else {
						o.logClaimRetry(ctx, state, builder, attempt)
					}
				} else {
					pending[index] = false
					o.builderFailure(ctx, state, report, builder, "claimRewards", "prepare")
				}
				continue
			}
			o.logBuilderSubmitResult(ctx, state, builder, "claimRewards", "", result.result)
			switch {
			case result.result.Accepted:
				pending[index] = false
				claimBalanceTargets[builder] = retry.balanceTarget
				if builderReport := report.builder(builder); builderReport != nil {
					builderReport.ClaimStatus = ReportStepSuccess
					builderReport.ClaimSummary = "Reward 已领取"
					if !retry.claimableKnown {
						builderReport.ClaimSummary = "Reward 已领取；零点 Claimable 数据缺失"
					}
					if attempt > 1 {
						builderReport.ClaimSummary = fmt.Sprintf("Reward 已领取（第 %d 次尝试）", attempt)
						if !retry.claimableKnown {
							builderReport.ClaimSummary += "；零点 Claimable 数据缺失"
						}
					}
				}
			case result.result.Rejected:
				if retry.hadUnknown {
					if attempt == claimSubmitAttempts {
						pending[index] = false
						claimBalanceTargets[builder] = retry.balanceTarget
						o.markClaimUncertain(ctx, state, report, builder)
					} else {
						o.logClaimRetry(ctx, state, builder, attempt)
					}
				} else {
					pending[index] = false
					o.builderFailure(ctx, state, report, builder, "claimRewards", "rejected")
				}
			default:
				retry.hadUnknown = true
				if attempt == claimSubmitAttempts {
					pending[index] = false
					claimBalanceTargets[builder] = retry.balanceTarget
					o.markClaimUncertain(ctx, state, report, builder)
					continue
				}
				o.logClaimRetry(ctx, state, builder, attempt)
			}
		}
		if len(pendingBuilderIndexes(pending)) > 0 && attempt < claimSubmitAttempts {
			if err := o.sleeper.Sleep(ctx, claimSubmitRetryInterval); err != nil {
				return nil, err
			}
		}
	}
	report.setStage("rewards", aggregateBuilderStep(report.Builders, true), builderStepSummary(report.Builders, true))
	return claimBalanceTargets, nil
}

func (o *Orchestrator) logClaimRetry(
	ctx context.Context,
	state *RunState,
	builder string,
	attempt int,
) {
	o.warn(ctx, "builder claim remains uncertain; retrying",
		slog.String("event", "funding_builder_claim_retry_planned"),
		slog.String("run_id", state.RunID),
		slog.String("builder", builder),
		slog.Int("attempt", attempt),
		slog.Int("max_attempts", claimSubmitAttempts))
}

func (o *Orchestrator) markClaimUncertain(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	builder string,
) {
	if builderReport := report.builder(builder); builderReport != nil {
		builderReport.ClaimUncertain = true
	}
	o.builderFailure(ctx, state, report, builder, "claimRewards", "unknown")
}

type sweepRetryState struct {
	done       bool
	hadUnknown bool
	lastAmount string
}

func (o *Orchestrator) sweepBuilderBalances(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	initialBalances map[string]info.SpotBalanceAmounts,
	claimBalanceTargets map[string]decimal.Decimal,
	unconvergedClaims map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	states := make([]sweepRetryState, len(o.builders))
	for attempt := 1; attempt <= sweepSubmitAttempts; attempt++ {
		pending := false
		for index, builder := range o.builders {
			if err := ctx.Err(); err != nil {
				return err
			}
			retry := &states[index]
			if retry.done {
				continue
			}
			balance, balanceErr, skip := o.loadSweepBalance(
				ctx, state, report, token, builder, attempt, initialBalances, claimBalanceTargets,
			)
			if skip {
				retry.done = true
				continue
			}
			if balanceErr != nil {
				if attempt == sweepSubmitAttempts {
					retry.done = true
					o.builderFailure(ctx, state, report, builder, "spotBalance", "query")
				} else {
					pending = true
					o.warn(ctx, "builder sweep balance query failed; retrying",
						slog.String("event", "funding_builder_sweep_retry_planned"),
						slog.String("run_id", state.RunID),
						slog.String("builder", builder),
						slog.String("reason", "spot_balance_query"),
						slog.Int("attempt", attempt),
						slog.Int("max_attempts", sweepSubmitAttempts))
				}
				continue
			}
			if !balance.Available.IsPositive() {
				if retry.hadUnknown {
					o.markSweepSucceeded(
						report, builder, retry.lastAmount, unconvergedClaims,
						"资金已归集（余额已清空；此前提交结果不确定）",
					)
				}
				retry.done = true
				continue
			}
			prepared, prepareErr := o.chain.PrepareSpotSend(
				builder, state.Manifest.Settlement, token, balance.Available, o.nonce.Next(),
			)
			if prepareErr != nil {
				if retry.hadUnknown {
					if attempt == sweepSubmitAttempts {
						retry.done = true
						o.builderFailure(ctx, state, report, builder, "spotSend", "unknown")
					} else {
						pending = true
						o.logSweepRetry(ctx, state, builder, attempt)
					}
				} else {
					retry.done = true
					o.builderFailure(ctx, state, report, builder, "spotSend", "prepare")
				}
				continue
			}
			amount := balance.Available.String()
			result, _ := o.chain.Submit(ctx, prepared)
			o.logBuilderSubmitResult(ctx, state, builder, "spotSend", amount, result)
			switch {
			case result.Accepted:
				retry.done = true
				summary := "资金已归集"
				if attempt > 1 {
					summary = fmt.Sprintf("资金已归集（第 %d 次尝试）", attempt)
				}
				o.markSweepSucceeded(report, builder, amount, unconvergedClaims, summary)
			case result.Rejected && !retry.hadUnknown:
				retry.done = true
				o.builderFailure(ctx, state, report, builder, "spotSend", "rejected")
			default:
				retry.hadUnknown = true
				retry.lastAmount = amount
				if attempt == sweepSubmitAttempts {
					retry.done = true
					o.builderFailure(ctx, state, report, builder, "spotSend", "unknown")
					continue
				}
				pending = true
				o.logSweepRetry(ctx, state, builder, attempt)
			}
		}
		if !pending {
			break
		}
		if err := o.sleeper.Sleep(ctx, sweepSubmitRetryInterval); err != nil {
			return err
		}
	}
	return nil
}

func (o *Orchestrator) loadSweepBalance(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	builder string,
	attempt int,
	initialBalances map[string]info.SpotBalanceAmounts,
	claimBalanceTargets map[string]decimal.Decimal,
) (info.SpotBalanceAmounts, error, bool) {
	if attempt == 1 {
		if balance, observed := initialBalances[builder]; observed {
			return balance, nil, false
		}
		if _, alreadyPolled := claimBalanceTargets[builder]; alreadyPolled {
			return info.SpotBalanceAmounts{}, nil, true
		}
	}
	balance, err := o.chain.SpotBalance(ctx, builder, token)
	if err != nil {
		return info.SpotBalanceAmounts{}, err, false
	}
	o.recordBuilderBalance(ctx, state, report, builder, attempt, balance, nil)
	return balance, nil, false
}

func (o *Orchestrator) markSweepSucceeded(
	report *RunReport,
	builder, amount string,
	unconvergedClaims map[string]struct{},
	summary string,
) {
	builderReport := report.builder(builder)
	if builderReport == nil {
		return
	}
	report.addSweepAmount(builderReport, amount)
	if _, unconverged := unconvergedClaims[builder]; unconverged {
		builderReport.SweepStatus = ReportStepWarning
		builderReport.SweepSummary = "已按最新可用余额归集，Reward 余额未确认刷新"
		return
	}
	builderReport.SweepStatus = ReportStepSuccess
	builderReport.SweepSummary = summary
}

func (o *Orchestrator) logSweepRetry(
	ctx context.Context,
	state *RunState,
	builder string,
	attempt int,
) {
	o.warn(ctx, "builder sweep remains uncertain; retrying",
		slog.String("event", "funding_builder_sweep_retry_planned"),
		slog.String("run_id", state.RunID),
		slog.String("builder", builder),
		slog.Int("attempt", attempt),
		slog.Int("max_attempts", sweepSubmitAttempts))
}

func (o *Orchestrator) observeClaimBalances(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	targets map[string]decimal.Decimal,
) (map[string]info.SpotBalanceAmounts, map[string]struct{}, error) {
	balances := make(map[string]info.SpotBalanceAmounts, len(targets))
	if len(targets) == 0 {
		return balances, nil, nil
	}
	pending := make(map[string]decimal.Decimal, len(targets))
	for builder, claimable := range targets {
		pending[builder] = claimable
	}
	o.info(ctx, "builder claim balance observation waiting",
		slog.String("event", "funding_builder_claim_balance_wait_started"),
		slog.String("run_id", state.RunID),
		slog.Int("builder_count", len(pending)),
		slog.Duration("wait", claimBalanceObservationInterval))
	if err := o.sleeper.Sleep(ctx, claimBalanceObservationInterval); err != nil {
		return nil, nil, err
	}
	for attempt := 1; attempt <= claimBalanceObservationAttempts; attempt++ {
		for _, builder := range o.builders {
			claimable, waiting := pending[builder]
			if !waiting {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			balance, err := o.chain.SpotBalance(ctx, builder, token)
			if err != nil {
				o.builderFailure(ctx, state, report, builder, "spotBalance", "query")
				continue
			}
			balances[builder] = balance
			o.recordBuilderBalance(ctx, state, report, builder, attempt, balance, &claimable)
			if !balance.Available.LessThan(claimable) {
				delete(pending, builder)
			}
		}
		if len(pending) == 0 {
			return balances, nil, nil
		}
		if attempt < claimBalanceObservationAttempts {
			if err := o.sleeper.Sleep(ctx, claimBalanceObservationInterval); err != nil {
				return nil, nil, err
			}
		}
	}
	unconverged := make(map[string]struct{}, len(pending))
	for _, builder := range o.builders {
		claimable, waiting := pending[builder]
		if !waiting {
			continue
		}
		unconverged[builder] = struct{}{}
		balance, observed := balances[builder]
		o.claimBalanceObservationExhausted(ctx, state, report, builder, claimable, balance, observed)
	}
	return balances, unconverged, nil
}

func (o *Orchestrator) recordBuilderBalance(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	builder string,
	attempt int,
	balance info.SpotBalanceAmounts,
	claimable *decimal.Decimal,
) {
	if builderReport := report.builder(builder); builderReport != nil {
		builderReport.FinalTotal = balance.Total.String()
		builderReport.FinalAvailable = balance.Available.String()
		if !balance.Available.IsPositive() && builderReport.SweepStatus == ReportStepPending {
			builderReport.SweepStatus = ReportStepSkipped
			builderReport.SweepSummary = "无可归集余额"
		}
	}
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_balance_observed"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.Int("attempt", attempt), slog.String("total", balance.Total.String()),
		slog.String("hold", balance.Total.Sub(balance.Available).String()),
		slog.String("available", balance.Available.String()),
	}
	if claimable != nil {
		attrs = append(attrs,
			slog.String("claimable_usdc", claimable.String()),
			slog.Bool("claim_balance_visible", !balance.Available.LessThan(*claimable)),
		)
	}
	o.info(ctx, "builder balance observed", attrs...)
}

func (o *Orchestrator) claimBalanceObservationExhausted(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	builder string,
	claimable decimal.Decimal,
	balance info.SpotBalanceAmounts,
	observed bool,
) {
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_claim_balance_observation_exhausted"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.Int("attempt_count", claimBalanceObservationAttempts),
		slog.String("claimable_usdc", claimable.String()),
		slog.Bool("balance_observed", observed),
	}
	if observed {
		attrs = append(attrs,
			slog.String("last_total", balance.Total.String()),
			slog.String("last_available", balance.Available.String()),
		)
	}
	o.warn(ctx, "builder claim balance did not become visible", attrs...)
	summary := fmt.Sprintf("Reward 余额未在 %d 次查询内确认刷新", claimBalanceObservationAttempts)
	if builderReport := report.builder(builder); builderReport != nil {
		builderReport.SweepStatus = ReportStepWarning
		builderReport.SweepSummary = summary
		report.addWarning(builderReport.Name + ": " + summary)
	}
	o.alertOnce(
		ctx, state.RunID, "builder_claim_balance_not_visible:"+strings.ToLower(builder),
		AlertSeverityWarning,
		fmt.Sprintf(
			"builder claim balance did not become visible\nbuilder: %s\nclaimable USDC: %s\nattempts: %d",
			builder, claimable, claimBalanceObservationAttempts,
		),
	)
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
	if !result.Accepted && len(result.Response) != 0 {
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
		case "claimable", "claimRewards":
			builderReport.ClaimStatus = ReportStepWarning
			builderReport.ClaimSummary = builderActionSummary(action, outcome)
		default:
			builderReport.SweepStatus = ReportStepWarning
			builderReport.SweepSummary = builderActionSummary(action, outcome)
		}
		report.addWarning(builderReport.Name + ": " + builderActionSummary(action, outcome))
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
