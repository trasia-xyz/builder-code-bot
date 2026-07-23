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
	Name                 string
	Address              string
	ClaimableUSDC        string
	ClaimStatus          ReportStepStatus
	ClaimSummary         string
	SweepAmount          string
	SweepStatus          ReportStepStatus
	SweepSummary         string
	FinalTotal           string
	FinalAvailable       string
	RateLimitRemaining   uint64
	RateLimitObserved    bool
	RateLimitQueryFailed bool
}

type PayoutReport struct {
	Status                ReportStepStatus
	Amount                string
	Settlement            string
	Recipient             string
	SettlementTotalBefore string
	ConfirmationEvidence  string
	RequestHash           string
	Nonce                 uint64
}

type SettlementBalanceReport struct {
	Address     string
	Total       string
	Hold        string
	Available   string
	Observed    bool
	QueryFailed bool
}

type RunReport struct {
	Status                         ReportStatus
	Compact                        bool
	Trigger                        Trigger
	Outcome                        string
	Attempt                        int
	MaxAttempts                    int
	Recovery                       bool
	Network                        string
	RunID                          string
	UTCDate                        string
	Phase                          Phase
	StartedAt                      time.Time
	FinishedAt                     time.Time
	RecordCount                    int
	RecordsRead                    bool
	RawTotal                       string
	PayoutTotal                    string
	Stages                         []ReportStage
	Builders                       []BuilderReport
	Payout                         PayoutReport
	SettlementBalance              SettlementBalanceReport
	SettlementRateLimitRemaining   uint64
	SettlementRateLimitObserved    bool
	SettlementRateLimitQueryFailed bool
	Warnings                       []string
	FailureStage                   string
	FailureSummary                 string
	NextAction                     string
	token                          *info.Token
}

func newRunReport(
	now time.Time,
	trigger Trigger,
	execution ReportExecution,
	network string,
	builders []string,
	builderNames map[string]string,
	settlement, recipient string,
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
			{Key: "payout", Label: "Payout", Status: ReportStepPending},
			{Key: "database", Label: "数据库", Status: ReportStepPending},
		},
		Payout: PayoutReport{
			Status: ReportStepPending, Settlement: settlement, Recipient: recipient,
		},
		SettlementBalance: SettlementBalanceReport{Address: settlement},
	}
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
	r.Phase = state.Phase
	r.RecordCount = len(state.Manifest.Records)
	r.RecordsRead = true
	r.RawTotal = state.Manifest.RawTotal
	r.PayoutTotal = state.Manifest.PayoutTotal
	r.Payout.Amount = state.Manifest.PayoutTotal
	r.Payout.Settlement = state.Manifest.Settlement
	r.Payout.Recipient = state.Manifest.Recipient
	r.SettlementBalance.Address = state.Manifest.Settlement
	r.token = cloneToken(state.Manifest.Token)
	if state.Payout != nil {
		r.Payout.SettlementTotalBefore = state.Payout.TotalBefore
		r.Payout.RequestHash = state.Payout.Prepared.RequestHash
		r.Payout.Nonce = state.Payout.Prepared.Nonce
	}
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
	if stage == "payout" {
		r.Payout.Status = ReportStepFailed
	}
	if stage != "" {
		r.setStage(stage, ReportStepFailed, summary)
	}
}

func (r *RunReport) finalize(now time.Time, runErr error) bool {
	if r == nil {
		return false
	}
	r.FinishedAt = now.UTC()
	if errors.Is(runErr, context.Canceled) {
		return false
	}
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
		"claimRewards": "Reward 领取",
		"spotBalance":  "余额查询",
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
