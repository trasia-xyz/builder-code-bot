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
	"builder-code-bot/internal/logging"

	"github.com/shopspring/decimal"
)

type OrchestratorConfig struct {
	ReferralRepository  Repository
	Store               StateStore
	DailyConsolidations DailyConsolidationStore
	Chain               Chain
	Notifier            Notifier
	Logger              Logger
	Builders            []string
	BuilderNames        map[string]string
	Settlement          string
	ReferralRecipient   string
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
	claimableQueryAttempts                 = 5
	claimableQueryRetryInterval            = 200 * time.Millisecond
	claimBaselineQueryAttempts             = 5
	claimBaselineQueryInterval             = time.Second
	claimBaselineRequiredConfirmations     = 2
	sweepSubmitAttempts                    = 3
	sweepSubmitRetryInterval               = time.Second
	sweepBalanceQueryAttempts              = 5
	sweepBalanceQueryInterval              = time.Second
	sweepBalanceRequiredConfirmations      = 2
	settlementBalanceQueryAttempts         = 5
	settlementBalanceQueryInterval         = time.Second
	settlementBalanceRequiredConfirmations = 2
	claimBalanceObservationAttempts        = 10
	claimBalanceObservationInterval        = time.Second
	claimBalanceRequiredConfirmations      = 2
	claimRewardThresholdUSDC               = int64(1)
	claimBelowThresholdRejection           = "Must have at least 1 of a reward token to claim"
	zeroEthereumAddress                    = "0x0000000000000000000000000000000000000000"
)

func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.ReferralRepository == nil ||
		cfg.Store == nil || cfg.DailyConsolidations == nil ||
		cfg.Chain == nil || cfg.Clock == nil || cfg.Nonce == nil {
		return nil, fmt.Errorf("funding orchestrator dependencies are required")
	}
	if len(cfg.Builders) == 0 || strings.TrimSpace(cfg.Settlement) == "" ||
		strings.TrimSpace(cfg.ReferralRecipient) == "" {
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
	recipient := strings.TrimSpace(cfg.ReferralRecipient)
	if strings.EqualFold(recipient, zeroEthereumAddress) {
		return nil, fmt.Errorf("referral recipient account must not be the zero address")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Settlement), recipient) {
		return nil, fmt.Errorf("settlement and referral recipient accounts must differ")
	}
	if _, controlled := seen[strings.ToLower(recipient)]; controlled {
		return nil, fmt.Errorf("referral recipient account must not be a builder")
	}
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	return &Orchestrator{
		repository:          cfg.ReferralRepository,
		store:               cfg.Store,
		dailyConsolidations: cfg.DailyConsolidations, chain: cfg.Chain,
		notifier: cfg.Notifier, logger: cfg.Logger,
		builders:     append([]string(nil), cfg.Builders...),
		builderNames: cloneBuilderNames(cfg.BuilderNames),
		settlement:   cfg.Settlement, recipient: recipient, network: cfg.Network,
		clock: cfg.Clock, nonce: cfg.Nonce, sleeper: sleeper,
		alerts:    make(map[string]struct{}),
		lowLimits: make(map[string]struct{}),
	}, nil
}

func (o *Orchestrator) runNew(ctx context.Context, trigger Trigger, report *RunReport) error {
	o.info(ctx, "pending funding records query started",
		slog.String("event", "funding_pending_records_query_started"),
		slog.String("payout_kind", string(PayoutKindReferral)))
	records, err := o.repository.ListPending(ctx)
	if err != nil {
		report.fail("snapshot", "数据库 pending funding records 查询失败。", "请检查 MySQL 可用性与查询权限。")
		return fmt.Errorf("list referral pending records: %w", err)
	}
	totalRecords := len(records)
	o.info(ctx, "pending funding records query completed",
		slog.String("event", "funding_pending_records_query_completed"),
		slog.String("payout_kind", string(PayoutKindReferral)),
		slog.Int("record_count", totalRecords))
	if totalRecords == 0 {
		report.RecordsRead = true
		report.RecordCount = 0
		report.RawTotal = "0.000000000000000000"
		report.PayoutTotal = "0"
		report.Outcome = "no_data"
		report.setStage("snapshot", ReportStepSuccess, "没有待处理记录")
		report.markTargetNoData(PayoutKindReferral)
		report.setStage("finalize", ReportStepSkipped, "无需持久化运行状态")
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
		slog.String("trigger", string(trigger)), slog.Int("record_count", totalRecords),
		slog.Int("builder_count", len(o.builders)))

	manifest, validationErr := buildManifest(ManifestInput{
		Records: records, Settlement: o.settlement, Recipient: o.recipient,
	}, false)
	run.Targets = []PayoutTargetState{
		{Kind: PayoutKindReferral, Phase: PhasePrepared, Manifest: manifest},
	}
	report.syncState(&run)
	if validationErr != nil {
		// An invalid manifest has no reliable total for claimable reconciliation.
		report.RawTotal = ""
		report.PayoutTotal = ""
		report.Outcome = "failed_validation"
		report.fail("snapshot", "资金记录校验失败。", "请修复数据库中的异常 funding record 后再启动服务。")
		if archiveErr := o.store.Archive(ctx, run, "failed_validation"); archiveErr != nil {
			report.fail("finalize", "异常 funding records 运行归档失败。", "请检查 data/history 目录并保留服务现场。")
			return &FatalError{Err: fmt.Errorf("validate records: %w; archive failure: %v", validationErr, archiveErr)}
		}
		report.setStage("finalize", ReportStepSuccess, "校验失败现场已归档")
		o.alertOnce(
			ctx, run.RunID, "validation_failed",
			AlertSeverityCritical, "funding snapshot validation failed",
		)
		return &FatalError{Err: validationErr}
	}
	payout, err := decimalFromString(manifest.PayoutTotal)
	if err != nil {
		return err
	}
	if payout.IsPositive() {
		token, tokenErr := o.chain.CanonicalUSDC(ctx)
		if tokenErr != nil {
			report.fail("snapshot", "Canonical USDC metadata 查询失败。", "")
			return tokenErr
		}
		run.Targets[0].Manifest.Token = cloneToken(&token)
	}
	o.info(ctx, "funding snapshot calculated",
		slog.String("event", "funding_snapshot_calculated"),
		slog.String("run_id", run.RunID), slog.Int("record_count", totalRecords),
		slog.String("payout_total", payout.String()))
	report.Outcome = "completed"
	report.syncState(&run)
	report.setStage("snapshot", ReportStepSuccess, fmt.Sprintf(
		"%d 条记录，Referral Payout %s USDC", totalRecords, payout,
	))
	if err := o.saveActivePhase(ctx, &run, PhasePrepared); err != nil {
		report.fail("snapshot", "资金快照持久化失败。", "请检查本地 data 目录和文件系统状态。")
		return err
	}
	report.syncState(&run)
	return o.runActiveTarget(ctx, &run, report)
}

