package funding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

type DailyConsolidationStatus string

const (
	DailyConsolidationClaimablesCaptured DailyConsolidationStatus = "claimables_captured"
	DailyConsolidationCompleted          DailyConsolidationStatus = "completed"
)

// DailyConsolidation is the human-auditable result of the scheduled UTC 00:00
// task. The claimable values are persisted before any claim is submitted, so
// the reconciliation boundary is independent from later exchange responses.
// Payout recovery continues to use RunState exclusively.
type DailyConsolidation struct {
	UTCDate              string                      `json:"utc_date"`
	Status               DailyConsolidationStatus    `json:"status"`
	StartedAt            time.Time                   `json:"started_at"`
	ClaimablesCapturedAt time.Time                   `json:"claimables_captured_at"`
	CompletedAt          time.Time                   `json:"completed_at,omitzero"`
	Builders             []DailyConsolidationBuilder `json:"builders"`
}

type DailyConsolidationBuilder struct {
	Address                string           `json:"address"`
	ClaimableUSDC          string           `json:"claimable_usdc,omitempty"`
	ClaimableObserved      bool             `json:"claimable_observed"`
	ClaimableQueryAttempts int              `json:"claimable_query_attempts"`
	ClaimAttempts          int              `json:"claim_attempts"`
	ClaimUncertain         bool             `json:"claim_uncertain,omitempty"`
	ClaimStatus            ReportStepStatus `json:"claim_status"`
	ClaimSummary           string           `json:"claim_summary,omitempty"`
	SweepAmount            string           `json:"sweep_amount,omitempty"`
	SweepStatus            ReportStepStatus `json:"sweep_status"`
	SweepSummary           string           `json:"sweep_summary,omitempty"`
}

func (c DailyConsolidation) Validate() error {
	date, err := time.Parse(time.DateOnly, c.UTCDate)
	if err != nil || date.Format(time.DateOnly) != c.UTCDate {
		return fmt.Errorf("invalid daily consolidation UTC date %q", c.UTCDate)
	}
	if c.StartedAt.IsZero() || c.ClaimablesCapturedAt.IsZero() {
		return fmt.Errorf("daily consolidation capture times are required")
	}
	switch c.Status {
	case DailyConsolidationClaimablesCaptured:
		if !c.CompletedAt.IsZero() {
			return fmt.Errorf("captured daily consolidation has a completion time")
		}
	case DailyConsolidationCompleted:
		if c.CompletedAt.IsZero() {
			return fmt.Errorf("completed daily consolidation completion time is required")
		}
	default:
		return fmt.Errorf("invalid daily consolidation status %q", c.Status)
	}
	if len(c.Builders) == 0 {
		return fmt.Errorf("daily consolidation builders are required")
	}
	seen := make(map[string]struct{}, len(c.Builders))
	for index, builder := range c.Builders {
		address := strings.ToLower(strings.TrimSpace(builder.Address))
		if address == "" {
			return fmt.Errorf("daily consolidation builder %d address is required", index)
		}
		if _, duplicate := seen[address]; duplicate {
			return fmt.Errorf("duplicate daily consolidation builder address %q", builder.Address)
		}
		seen[address] = struct{}{}
		if builder.ClaimableQueryAttempts <= 0 {
			return fmt.Errorf("claimable query attempts for %q are required", builder.Address)
		}
		if builder.ClaimableObserved {
			if err := validateRequiredNonnegativeAmount(builder.ClaimableUSDC); err != nil {
				return fmt.Errorf("invalid claimable amount for %q: %w", builder.Address, err)
			}
		} else if strings.TrimSpace(builder.ClaimableUSDC) != "" {
			return fmt.Errorf("unobserved claimable for %q has an amount", builder.Address)
		}
		if builder.ClaimAttempts < 0 {
			return fmt.Errorf("invalid claim attempts for %q", builder.Address)
		}
		if err := validateOptionalNonnegativeAmount(builder.SweepAmount); err != nil {
			return fmt.Errorf("invalid sweep amount for %q: %w", builder.Address, err)
		}
		allowPending := c.Status == DailyConsolidationClaimablesCaptured
		if !validDailyStep(builder.ClaimStatus, allowPending) ||
			!validDailyStep(builder.SweepStatus, allowPending) {
			return fmt.Errorf("invalid daily consolidation status for %q", builder.Address)
		}
		if builder.ClaimUncertain && builder.ClaimStatus != ReportStepWarning {
			return fmt.Errorf("uncertain claim for %q must have warning status", builder.Address)
		}
	}
	return nil
}

func validateRequiredNonnegativeAmount(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("amount is required")
	}
	return validateOptionalNonnegativeAmount(value)
}

