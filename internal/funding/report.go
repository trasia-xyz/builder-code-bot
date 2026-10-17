package funding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

type ReportStatus string

const (
	ReportStatusSuccess  ReportStatus = "success"
	ReportStatusWarning  ReportStatus = "warning"
	ReportStatusNoData   ReportStatus = "no_data"
	ReportStatusRetrying ReportStatus = "retrying"
	ReportStatusCritical ReportStatus = "critical"
)

type ReportStepStatus string

const (
	ReportStepPending ReportStepStatus = "pending"
	ReportStepSuccess ReportStepStatus = "success"
	ReportStepWarning ReportStepStatus = "warning"
	ReportStepSkipped ReportStepStatus = "skipped"
	ReportStepFailed  ReportStepStatus = "failed"
)

type ReportExecution struct {
	Attempt     int
	MaxAttempts int
	Recovery    bool
}

type ReportStage struct {
	Key     string
	Label   string
	Status  ReportStepStatus
	Summary string
}

type BuilderReport struct {
	Name                           string
	Address                        string
	ClaimableUSDC                  string
	ClaimableObserved              bool
	ClaimableQueryAttempts         int
	ClaimAttempts                  int
	ClaimUncertain                 bool
	ClaimStatus                    ReportStepStatus
	ClaimSummary                   string
	SweepAmount                    string
	SweepStatus                    ReportStepStatus
	SweepSummary                   string
	FinalTotal                     string
	FinalAvailable                 string
	RateLimitRemaining             uint64
	RateLimitObserved              bool
	RateLimitQueryFailed           bool
	RateLimitVisibilityUnconfirmed bool
}

type DailyConsolidationReport struct {
	UTCDate              string
	Status               DailyConsolidationStatus
	ClaimablesCapturedAt time.Time
	CompletedAt          time.Time
	TotalClaimableUSDC   string
	BuilderCount         int
	RecordedBuilders     int
	ObservedBuilders     int
	UncertainClaims      int
	Observed             bool
	Missing              bool
	LoadFailed           bool
	CoversPayout         bool
	BelowPayout          bool
	ShortfallUSDC        string
}

type PayoutReport struct {
	Status                ReportStepStatus
	Amount                string
	Settlement            string
	Recipient             string
	SettlementTotalBefore string
	SubmitAttempts        int
	Blocked               bool
	ConfirmationEvidence  string
	RequestHash           string
	Nonce                 uint64
}

type PayoutTargetReport struct {
	Kind           PayoutKind
	RecordsRead    bool
	RecordCount    int
	RawTotal       string
	PayoutTotal    string
	Payout         PayoutReport
	DatabaseStatus ReportStepStatus
}

type SettlementBalanceReport struct {
	Address               string
	Total                 string
	Hold                  string
	Available             string
	Observed              bool
	QueryFailed           bool
	PostPayoutUnconfirmed bool
}

type RunReport struct {
	Status                                   ReportStatus
	Compact                                  bool
	Trigger                                  Trigger
	Outcome                                  string
	Attempt                                  int
	MaxAttempts                              int
	Recovery                                 bool
	Network                                  string
	RunID                                    string
	UTCDate                                  string
	Phase                                    Phase
	StartedAt                                time.Time
	ClaimablesCapturedAt                     time.Time
	FinishedAt                               time.Time
	RecordCount                              int
	RecordsRead                              bool
	RawTotal                                 string
	PayoutTotal                              string
	Stages                                   []ReportStage
	Builders                                 []BuilderReport
	DailyConsolidation                       DailyConsolidationReport
	Payouts                                  []PayoutTargetReport
	SettlementBalance                        SettlementBalanceReport
	SettlementRateLimitRemaining             uint64
	SettlementRateLimitObserved              bool
	SettlementRateLimitQueryFailed           bool
	SettlementRateLimitVisibilityUnconfirmed bool
	Warnings                                 []string
	FailureStage                             string
	FailureSummary                           string
	NextAction                               string
	token                                    *info.Token
	submittedAccounts                        map[string]struct{}
}