func (o *Orchestrator) runActiveTarget(ctx context.Context, state *RunState, report *RunReport) error {
	target, _, err := state.activePayoutTarget()
	if err != nil {
		return &FatalError{Err: err}
	}
	if target == nil {
		return o.finishRun(ctx, state, report)
	}
	if len(target.Manifest.Records) == 0 {
		report.markTargetNoData(target.Kind)
		return o.completeDatabase(ctx, state, target, report)
	}
	if target.Manifest.PayoutTotal == "0" {
		report.setTargetStage(target.Kind, "payout", ReportStepSkipped, "无需付款")
		report.target(target.Kind).Payout.Status = ReportStepSkipped
		return o.completeDatabase(ctx, state, target, report)
	}
	return o.runPositive(ctx, state, target, report)
}

func (o *Orchestrator) runPositive(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
) error {
	token := *target.Manifest.Token
	payout, err := decimalFromString(target.Manifest.PayoutTotal)
	if err != nil {
		report.fail("snapshot", "持久化的 Payout 金额无法解析。", "请保留 current 状态并检查数据完整性。")
		return err
	}
	var (
		settlement             info.SpotBalanceAmounts
		candidate              info.SpotBalanceAmounts
		observed               bool
		lastQueryErr           error
		lastSufficient         bool
		consecutiveStableReads int
	)
	for attempt := 1; attempt <= settlementBalanceQueryAttempts; attempt++ {
		balance, queryErr := o.chain.SpotBalance(ctx, target.Manifest.Settlement, token)
		if queryErr == nil {
			settlement = balance
			observed = true
			sufficient := !balance.Available.LessThan(payout)
			lastSufficient = sufficient
			if consecutiveStableReads > 0 && sameSpotBalance(balance, candidate) {
				consecutiveStableReads++
			} else {
				candidate = balance
				consecutiveStableReads = 1
			}
			o.info(ctx, "settlement balance observed",
				slog.String("event", "funding_settlement_balance_observed"),
				slog.String("run_id", state.RunID),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", settlementBalanceQueryAttempts),
				slog.String("total", balance.Total.String()),
				slog.String("hold", balance.Total.Sub(balance.Available).String()),
				slog.String("available", balance.Available.String()),
				slog.String("payout_total", payout.String()),
				slog.String("required_available", payout.String()),
				slog.Bool("sufficient", sufficient),
				slog.Int("consecutive_stable_reads", consecutiveStableReads))
			if sufficient && consecutiveStableReads >= settlementBalanceRequiredConfirmations {
				return o.executePayout(ctx, state, target, balance, payout, report)
			}
		} else {
			lastQueryErr = queryErr
			consecutiveStableReads = 0
			o.warn(ctx, "settlement balance query failed; retrying before payout",
				slog.String("event", "funding_settlement_balance_query_failed"),
				slog.String("run_id", state.RunID),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", settlementBalanceQueryAttempts),
				logging.Err(queryErr))
		}
		if attempt < settlementBalanceQueryAttempts {
			if err := o.sleeper.Sleep(ctx, settlementBalanceQueryInterval); err != nil {
				return err
			}
		}
	}
	if !observed {
		report.failTarget(target.Kind, "payout", "Settlement 余额查询失败，无法确认是否满足 Payout。", "")
		o.alertOnce(
			ctx, state.RunID, "settlement_balance_query_failed",
			AlertSeverityWarning, "settlement spot balance query failed",
		)
		return fmt.Errorf("query settlement spot balance: %w", lastQueryErr)
	}
	if lastSufficient {
		o.alertOnce(
			ctx, state.RunID, "settlement_balance_unstable",
			AlertSeverityWarning, "settlement spot balance did not stabilize before payout",
		)
		report.failTarget(
			target.Kind, "payout",
			"Settlement 余额未在有限查询内稳定，未提交 Payout。",
			"请等待余额索引稳定后重试。",
		)
		return fmt.Errorf("settlement balance did not stabilize before payout")
	}
	o.alertOnce(
		ctx, state.RunID, "settlement_underfunded",
		AlertSeverityWarning, "settlement available balance is below required payout",
	)
	report.failTarget(
		target.Kind, "payout",
		fmt.Sprintf("Settlement 可用余额 %s USDC，低于所需的 %s USDC。", settlement.Available, payout),
		"",
	)
	return fmt.Errorf(
		"settlement available balance %s is below required payout total %s",
		settlement.Available, payout,
	)
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
			slog.Bool("will_retry", willRetry),
			logging.Err(err))
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
				slog.Bool("will_retry", attempt < claimableQueryAttempts),
				logging.Err(result.err))
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
	submitErr  error
	result     exchange.SubmitResult
}