func validateOptionalNonnegativeAmount(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	amount, err := decimal.NewFromString(value)
	if err != nil {
		return err
	}
	if amount.IsNegative() {
		return fmt.Errorf("amount is negative")
	}
	return nil
}

func validDailyStep(status ReportStepStatus, allowPending bool) bool {
	if allowPending && status == ReportStepPending {
		return true
	}
	switch status {
	case ReportStepSuccess, ReportStepWarning, ReportStepSkipped:
		return true
	default:
		return false
	}
}

// RunDailyConsolidation captures and persists the UTC 00:00 claimable values
// before running claim and sweep. An existing record, including a partial one
// left after a crash, prevents the scheduled actions from being repeated. The
// consolidate-only entry point never calls this method.
func (o *Orchestrator) RunDailyConsolidation(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("daily consolidation context is nil")
	}
	if o == nil || o.dailyConsolidations == nil || o.chain == nil || o.clock == nil {
		return fmt.Errorf("daily consolidation runtime is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	utcDate := o.clock.Now().UTC().Format(time.DateOnly)
	existing, err := o.dailyConsolidations.LoadDailyConsolidation(ctx, utcDate)
	if err != nil {
		return fmt.Errorf("check existing daily consolidation: %w", err)
	}
	if existing != nil {
		o.info(ctx, "daily consolidation already exists",
			slog.String("event", "funding_daily_consolidation_skipped"),
			slog.String("utc_date", utcDate),
			slog.String("status", string(existing.Status)))
		return nil
	}

	report, err := o.runConsolidation(
		ctx,
		TriggerUTCConsolidation,
		func(captured *RunReport) error {
			record := dailyConsolidationFromReport(
				utcDate, DailyConsolidationClaimablesCaptured, time.Time{}, captured,
			)
			if err := o.dailyConsolidations.SaveDailyConsolidation(ctx, record); err != nil {
				return fmt.Errorf("save captured daily claimables: %w", err)
			}
			o.info(ctx, "daily claimables recorded before claim",
				slog.String("event", "funding_daily_claimables_recorded"),
				slog.String("utc_date", utcDate),
				slog.Time("captured_at", record.ClaimablesCapturedAt))
			return nil
		},
	)
	if err != nil {
		return err
	}
	completedAt := o.clock.Now().UTC()
	record := dailyConsolidationFromReport(
		utcDate, DailyConsolidationCompleted, completedAt, &report,
	)
	if err := o.dailyConsolidations.SaveDailyConsolidation(ctx, record); err != nil {
		return fmt.Errorf("save completed daily consolidation: %w", err)
	}
	o.info(ctx, "daily consolidation recorded",
		slog.String("event", "funding_daily_consolidation_recorded"),
		slog.String("utc_date", utcDate),
		slog.Time("completed_at", completedAt),
		slog.Int("builder_count", len(record.Builders)))
	return nil
}

func dailyConsolidationFromReport(
	utcDate string,
	status DailyConsolidationStatus,
	completedAt time.Time,
	report *RunReport,
) DailyConsolidation {
	record := DailyConsolidation{
		UTCDate: utcDate, Status: status, CompletedAt: completedAt,
	}
	if report == nil {
		return record
	}
	record.StartedAt = report.StartedAt
	record.ClaimablesCapturedAt = report.ClaimablesCapturedAt
	record.Builders = make([]DailyConsolidationBuilder, len(report.Builders))
	for index, builder := range report.Builders {
		record.Builders[index] = DailyConsolidationBuilder{
			Address: builder.Address, ClaimableUSDC: builder.ClaimableUSDC,
			ClaimableObserved:      builder.ClaimableObserved,
			ClaimableQueryAttempts: builder.ClaimableQueryAttempts,
			ClaimAttempts:          builder.ClaimAttempts, ClaimUncertain: builder.ClaimUncertain,
			ClaimStatus: builder.ClaimStatus, ClaimSummary: builder.ClaimSummary,
			SweepAmount: builder.SweepAmount,
			SweepStatus: builder.SweepStatus, SweepSummary: builder.SweepSummary,
		}
	}
	return record
}

func (o *Orchestrator) attachDailyConsolidation(
	ctx context.Context,
	report *RunReport,
	utcDate string,
) {
	if o == nil || report == nil || o.dailyConsolidations == nil {
		return
	}
	summary := DailyConsolidationReport{
		UTCDate: utcDate, BuilderCount: len(report.Builders),
	}
	record, err := o.dailyConsolidations.LoadDailyConsolidation(ctx, utcDate)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		summary.LoadFailed = true
		report.DailyConsolidation = summary
		o.markMissingDailyConsolidation(report, "UTC 00:00 归集记录读取失败")
		o.warn(ctx, "daily consolidation load failed",
			slog.String("event", "funding_daily_consolidation_load_failed"),
			slog.String("utc_date", utcDate))
		return
	}
	if record == nil {
		summary.Missing = true
		report.DailyConsolidation = summary
		o.markMissingDailyConsolidation(report, "未找到 UTC 00:00 归集记录")
		return
	}

	summary.Observed = true
	summary.Status = record.Status
	summary.ClaimablesCapturedAt = record.ClaimablesCapturedAt
	summary.CompletedAt = record.CompletedAt
	entries := make(map[string]DailyConsolidationBuilder, len(record.Builders))
	for _, builder := range record.Builders {
		entries[strings.ToLower(strings.TrimSpace(builder.Address))] = builder
	}
	total := decimal.Zero
	for index := range report.Builders {
		builder := &report.Builders[index]
		entry, found := entries[strings.ToLower(strings.TrimSpace(builder.Address))]
		if !found {
			continue
		}
		summary.RecordedBuilders++
		builder.ClaimableUSDC = entry.ClaimableUSDC
		builder.ClaimableObserved = entry.ClaimableObserved
		builder.ClaimableQueryAttempts = entry.ClaimableQueryAttempts
		builder.ClaimAttempts = entry.ClaimAttempts
		builder.ClaimUncertain = entry.ClaimUncertain
		builder.ClaimStatus = entry.ClaimStatus
		builder.ClaimSummary = entry.ClaimSummary
		builder.SweepAmount = entry.SweepAmount
		builder.SweepStatus = entry.SweepStatus
		builder.SweepSummary = entry.SweepSummary
		if record.Status != DailyConsolidationCompleted {
			markIncompleteBuilderActions(builder)
		}
		if entry.ClaimableObserved {
			summary.ObservedBuilders++
			claimable, parseErr := decimal.NewFromString(entry.ClaimableUSDC)
			if parseErr == nil && !claimable.IsNegative() {
				total = total.Add(claimable)
			}
		} else {
			report.addWarning(builder.Name + "：零点 Claimable 查询失败")
		}
		if entry.ClaimUncertain {
			summary.UncertainClaims++
		}
		if builder.ClaimStatus == ReportStepWarning {
			report.addWarning(builder.Name + "：" + fallbackSummary(builder.ClaimSummary, "Reward 领取存在告警"))
		}
		if builder.SweepStatus == ReportStepWarning {
			report.addWarning(builder.Name + "：" + fallbackSummary(builder.SweepSummary, "资金归集存在告警"))
		}
	}
	summary.TotalClaimableUSDC = total.String()
	report.DailyConsolidation = summary
	report.setStage("rewards", aggregateBuilderStep(report.Builders, true), builderStepSummary(report.Builders, true))
	report.setStage("sweep", aggregateBuilderStep(report.Builders, false), builderStepSummary(report.Builders, false))
	if report.Trigger != TriggerUTC {
		return
	}
	if summary.RecordedBuilders != summary.BuilderCount {
		report.addWarning(fmt.Sprintf(
			"UTC 00:00 归集记录仅包含 %d/%d 个 Builder",
			summary.RecordedBuilders, summary.BuilderCount,
		))
	}
	if summary.ObservedBuilders != summary.BuilderCount {
		report.addWarning(fmt.Sprintf(
			"UTC 00:00 Claimable 仅成功查询 %d/%d 个 Builder，当日对账数据不完整",
			summary.ObservedBuilders, summary.BuilderCount,
		))
	}
	if summary.Status != DailyConsolidationCompleted {
		report.addWarning("UTC 00:00 Claimable 已保存，但 claim 与资金归集任务未完整结束")
	}
	if summary.UncertainClaims > 0 {
		report.addWarning(fmt.Sprintf(
			"UTC 00:00 有 %d 个 Builder 的 claim 在有限重试后结果仍不确定，请人工核查",
			summary.UncertainClaims,
		))
	}
}

func markIncompleteBuilderActions(builder *BuilderReport) {
	if builder == nil {
		return
	}
	const summary = "零点任务在执行完成前中断"
	if builder.ClaimStatus == ReportStepPending {
		builder.ClaimStatus = ReportStepWarning
		builder.ClaimSummary = summary
	}
	if builder.SweepStatus == ReportStepPending {
		builder.SweepStatus = ReportStepWarning
		builder.SweepSummary = summary
	}
}

func (o *Orchestrator) markMissingDailyConsolidation(report *RunReport, warning string) {
	const summary = "UTC 00:00 归集记录不可用"
	report.setStage("rewards", ReportStepSkipped, summary)
	report.setStage("sweep", ReportStepSkipped, summary)
	markBuilderStepsSkipped(report, summary)
	if report.Trigger == TriggerUTC {
		report.addWarning(warning)
	}
}

func fallbackSummary(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