func newRunReport(
	now time.Time,
	trigger Trigger,
	execution ReportExecution,
	network string,
	builders []string,
	builderNames map[string]string,
	settlement, referralRecipient string,
) RunReport {
	report := RunReport{
		Trigger:     trigger,
		Attempt:     execution.Attempt,
		MaxAttempts: execution.MaxAttempts,
		Recovery:    execution.Recovery,
		Network:     strings.TrimSpace(network),
		StartedAt:   now.UTC(),
		Stages: []ReportStage{
			{Key: "snapshot", Label: "数据快照", Status: ReportStepPending},
			{Key: "rewards", Label: "Reward", Status: ReportStepPending},
			{Key: "sweep", Label: "资金归集", Status: ReportStepPending},
			{Key: targetStageKey(PayoutKindReferral, "payout"), Label: "Referral Payout", Status: ReportStepPending},
			{Key: targetStageKey(PayoutKindReferral, "database"), Label: "Referral 数据库", Status: ReportStepPending},
			{Key: "finalize", Label: "状态归档", Status: ReportStepPending},
		},
		Payouts: []PayoutTargetReport{
			{
				Kind: PayoutKindReferral,
				Payout: PayoutReport{
					Status: ReportStepPending, Settlement: settlement,
					Recipient: referralRecipient,
				},
				DatabaseStatus: ReportStepPending,
			},
		},
		SettlementBalance: SettlementBalanceReport{Address: settlement},
	}
	report.submittedAccounts = make(map[string]struct{})
	report.Builders = make([]BuilderReport, 0, len(builders))
	for _, address := range builders {
		name := builderNames[strings.ToLower(strings.TrimSpace(address))]
		if name == "" {
			name = "Builder"
		}
		report.Builders = append(report.Builders, BuilderReport{
			Name: name, Address: address,
			ClaimStatus: ReportStepPending, SweepStatus: ReportStepPending,
		})
	}
	return report
}

func (r *RunReport) syncState(state *RunState) {
	if r == nil || state == nil {
		return
	}
	r.RunID = state.RunID
	r.UTCDate = state.UTCDate
	active, _, activeErr := state.activePayoutTarget()
	if activeErr == nil && active != nil {
		r.Phase = active.Phase
	} else if activeErr == nil {
		r.Phase = PhaseCompleted
	}
	r.RecordCount = 0
	r.RecordsRead = len(state.Targets) > 0
	rawTotal := decimal.Zero
	payoutTotal := decimal.Zero
	for index := range state.Targets {
		stateTarget := &state.Targets[index]
		targetReport := r.target(stateTarget.Kind)
		targetReport.RecordsRead = true
		targetReport.RecordCount = len(stateTarget.Manifest.Records)
		targetReport.RawTotal = stateTarget.Manifest.RawTotal
		targetReport.PayoutTotal = stateTarget.Manifest.PayoutTotal
		targetReport.Payout.Amount = stateTarget.Manifest.PayoutTotal
		targetReport.Payout.Settlement = stateTarget.Manifest.Settlement
		targetReport.Payout.Recipient = stateTarget.Manifest.Recipient
		r.RecordCount += len(stateTarget.Manifest.Records)
		if parsed, err := decimal.NewFromString(stateTarget.Manifest.RawTotal); err == nil {
			rawTotal = rawTotal.Add(parsed)
		}
		if parsed, err := decimal.NewFromString(stateTarget.Manifest.PayoutTotal); err == nil {
			payoutTotal = payoutTotal.Add(parsed)
		}
		if r.SettlementBalance.Address == "" {
			r.SettlementBalance.Address = stateTarget.Manifest.Settlement
		}
		if r.token == nil && stateTarget.Manifest.Token != nil {
			r.token = cloneToken(stateTarget.Manifest.Token)
		}
		if stateTarget.Payout != nil {
			targetReport.Payout.SettlementTotalBefore = stateTarget.Payout.TotalBefore
			targetReport.Payout.SubmitAttempts = stateTarget.Payout.SubmitAttempts
			targetReport.Payout.RequestHash = stateTarget.Payout.Prepared.RequestHash
			targetReport.Payout.Nonce = stateTarget.Payout.Prepared.Nonce
		}
		targetReport.Payout.Blocked = isUncertainPayoutBlock(stateTarget)
		if stateTarget.ConfirmationEvidence != "" {
			targetReport.Payout.ConfirmationEvidence = stateTarget.ConfirmationEvidence
		}
		r.syncTargetStatuses(stateTarget, targetReport)
	}
	r.RawTotal = rawTotal.StringFixed(18)
	r.PayoutTotal = payoutTotal.String()
}