type claimBalanceTarget struct {
	totalBefore             decimal.Decimal
	availableBefore         decimal.Decimal
	claimableBefore         decimal.Decimal
	claimableKnown          bool
	minimumIncrease         decimal.Decimal
	submitUnknown           bool
	preStateSeenAfterSubmit bool
}

func (t claimBalanceTarget) minimumTotal() decimal.Decimal {
	return t.totalBefore.Add(t.minimumIncrease)
}

func (t claimBalanceTarget) visible(total decimal.Decimal) bool {
	return !total.LessThan(t.minimumTotal())
}

type balanceQueryResult struct {
	index   int
	balance info.SpotBalanceAmounts
	err     error
}

func sameSpotBalance(left, right info.SpotBalanceAmounts) bool {
	return left.Total.Equal(right.Total) && left.Available.Equal(right.Available)
}

func claimEligible(claimable decimal.Decimal, observed bool) bool {
	// A missing observation falls back to one bounded claim attempt. The
	// exchange's exact below-threshold rejection is classified locally, while
	// every other response remains a normal failure or uncertain submission.
	return !observed || !claimable.LessThan(decimal.NewFromInt(claimRewardThresholdUSDC))
}

func (o *Orchestrator) captureClaimBalanceBaselines(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	claimables map[string]decimal.Decimal,
) (map[string]info.SpotBalanceAmounts, error) {
	baselines := make(map[string]info.SpotBalanceAmounts, len(o.builders))
	pending := make([]bool, len(o.builders))
	candidates := make([]info.SpotBalanceAmounts, len(o.builders))
	consecutive := make([]int, len(o.builders))
	for index, builder := range o.builders {
		claimable, observed := claimables[builder]
		pending[index] = claimEligible(claimable, observed)
	}

	for attempt := 1; attempt <= claimBaselineQueryAttempts; attempt++ {
		indexes := pendingBuilderIndexes(pending)
		if len(indexes) == 0 {
			break
		}
		results := make(chan balanceQueryResult, len(indexes))
		for _, index := range indexes {
			index := index
			go func() {
				balance, err := o.chain.SpotBalance(ctx, o.builders[index], token)
				results <- balanceQueryResult{index: index, balance: balance, err: err}
			}()
		}
		round := make([]balanceQueryResult, len(o.builders))
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
			if result.err == nil {
				if consecutive[index] > 0 &&
					result.balance.Total.Equal(candidates[index].Total) &&
					result.balance.Available.Equal(candidates[index].Available) {
					consecutive[index]++
				} else {
					candidates[index] = result.balance
					consecutive[index] = 1
				}
				o.info(ctx, "builder pre-claim balance captured",
					slog.String("event", "funding_builder_claim_balance_baseline_captured"),
					slog.String("run_id", state.RunID),
					slog.String("builder", builder),
					slog.Int("attempt", attempt),
					slog.String("total", result.balance.Total.String()),
					slog.String("available", result.balance.Available.String()),
					slog.Int("consecutive_matches", consecutive[index]))
				if consecutive[index] >= claimBaselineRequiredConfirmations {
					pending[index] = false
					baselines[builder] = result.balance
				}
				continue
			}
			consecutive[index] = 0
			o.warn(ctx, "builder pre-claim balance query failed",
				slog.String("event", "funding_builder_claim_balance_baseline_query_failed"),
				slog.String("run_id", state.RunID),
				slog.String("builder", builder),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", claimBaselineQueryAttempts),
				logging.Err(result.err))
		}
		if len(pendingBuilderIndexes(pending)) > 0 && attempt < claimBaselineQueryAttempts {
			if err := o.sleeper.Sleep(ctx, claimBaselineQueryInterval); err != nil {
				return nil, err
			}
		}
	}

	for index, waiting := range pending {
		if !waiting {
			continue
		}
		builder := o.builders[index]
		summary := "Claim 前余额基线查询失败，未提交 Reward 领取"
		if builderReport := report.builder(builder); builderReport != nil {
			builderReport.ClaimStatus = ReportStepWarning
			builderReport.ClaimSummary = summary
			report.addWarning(builderReport.Name + ": " + summary)
		}
		o.alertOnce(
			ctx, state.RunID, "builder_claim_baseline_failed:"+strings.ToLower(builder),
			AlertSeverityWarning,
			fmt.Sprintf("builder pre-claim balance baseline query failed\nbuilder: %s", builder),
		)
	}
	return baselines, nil
}