func (r *RunReport) syncTargetStatuses(
	stateTarget *PayoutTargetState,
	targetReport *PayoutTargetReport,
) {
	if r == nil || stateTarget == nil || targetReport == nil {
		return
	}
	switch stateTarget.Phase {
	case PhasePayoutConfirmed:
		targetReport.Payout.Status = ReportStepSuccess
		r.setTargetStage(stateTarget.Kind, "payout", ReportStepSuccess, "Payout 已确认")
	case PhaseCompleted:
		if len(stateTarget.Manifest.Records) == 0 {
			targetReport.Payout.Status = ReportStepSkipped
			targetReport.DatabaseStatus = ReportStepSkipped
			r.setTargetStage(stateTarget.Kind, "payout", ReportStepSkipped, "没有待处理记录")
			r.setTargetStage(stateTarget.Kind, "database", ReportStepSkipped, "没有待完成记录")
		} else {
			if stateTarget.Manifest.PayoutTotal == "0" {
				targetReport.Payout.Status = ReportStepSkipped
				r.setTargetStage(stateTarget.Kind, "payout", ReportStepSkipped, "无需付款")
			} else {
				targetReport.Payout.Status = ReportStepSuccess
				r.setTargetStage(stateTarget.Kind, "payout", ReportStepSuccess, "Payout 已确认")
			}
			targetReport.DatabaseStatus = ReportStepSuccess
			r.setTargetStage(
				stateTarget.Kind, "database", ReportStepSuccess,
				fmt.Sprintf("%d 条记录已完成", len(stateTarget.Manifest.Records)),
			)
		}
	}
}

func targetStageKey(kind PayoutKind, stage string) string {
	return string(kind) + "_" + stage
}

func (r *RunReport) target(kind PayoutKind) *PayoutTargetReport {
	if r == nil {
		return nil
	}
	for index := range r.Payouts {
		if r.Payouts[index].Kind == kind {
			return &r.Payouts[index]
		}
	}
	r.Payouts = append(r.Payouts, PayoutTargetReport{
		Kind:           kind,
		Payout:         PayoutReport{Status: ReportStepPending},
		DatabaseStatus: ReportStepPending,
	})
	return &r.Payouts[len(r.Payouts)-1]
}

func (r *RunReport) setTargetStage(
	kind PayoutKind,
	stage string,
	status ReportStepStatus,
	summary string,
) {
	r.setStage(targetStageKey(kind, stage), status, summary)
}

func (r *RunReport) markTargetNoData(kind PayoutKind) {
	if r == nil {
		return
	}
	target := r.target(kind)
	target.RecordsRead = true
	target.RecordCount = 0
	target.RawTotal = "0.000000000000000000"
	target.PayoutTotal = "0"
	target.Payout.Amount = "0"
	target.Payout.Status = ReportStepSkipped
	target.DatabaseStatus = ReportStepSkipped
	r.setTargetStage(kind, "payout", ReportStepSkipped, "没有待处理记录")
	r.setTargetStage(kind, "database", ReportStepSkipped, "没有待完成记录")
}

func (r *RunReport) markSubmitted(address string) {
	if r == nil {
		return
	}
	if r.submittedAccounts == nil {
		r.submittedAccounts = make(map[string]struct{})
	}
	r.submittedAccounts[strings.ToLower(strings.TrimSpace(address))] = struct{}{}
}

func (r *RunReport) submitted(address string) bool {
	if r == nil {
		return false
	}
	_, exists := r.submittedAccounts[strings.ToLower(strings.TrimSpace(address))]
	return exists
}

func (r *RunReport) stage(key string) *ReportStage {
	if r == nil {
		return nil
	}
	for index := range r.Stages {
		if r.Stages[index].Key == key {
			return &r.Stages[index]
		}
	}
	return nil
}

func (r *RunReport) setStage(key string, status ReportStepStatus, summary string) {
	if stage := r.stage(key); stage != nil {
		stage.Status = status
		stage.Summary = summary
	}
}

func (r *RunReport) builder(address string) *BuilderReport {
	if r == nil {
		return nil
	}
	for index := range r.Builders {
		if strings.EqualFold(r.Builders[index].Address, address) {
			return &r.Builders[index]
		}
	}
	return nil
}

func (r *RunReport) addWarning(message string) {
	if r == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	for _, existing := range r.Warnings {
		if existing == message {
			return
		}
	}
	r.Warnings = append(r.Warnings, message)
}

func (r *RunReport) addSweepAmount(builder *BuilderReport, amount string) {
	if r == nil || builder == nil || amount == "" {
		return
	}
	next, err := decimal.NewFromString(amount)
	if err != nil {
		return
	}
	total := decimal.Zero
	if builder.SweepAmount != "" {
		if parsed, parseErr := decimal.NewFromString(builder.SweepAmount); parseErr == nil {
			total = parsed
		}
	}
	builder.SweepAmount = total.Add(next).String()
}

func (r *RunReport) fail(stage, summary, nextAction string) {
	if r == nil {
		return
	}
	r.FailureStage = stage
	r.FailureSummary = summary
	r.NextAction = nextAction
	if stage != "" {
		r.setStage(stage, ReportStepFailed, summary)
	}
}

func (r *RunReport) failTarget(
	kind PayoutKind,
	stage, summary, nextAction string,
) {
	if r == nil {
		return
	}
	stageKey := targetStageKey(kind, stage)
	r.FailureStage = stageKey
	r.FailureSummary = summary
	r.NextAction = nextAction
	target := r.target(kind)
	if stage == "payout" {
		target.Payout.Status = ReportStepFailed
	}
	if stage == "database" {
		target.DatabaseStatus = ReportStepFailed
	}
	r.setTargetStage(kind, stage, ReportStepFailed, summary)
}

func (r *RunReport) finalize(now time.Time, runErr error) bool {
	if r == nil {
		return false
	}
	r.FinishedAt = now.UTC()
	if errors.Is(runErr, context.Canceled) {
		return false
	}
	r.reconcileDailyConsolidation()
	if runErr == nil {
		switch r.Outcome {
		case "no_data":
			if len(r.Warnings) > 0 {
				r.Status = ReportStatusWarning
			} else {
				r.Status = ReportStatusNoData
			}
		default:
			if len(r.Warnings) > 0 {
				r.Status = ReportStatusWarning
			} else {
				r.Status = ReportStatusSuccess
			}
		}
		return true
	}

	retryAvailable := !IsFatal(runErr) && r.Attempt > 0 && r.MaxAttempts > 0 && r.Attempt < r.MaxAttempts
	if retryAvailable {
		r.Status = ReportStatusRetrying
		r.Compact = true
		if r.FailureSummary == "" {
			r.FailureSummary = "资金任务未能完成。"
		}
		if r.NextAction == "" {
			r.NextAction = "服务将在约 1 分钟后自动重试，无需立即人工操作。"
		}
		return true
	}
	r.Status = ReportStatusCritical
	if !IsFatal(runErr) && r.Attempt > 0 && r.Attempt == r.MaxAttempts {
		r.Outcome = "retry_exhausted"
	}
	if r.FailureSummary == "" {
		r.FailureSummary = "资金任务未能完成。"
	}
	if r.NextAction == "" {
		switch r.FailureStage {
		case "sweep":
			r.NextAction = "自动重试已耗尽，请检查 Builder 余额、Reward 可见性和 Settlement hold。"
		default:
			r.NextAction = "请检查服务日志和当前持久化状态后再决定是否重启。"
		}
	}
	return true
}