func (o *Orchestrator) claimBuilderRewards(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	claimables map[string]decimal.Decimal,
	baselines map[string]info.SpotBalanceAmounts,
) (map[string]claimBalanceTarget, error) {
	claimBalanceTargets := make(map[string]claimBalanceTarget, len(o.builders))
	threshold := decimal.NewFromInt(claimRewardThresholdUSDC)
	indexes := make([]int, 0, len(o.builders))
	for index, builder := range o.builders {
		claimable, observed := claimables[builder]
		eligible := claimEligible(claimable, observed)
		if !eligible {
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimStatus = ReportStepSkipped
				builderReport.ClaimSummary = "低于 1 USDC 阈值"
			}
		} else if _, baselineObserved := baselines[builder]; baselineObserved {
			indexes = append(indexes, index)
		}
		attrs := []slog.Attr{
			slog.String("event", "funding_builder_claim_eligibility_checked"),
			slog.String("run_id", state.RunID), slog.String("builder", builder),
			slog.String("threshold_usdc", threshold.String()),
			slog.Bool("claimable_observed", observed),
			slog.Bool("eligible", eligible),
		}
		if observed {
			attrs = append(attrs, slog.String("claimable_usdc", claimable.String()))
		}
		o.info(ctx, "builder claim eligibility checked", attrs...)
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
			result, submitErr := o.chain.Submit(ctx, prepared)
			results <- claimAttemptResult{index: index, result: result, submitErr: submitErr}
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
		claimable, claimableKnown := claimables[builder]
		minimumIncrease := claimable.Truncate(int32(token.WeiDecimals))
		if !claimableKnown {
			minimumIncrease = threshold
		}
		target := claimBalanceTarget{
			totalBefore:     baselines[builder].Total,
			availableBefore: baselines[builder].Available,
			claimableBefore: claimable,
			claimableKnown:  claimableKnown,
			minimumIncrease: minimumIncrease,
		}
		result := round[index]
		if builderReport := report.builder(builder); builderReport != nil {
			builderReport.ClaimAttempts = 1
		}
		if result.prepareErr != nil {
			o.builderFailure(ctx, state, report, builder, "claimRewards", "prepare", result.prepareErr)
			continue
		}
		report.markSubmitted(builder)
		o.logBuilderSubmitResult(ctx, state, builder, "claimRewards", "", result.result, result.submitErr)
		switch {
		case result.submitErr == nil && result.result.Accepted:
			claimBalanceTargets[builder] = target
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimStatus = ReportStepSuccess
				builderReport.ClaimSummary = "Reward 已领取"
				if !claimableKnown {
					builderReport.ClaimSummary += "；零点 Claimable 数据缺失"
				}
			}
		case result.submitErr == nil && result.result.Rejected &&
			!claimableKnown && claimBelowThreshold(result.result):
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimStatus = ReportStepSkipped
				builderReport.ClaimSummary = "未达到 1 USDC 领取门槛（由交易所确认）"
			}
			o.info(ctx, "builder claim below threshold confirmed by exchange",
				slog.String("event", "funding_builder_claim_below_threshold_confirmed"),
				slog.String("run_id", state.RunID),
				slog.String("builder", builder),
				slog.String("exchange_message", claimBelowThresholdRejection))
		case result.submitErr == nil && result.result.Rejected:
			o.builderFailure(ctx, state, report, builder, "claimRewards", "rejected")
		default:
			target.submitUnknown = true
			claimBalanceTargets[builder] = target
			if builderReport := report.builder(builder); builderReport != nil {
				builderReport.ClaimUncertain = true
				builderReport.ClaimSummary = "Reward 提交结果不确定，等待余额变化确认"
			}
			o.warn(ctx, "builder claim result is uncertain; observing balance without resubmission",
				slog.String("event", "funding_builder_claim_observation_planned"),
				slog.String("run_id", state.RunID),
				slog.String("builder", builder),
				slog.String("total_before", target.totalBefore.String()),
				slog.String("minimum_total", target.minimumTotal().String()))
		}
	}
	report.setStage("rewards", aggregateBuilderStep(report.Builders, true), builderStepSummary(report.Builders, true))
	return claimBalanceTargets, nil
}

func claimBelowThreshold(result exchange.SubmitResult) bool {
	message, ok := result.RejectionMessage()
	return ok && message == claimBelowThresholdRejection
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

func (o *Orchestrator) sweepBuilderBalances(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	claimEndingBalances map[string]info.SpotBalanceAmounts,
	unconvergedClaims map[string]struct{},
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, builder := range o.builders {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimFloor, claimFloorKnown := claimEndingBalances[builder]
		if _, unconverged := unconvergedClaims[builder]; unconverged {
			claimFloorKnown = false
		}
		hadUnknown := false
		retryAmount := decimal.Zero
		for attempt := 1; attempt <= sweepSubmitAttempts; attempt++ {
			if attempt > 1 {
				if err := o.sleeper.Sleep(ctx, sweepSubmitRetryInterval); err != nil {
					return err
				}
			}

			// Once a prior request is ambiguous, a lower source balance is an
			// expected outcome rather than a stale regression. Reload the current
			// stable Available; the retry amount is selected below using the last
			// ambiguous request as a floor against stale lower snapshots.
			minimumKnown := claimFloorKnown && !hadUnknown
			balance, balanceErr := o.loadSweepBalance(
				ctx, state, report, token, builder, claimFloor.Total, minimumKnown,
			)
			if balanceErr != nil {
				if err := ctx.Err(); err != nil {
					return err
				}
				if hadUnknown {
					o.builderFailure(ctx, state, report, builder, "spotSend", "unknown", balanceErr)
				} else {
					o.builderFailure(ctx, state, report, builder, "spotBalance", "query", balanceErr)
				}
				break
			}
			amount := balance.Available
			if hadUnknown && retryAmount.GreaterThan(amount) {
				// A lower balance can be a stale pre-claim or partial snapshot. Retrying
				// at least the last ambiguous amount is safe for this internal transfer:
				// if that request already applied, the exchange rejects the retry for
				// insufficient balance.
				amount = retryAmount
				o.warn(ctx, "builder sweep retry is using last ambiguous amount after lower balance",
					slog.String("event", "funding_builder_sweep_retry_stale_lower_guard"),
					slog.String("run_id", state.RunID),
					slog.String("builder", builder),
					slog.String("observed_available", balance.Available.String()),
					slog.String("amount", amount.String()),
					slog.Int("attempt", attempt))
			}
			if !amount.IsPositive() {
				if builderReport := report.builder(builder); builderReport != nil {
					if _, unconverged := unconvergedClaims[builder]; unconverged {
						builderReport.SweepStatus = ReportStepWarning
						builderReport.SweepSummary = "Reward 余额未确认刷新，当前无可归集余额"
						report.addWarning(builderReport.Name + ": " + builderReport.SweepSummary)
					} else {
						builderReport.SweepStatus = ReportStepSkipped
						builderReport.SweepSummary = "无可归集余额"
					}
				}
				break
			}

			prepared, prepareErr := o.chain.PrepareSpotSend(
				builder, state.Manifest.Settlement, token, amount, o.nonce.Next(),
			)
			if prepareErr != nil {
				if hadUnknown && attempt < sweepSubmitAttempts {
					o.logSweepRetry(ctx, state, builder, attempt, "prepare", prepareErr)
					continue
				}
				outcome := "prepare"
				if hadUnknown {
					outcome = "unknown"
				}
				o.builderFailure(ctx, state, report, builder, "spotSend", outcome, prepareErr)
				break
			}

			result, submitErr := o.chain.Submit(ctx, prepared)
			report.markSubmitted(builder)
			if builderReport := report.builder(builder); builderReport != nil {
				// The latest recorded balance was captured before this request and
				// must not be presented as the Builder's ending balance.
				builderReport.FinalTotal = ""
				builderReport.FinalAvailable = ""
			}
			o.logBuilderSubmitResult(ctx, state, builder, "spotSend", amount.String(), result, submitErr)
			if submitErr == nil && result.Accepted {
				if hadUnknown {
					markSweepRetryAccepted(
						report, builder, amount.String(), attempt, unconvergedClaims,
					)
				} else {
					o.markSweepSucceeded(
						report, builder, amount.String(), unconvergedClaims, "资金已归集",
					)
				}
				break
			}
			if submitErr == nil && result.Rejected && !hadUnknown {
				o.builderFailure(ctx, state, report, builder, "spotSend", "rejected")
				break
			}

			if submitErr != nil || !result.Rejected {
				hadUnknown = true
				retryAmount = amount
			}
			if attempt == sweepSubmitAttempts {
				o.builderFailure(ctx, state, report, builder, "spotSend", "unknown", submitErr)
				break
			}
			o.logSweepRetry(ctx, state, builder, attempt, "submit_result", submitErr)
		}
	}
	return nil
}

func (o *Orchestrator) logSweepRetry(
	ctx context.Context,
	state *RunState,
	builder string,
	attempt int,
	reason string,
	cause ...error,
) {
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_sweep_retry_planned"),
		slog.String("run_id", state.RunID),
		slog.String("builder", builder),
		slog.String("reason", reason),
		slog.Int("attempt", attempt),
		slog.Int("max_attempts", sweepSubmitAttempts),
	}
	if len(cause) > 0 && cause[0] != nil {
		attrs = append(attrs, logging.Err(cause[0]))
	}
	o.warn(ctx, "builder sweep remains uncertain; retrying current available balance", attrs...)
}