func (r *RunReport) reconcileDailyConsolidation() {
	if r == nil {
		return
	}
	consolidation := &r.DailyConsolidation
	consolidation.CoversPayout = false
	consolidation.BelowPayout = false
	consolidation.ShortfallUSDC = ""
	if !consolidation.Observed || consolidation.BuilderCount <= 0 ||
		consolidation.RecordedBuilders != consolidation.BuilderCount ||
		consolidation.ObservedBuilders != consolidation.BuilderCount {
		return
	}
	claimable, claimableErr := decimal.NewFromString(consolidation.TotalClaimableUSDC)
	payout, payoutErr := decimal.NewFromString(r.PayoutTotal)
	if claimableErr != nil || payoutErr != nil {
		return
	}
	if !claimable.LessThan(payout) {
		consolidation.CoversPayout = true
		return
	}
	shortfall := payout.Sub(claimable)
	consolidation.BelowPayout = true
	consolidation.ShortfallUSDC = shortfall.String()
	r.addWarning(fmt.Sprintf(
		"UTC 00:00 Claimable 合计 %s USDC，低于 Referral Payout %s USDC，短缺 %s USDC，请人工核查结算口径",
		claimable.String(), payout.String(), shortfall.String(),
	))
}

func aggregateBuilderStep(builders []BuilderReport, claim bool) ReportStepStatus {
	var success, warning, skipped int
	for _, builder := range builders {
		status := builder.SweepStatus
		if claim {
			status = builder.ClaimStatus
		}
		switch status {
		case ReportStepFailed, ReportStepWarning:
			warning++
		case ReportStepSuccess:
			success++
		case ReportStepSkipped:
			skipped++
		}
	}
	switch {
	case warning > 0:
		return ReportStepWarning
	case success > 0:
		return ReportStepSuccess
	case skipped == len(builders):
		return ReportStepSkipped
	default:
		return ReportStepPending
	}
}

func builderStepSummary(builders []BuilderReport, claim bool) string {
	var success, warning, skipped int
	for _, builder := range builders {
		status := builder.SweepStatus
		if claim {
			status = builder.ClaimStatus
		}
		switch status {
		case ReportStepFailed, ReportStepWarning:
			warning++
		case ReportStepSuccess:
			success++
		case ReportStepSkipped:
			skipped++
		}
	}
	return fmt.Sprintf("成功 %d，跳过 %d，告警 %d", success, skipped, warning)
}

func builderActionSummary(action, outcome string) string {
	actionName := map[string]string{
		"claimable":    "零点 Claimable",
		"claimRewards": "Reward 领取",
		"spotBalance":  "余额",
		"spotSend":     "资金归集",
	}[action]
	if actionName == "" {
		actionName = action
	}
	outcomeName := map[string]string{
		"query":    "查询失败",
		"prepare":  "请求准备失败",
		"rejected": "被明确拒绝",
		"unknown":  "结果不确定",
	}[outcome]
	if outcomeName == "" {
		outcomeName = "未完成"
	}
	return actionName + outcomeName
}

func markBuilderStepsSkipped(report *RunReport, summary string) {
	if report == nil {
		return
	}
	for index := range report.Builders {
		report.Builders[index].ClaimStatus = ReportStepSkipped
		report.Builders[index].ClaimSummary = summary
		report.Builders[index].SweepStatus = ReportStepSkipped
		report.Builders[index].SweepSummary = summary
	}
}

func cloneBuilderNames(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for address, name := range values {
		address = strings.ToLower(strings.TrimSpace(address))
		if address != "" {
			cloned[address] = strings.TrimSpace(name)
		}
	}
	return cloned
}