func (o *Orchestrator) loadSweepBalance(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	builder string,
	minimumTotal decimal.Decimal,
	minimumTotalKnown bool,
) (info.SpotBalanceAmounts, error) {
	var (
		candidate   info.SpotBalanceAmounts
		lastErr     error
		consecutive int
	)
	for attempt := 1; attempt <= sweepBalanceQueryAttempts; attempt++ {
		balance, err := o.chain.SpotBalance(ctx, builder, token)
		if err == nil {
			lastErr = nil
			if minimumTotalKnown && balance.Total.LessThan(minimumTotal) {
				consecutive = 0
				o.warn(ctx, "builder sweep balance regressed below confirmed claim floor",
					slog.String("event", "funding_builder_sweep_balance_below_claim_floor"),
					slog.String("run_id", state.RunID),
					slog.String("builder", builder),
					slog.String("minimum_total", minimumTotal.String()),
					slog.String("total", balance.Total.String()),
					slog.Int("attempt", attempt))
				if attempt < sweepBalanceQueryAttempts {
					if sleepErr := o.sleeper.Sleep(ctx, sweepBalanceQueryInterval); sleepErr != nil {
						return info.SpotBalanceAmounts{}, sleepErr
					}
				}
				continue
			}
			o.recordBuilderBalance(ctx, state, report, builder, attempt, balance, nil)
			if consecutive > 0 && sameSpotBalance(balance, candidate) {
				consecutive++
			} else {
				candidate = balance
				consecutive = 1
			}
			if consecutive >= sweepBalanceRequiredConfirmations {
				return balance, nil
			}
		} else {
			lastErr = err
			consecutive = 0
			o.warn(ctx, "builder sweep balance query failed",
				slog.String("event", "funding_builder_sweep_balance_query_failed"),
				slog.String("run_id", state.RunID),
				slog.String("builder", builder),
				slog.Int("attempt", attempt),
				slog.Int("max_attempts", sweepBalanceQueryAttempts),
				logging.Err(err))
		}
		if attempt < sweepBalanceQueryAttempts {
			if err := o.sleeper.Sleep(ctx, sweepBalanceQueryInterval); err != nil {
				return info.SpotBalanceAmounts{}, err
			}
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("builder sweep balance did not stabilize")
	}
	return info.SpotBalanceAmounts{}, lastErr
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

func markSweepRetryAccepted(
	report *RunReport,
	builder, amount string,
	attempt int,
	unconvergedClaims map[string]struct{},
) {
	builderReport := report.builder(builder)
	if builderReport == nil {
		return
	}
	report.addSweepAmount(builderReport, amount)
	builderReport.SweepStatus = ReportStepWarning
	builderReport.SweepSummary = fmt.Sprintf(
		"第 %d 次归集已接受；此前响应未知，累计归集金额无法确认", attempt,
	)
	if _, unconverged := unconvergedClaims[builder]; unconverged {
		builderReport.SweepSummary += "；Reward 余额未确认刷新"
	}
	report.addWarning(builderReport.Name + ": " + builderReport.SweepSummary)
}

func (o *Orchestrator) observeClaimBalances(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	token info.Token,
	targets map[string]claimBalanceTarget,
) (map[string]info.SpotBalanceAmounts, map[string]struct{}, error) {
	balances := make(map[string]info.SpotBalanceAmounts, len(targets))
	if len(targets) == 0 {
		return balances, nil, nil
	}
	pending := make(map[string]claimBalanceTarget, len(targets))
	for builder, target := range targets {
		pending[builder] = target
	}
	consecutive := make(map[string]int, len(targets))
	candidates := make(map[string]info.SpotBalanceAmounts, len(targets))
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
			target, waiting := pending[builder]
			if !waiting {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			balance, err := o.chain.SpotBalance(ctx, builder, token)
			if err != nil {
				consecutive[builder] = 0
				o.warn(ctx, "builder claim balance observation failed",
					slog.String("event", "funding_builder_claim_balance_query_failed"),
					slog.String("run_id", state.RunID),
					slog.String("builder", builder),
					slog.Int("attempt", attempt),
					slog.Int("max_attempts", claimBalanceObservationAttempts),
					logging.Err(err))
				continue
			}
			balances[builder] = balance
			if target.submitUnknown &&
				balance.Total.Equal(target.totalBefore) &&
				balance.Available.Equal(target.availableBefore) {
				target.preStateSeenAfterSubmit = true
				pending[builder] = target
			}
			o.recordBuilderBalance(ctx, state, report, builder, attempt, balance, &target)
			visible := target.visible(balance.Total)
			claimableTransitioned := !target.submitUnknown || !target.claimableKnown
			if visible && target.submitUnknown && target.claimableKnown {
				claimableAfter, claimableErr := o.chain.ClaimableUSDC(ctx, builder, token)
				if claimableErr != nil {
					consecutive[builder] = 0
					o.warn(ctx, "builder claimable transition query failed",
						slog.String("event", "funding_builder_claimable_transition_query_failed"),
						slog.String("run_id", state.RunID),
						slog.String("builder", builder),
						slog.Int("attempt", attempt),
						logging.Err(claimableErr))
					continue
				}
				claimableTransitioned = claimableAfter.LessThan(target.claimableBefore)
				o.info(ctx, "builder claimable transition observed",
					slog.String("event", "funding_builder_claimable_transition_observed"),
					slog.String("run_id", state.RunID),
					slog.String("builder", builder),
					slog.String("claimable_before", target.claimableBefore.String()),
					slog.String("claimable_after", claimableAfter.String()),
					slog.Bool("transitioned", claimableTransitioned),
					slog.Int("attempt", attempt))
			}
			visible = visible && claimableTransitioned
			if target.submitUnknown {
				visible = visible && target.preStateSeenAfterSubmit
				if !target.claimableKnown {
					// Without a captured claimable value, a Total-only change cannot
					// safely resolve an ambiguous claim. Keep observing so the latest
					// balance can still be swept on a best-effort basis.
					visible = false
				}
			}
			if visible {
				if consecutive[builder] > 0 && sameSpotBalance(balance, candidates[builder]) {
					consecutive[builder]++
				} else {
					candidates[builder] = balance
					consecutive[builder] = 1
				}
			} else {
				consecutive[builder] = 0
			}
			visible = consecutive[builder] >= claimBalanceRequiredConfirmations
			if visible {
				if target.submitUnknown {
					o.resolveUncertainClaim(report, builder)
				}
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
		target, waiting := pending[builder]
		if !waiting {
			continue
		}
		unconverged[builder] = struct{}{}
		balance, observed := balances[builder]
		o.claimBalanceObservationExhausted(ctx, state, report, builder, target, balance, observed)
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
	claimTarget *claimBalanceTarget,
) {
	if builderReport := report.builder(builder); builderReport != nil {
		builderReport.FinalTotal = balance.Total.String()
		builderReport.FinalAvailable = balance.Available.String()
	}
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_balance_observed"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.Int("attempt", attempt), slog.String("total", balance.Total.String()),
		slog.String("hold", balance.Total.Sub(balance.Available).String()),
		slog.String("available", balance.Available.String()),
	}
	if claimTarget != nil {
		attrs = append(attrs,
			slog.String("total_before", claimTarget.totalBefore.String()),
			slog.String("minimum_increase", claimTarget.minimumIncrease.String()),
			slog.String("minimum_total", claimTarget.minimumTotal().String()),
			slog.Bool("pre_state_seen_after_submit", claimTarget.preStateSeenAfterSubmit),
			slog.Bool("claim_balance_visible", claimTarget.visible(balance.Total)),
		)
	}
	o.info(ctx, "builder balance observed", attrs...)
}

func (o *Orchestrator) claimBalanceObservationExhausted(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	builder string,
	target claimBalanceTarget,
	balance info.SpotBalanceAmounts,
	observed bool,
) {
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_claim_balance_observation_exhausted"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.Int("attempt_count", claimBalanceObservationAttempts),
		slog.String("total_before", target.totalBefore.String()),
		slog.String("minimum_increase", target.minimumIncrease.String()),
		slog.String("minimum_total", target.minimumTotal().String()),
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
		if !target.submitUnknown {
			builderReport.ClaimStatus = ReportStepWarning
			builderReport.ClaimSummary = "Reward 请求已接受，但余额未确认刷新"
			report.addWarning(builderReport.Name + ": " + builderReport.ClaimSummary)
		}
	}
	if target.submitUnknown {
		o.markClaimUncertain(ctx, state, report, builder)
	}
	o.alertOnce(
		ctx, state.RunID, "builder_claim_balance_not_visible:"+strings.ToLower(builder),
		AlertSeverityWarning,
		fmt.Sprintf(
			"builder claim balance did not become visible\nbuilder: %s\nminimum increase: %s\nattempts: %d",
			builder, target.minimumIncrease, claimBalanceObservationAttempts,
		),
	)
}

func (o *Orchestrator) resolveUncertainClaim(report *RunReport, builder string) {
	builderReport := report.builder(builder)
	if builderReport == nil {
		return
	}
	builderReport.ClaimUncertain = false
	builderReport.ClaimStatus = ReportStepSuccess
	builderReport.ClaimSummary = "Reward 已领取（由余额增长确认；未重发）"
	if !builderReport.ClaimableObserved {
		builderReport.ClaimSummary += "；零点 Claimable 数据缺失"
	}
}

func (o *Orchestrator) logBuilderSubmitResult(
	ctx context.Context,
	state *RunState,
	builder, action, amount string,
	result exchange.SubmitResult,
	submitErr error,
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
	if submitErr != nil {
		attrs = append(attrs, logging.Err(submitErr))
	}
	if !result.Accepted && len(result.Response) != 0 {
		attrs = append(attrs, slog.String(
			"response", logging.SafeDiagnosticExcerpt(string(result.Response), 1024),
		))
	}
	o.info(ctx, "builder submission returned", attrs...)
}

func (o *Orchestrator) builderFailure(
	ctx context.Context,
	state *RunState,
	report *RunReport,
	builder, action, outcome string,
	cause ...error,
) {
	attrs := []slog.Attr{
		slog.String("event", "funding_builder_action_failed"),
		slog.String("run_id", state.RunID), slog.String("builder", builder),
		slog.String("action_kind", action),
		slog.String("outcome", outcome),
	}
	if len(cause) > 0 && cause[0] != nil {
		attrs = append(attrs, logging.Err(cause[0]))
	}
	o.warn(ctx, "builder action did not complete", attrs...)
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
		ctx, state.RunID,
		"builder_"+action+"_failed:"+strings.ToLower(strings.TrimSpace(builder)),
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

func (o *Orchestrator) saveActivePhase(ctx context.Context, state *RunState, phase Phase) error {
	target, _, err := state.activePayoutTarget()
	if err != nil {
		return err
	}
	if target == nil {
		return fmt.Errorf("cannot persist phase %q without an active payout target", phase)
	}
	target.Phase = phase
	return o.saveState(ctx, state)
}

func (o *Orchestrator) saveState(ctx context.Context, state *RunState) error {
	state.UpdatedAt = o.clock.Now().UTC()
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	target, _, _ := state.activePayoutTarget()
	phase := PhaseCompleted
	kind := PayoutKind("")
	if target != nil {
		phase = target.Phase
		kind = target.Kind
	}
	o.info(ctx, "funding phase persisted",
		slog.String("event", "funding_phase_persisted"), slog.String("run_id", state.RunID),
		slog.String("payout_kind", string(kind)),
		slog.String("phase", string(phase)))
	return nil
}

func (o *Orchestrator) completeDatabase(
	ctx context.Context,
	state *RunState,
	target *PayoutTargetState,
	report *RunReport,
) error {
	ids := make([]uint64, len(target.Manifest.Records))
	for i, record := range target.Manifest.Records {
		ids[i] = record.ID
	}
	o.info(ctx, "funding database completion started",
		slog.String("event", "funding_database_completion_started"),
		slog.String("run_id", state.RunID),
		slog.String("payout_kind", string(target.Kind)),
		slog.Int("record_count", len(ids)))
	if err := o.repository.Complete(ctx, ids); err != nil {
		o.alertOnce(
			ctx, state.RunID, string(target.Kind)+"_database_completion_failed",
			AlertSeverityWarning, "funding database completion failed",
		)
		report.failTarget(
			target.Kind, "database",
			"MySQL funding records 未能标记完成。",
			"Payout 状态是安全的；服务会持续重试数据库操作，请检查 MySQL 可用性。",
		)
		return err
	}
	target.Phase = PhaseCompleted
	if len(ids) == 0 {
		report.setTargetStage(target.Kind, "database", ReportStepSkipped, "没有待完成记录")
		report.target(target.Kind).DatabaseStatus = ReportStepSkipped
	} else {
		report.setTargetStage(target.Kind, "database", ReportStepSuccess, fmt.Sprintf("%d 条记录已完成", len(ids)))
		report.target(target.Kind).DatabaseStatus = ReportStepSuccess
	}
	if err := o.saveState(ctx, state); err != nil {
		report.failTarget(
			target.Kind, "database",
			"数据库已完成，但目标完成状态持久化失败。",
			"数据库 Complete 可安全幂等重试；请检查 data 目录和文件系统状态。",
		)
		return err
	}
	report.syncState(state)
	o.info(ctx, "funding payout target completed",
		slog.String("event", "funding_payout_target_completed"),
		slog.String("run_id", state.RunID),
		slog.String("payout_kind", string(target.Kind)),
		slog.Int("record_count", len(ids)),
		slog.String("raw_total", target.Manifest.RawTotal),
		slog.String("payout_total", target.Manifest.PayoutTotal))
	return o.runActiveTarget(ctx, state, report)
}

func (o *Orchestrator) finishRun(ctx context.Context, state *RunState, report *RunReport) error {
	if err := o.store.Archive(ctx, *state, "completed"); err != nil {
		report.fail("finalize", "运行历史归档失败。", "请检查 data/history 目录和文件系统状态。")
		return err
	}
	if err := o.store.Clear(ctx); err != nil {
		report.fail("finalize", "已完成运行的 current 状态清理失败。", "请检查 data 目录并保留现场。")
		return err
	}
	report.setStage("finalize", ReportStepSuccess, "运行状态已归档并清理")
	report.syncState(state)
	o.info(ctx, "funding run completed",
		slog.String("event", "funding_run_completed"), slog.String("run_id", state.RunID),
		slog.Int("record_count", report.RecordCount),
		slog.String("payout_total", report.PayoutTotal))
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
			target, _, _ := current.activePayoutTarget()
			phase := PhaseCompleted
			kind := PayoutKind("")
			if target != nil {
				phase = target.Phase
				kind = target.Kind
			}
			attrs = append(attrs,
				slog.String("run_id", current.RunID),
				slog.String("payout_kind", string(kind)),
				slog.String("phase", string(phase)))
		}
		o.info(ctx, "funding current state checked", attrs...)
	}
	if err == nil && metadata.RecoveredFromBackup {
		runID := "state-store"
		if current != nil && current.RunID != "" {
			runID = current.RunID
		}
		attrs := []slog.Attr{
			slog.String("event", "state_snapshot_recovered_from_backup"),
			slog.String("run_id", runID),
			slog.Bool("primary_invalid", metadata.PrimaryInvalid),
		}
		if metadata.PrimaryError != nil {
			attrs = append(attrs, logging.Err(metadata.PrimaryError))
		}
		o.warn(ctx, "funding state recovered from backup", attrs...)
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
