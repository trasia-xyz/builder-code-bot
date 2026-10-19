package fundingreport

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/notification"

	"github.com/shopspring/decimal"
)

type view struct {
	StatusLabel        string
	StatusColor        string
	StatusTint         string
	Subject            string
	Preheader          string
	Compact            bool
	Duration           string
	Builders           []builderView
	SingleBuilder      *builderView
	DailyConsolidation dailyConsolidationView
	Settlement         settlementView
	Payouts            []payoutView
	Warnings           []string
	ShowFailure        bool
	FailureStage       string
	FailureSummary     string
	NextAction         string
	RunID              string
	Trigger            string
	Execution          string
	Network            string
	Phase              string
	FinishedAt         string
}

type builderView struct {
	Name             string
	Address          string
	Claimable        string
	ClaimableQueries string
	Claim            string
	ClaimColor       string
	Sweep            string
	SweepColor       string
	RateLimit        string
	RateColor        string
	RateIssue        bool
}

type dailyConsolidationView struct {
	Show            bool
	Status          string
	Color           string
	Tint            string
	UTCDate         string
	Total           string
	TotalLabel      string
	Coverage        string
	Difference      string
	DifferenceColor string
}

type payoutView struct {
	ShowDiagnostics bool
	Name            string
	RecordCount     string
	RawTotal        string
	Amount          string
	TotalBefore     string
	FirstSubmit     string
	ExactReplay     string
	Evidence        string
	Nonce           string
	Status          string
	StatusColor     string
	Database        string
	DatabaseColor   string
}

type settlementView struct {
	ShowBalances  bool
	ShowBreakdown bool
	RateIssue     bool
	Total         string
	Hold          string
	Available     string
	Status        string
	Color         string
	Tint          string
	RateLimit     string
	RateColor     string
}

func Render(report funding.RunReport) notification.Message {
	data := buildView(report)
	var body bytes.Buffer
	if err := reportTemplate.Execute(&body, data); err != nil {
		return notification.Message{
			Status:  notification.StatusCritical,
			Subject: "资金任务报告生成失败",
			Body:    "资金任务已经结束，但 HTML 报告生成失败。请检查服务日志。",
		}
	}
	return notification.Message{
		Status:   notificationStatus(report.Status),
		Subject:  data.Subject,
		HTMLBody: body.String(),
	}
}

func buildView(report funding.RunReport) view {
	statusLabel, statusColor, statusTint := reportStatus(report.Status)
	if report.Outcome == "no_data" && report.Status == funding.ReportStatusWarning {
		statusLabel = "无数据但有告警"
	} else if report.Outcome == "retry_exhausted" {
		statusLabel = "自动重试已耗尽"
	}
	duration := report.FinishedAt.Sub(report.StartedAt)
	if duration < 0 {
		duration = 0
	}
	data := view{
		StatusLabel:        statusLabel,
		StatusColor:        statusColor,
		StatusTint:         statusTint,
		Subject:            subject(report, statusLabel),
		Preheader:          preheader(report, duration),
		Compact:            report.Compact,
		Duration:           formatDuration(duration),
		Warnings:           append([]string(nil), report.Warnings...),
		ShowFailure:        report.FailureSummary != "",
		FailureStage:       stageLabel(report.FailureStage),
		FailureSummary:     report.FailureSummary,
		NextAction:         report.NextAction,
		RunID:              fallback(report.RunID, "—"),
		Trigger:            triggerLabel(report.Trigger),
		Execution:          executionLabel(report),
		Network:            fallback(strings.ToUpper(report.Network), "—"),
		Phase:              fallback(string(report.Phase), "—"),
		FinishedAt:         formatTime(report.FinishedAt),
		Settlement:         buildSettlementView(report),
		DailyConsolidation: buildDailyConsolidationView(report),
	}

	for _, builder := range report.Builders {
		claimText, claimColor := builderStep(builder.ClaimStatus, builder.ClaimSummary)
		if builder.ClaimAttempts > 1 {
			claimText += fmt.Sprintf(" · 提交 %d 次", builder.ClaimAttempts)
		}
		sweepText, sweepColor := builderStep(builder.SweepStatus, builder.SweepSummary)
		if builder.SweepAmount != "" {
			sweepText = builder.SweepAmount + " USDC · " + sweepText
		}
		rateText, rateColor := "未观察", "#64748b"
		switch {
		case builder.RateLimitQueryFailed:
			rateText, rateColor = "查询失败", "#dc2626"
		case builder.RateLimitVisibilityUnconfirmed:
			rateText = fmt.Sprintf("最低观测 %d · 刷新未确认", builder.RateLimitRemaining)
			rateColor = "#d97706"
		case builder.RateLimitObserved:
			rateText = fmt.Sprintf("剩余 %d", builder.RateLimitRemaining)
			if builder.RateLimitRemaining < 200 {
				rateColor = "#d97706"
			} else {
				rateColor = "#15803d"
			}
		}
		claimableText := displayClaimable(builder.ClaimableUSDC)
		if builder.ClaimableQueryAttempts > 1 {
			claimableText += fmt.Sprintf(" · 查询 %d 次", builder.ClaimableQueryAttempts)
		}
		data.Builders = append(data.Builders, builderView{
			Name: fallback(builder.Name, "Builder"), Address: shortAddress(builder.Address),
			Claimable: claimableText,
			Claim:     claimText, ClaimColor: claimColor,
			Sweep: sweepText, SweepColor: sweepColor,
			RateLimit: rateText, RateColor: rateColor,
			RateIssue: builder.RateLimitQueryFailed || builder.RateLimitVisibilityUnconfirmed ||
				(builder.RateLimitObserved && builder.RateLimitRemaining < 200),
		})
		if builder.ClaimableQueryAttempts > 1 {
			data.Builders[len(data.Builders)-1].ClaimableQueries = fmt.Sprintf("Claimable 查询 %d 次", builder.ClaimableQueryAttempts)
		}
	}
	if !report.Compact && len(data.Builders) == 1 {
		data.SingleBuilder = &data.Builders[0]
	}

	for _, target := range report.Payouts {
		if !target.RecordsRead && target.Payout.Amount == "" &&
			target.Payout.Status == funding.ReportStepPending {
			continue
		}
		payoutStatus, payoutColor := builderStep(target.Payout.Status, "")
		databaseStatus, databaseColor := builderStep(target.DatabaseStatus, "")
		item := payoutView{
			ShowDiagnostics: (report.Status != funding.ReportStatusSuccess && report.Status != funding.ReportStatusNoData) ||
				report.Recovery || target.Payout.SubmitAttempts > 1,
			Name:          payoutName(target.Kind),
			RecordCount:   fmt.Sprintf("%d", target.RecordCount),
			RawTotal:      displayAmount(target.RawTotal),
			Amount:        displayAmount(target.Payout.Amount),
			TotalBefore:   displayAmount(target.Payout.SettlementTotalBefore),
			FirstSubmit:   firstSubmitLabel(target.Payout.SubmitAttempts),
			ExactReplay:   exactReplayLabel(target.Payout.SubmitAttempts),
			Evidence:      evidenceLabel(target.Payout.ConfirmationEvidence, target.Payout.Blocked),
			Status:        payoutStatus,
			StatusColor:   payoutColor,
			Database:      databaseStatus,
			DatabaseColor: databaseColor,
		}
		if target.Payout.Nonce != 0 {
			item.Nonce = fmt.Sprintf("%d", target.Payout.Nonce)
		} else {
			item.Nonce = "—"
		}
		data.Payouts = append(data.Payouts, item)
	}
	return data
}

func buildDailyConsolidationView(report funding.RunReport) dailyConsolidationView {
	consolidation := report.DailyConsolidation
	if consolidation.UTCDate == "" && !consolidation.Observed &&
		!consolidation.Missing && !consolidation.LoadFailed {
		return dailyConsolidationView{}
	}
	data := dailyConsolidationView{
		Show:            true,
		Status:          "未记录",
		Color:           "#b45309",
		Tint:            "#fffbeb",
		UTCDate:         fallback(consolidation.UTCDate, "—"),
		Total:           displayAmount(consolidation.TotalClaimableUSDC),
		TotalLabel:      "零点 Claimable 合计",
		Coverage:        fmt.Sprintf("%d/%d", consolidation.ObservedBuilders, consolidation.BuilderCount),
		Difference:      "—",
		DifferenceColor: "#64748b",
	}
	if consolidation.Observed && consolidation.ObservedBuilders > 0 &&
		consolidation.ObservedBuilders != consolidation.BuilderCount {
		data.TotalLabel = "已观测 Claimable 小计"
	}
	switch {
	case consolidation.LoadFailed:
		data.Status, data.Color, data.Tint = "读取失败", "#b91c1c", "#fef2f2"
	case consolidation.Missing:
		data.Status, data.Color, data.Tint = "未记录", "#b45309", "#fffbeb"
	case consolidation.Observed && consolidation.ObservedBuilders != consolidation.BuilderCount:
		data.Status, data.Color, data.Tint = "零点数据不完整", "#b45309", "#fffbeb"
	case consolidation.Observed && consolidation.Status != funding.DailyConsolidationCompleted:
		data.Status, data.Color, data.Tint = "Claimable 已记录，归集未完成", "#b45309", "#fffbeb"
	case consolidation.Observed && consolidation.BelowPayout:
		data.Status, data.Color, data.Tint = "完整但低于 Payout", "#b91c1c", "#fef2f2"
	case consolidation.Observed && consolidation.UncertainClaims > 0:
		data.Status, data.Color, data.Tint = "对账完整，Claim 结果不确定", "#b45309", "#fffbeb"
	case consolidation.Observed && consolidation.CoversPayout:
		data.Status, data.Color, data.Tint = "完整，Claimable ≥ Payout", "#15803d", "#ecfdf3"
	case consolidation.Observed && consolidation.ObservedBuilders == consolidation.BuilderCount:
		data.Status, data.Color, data.Tint = "完整", "#15803d", "#ecfdf3"
	case consolidation.Observed:
		data.Status, data.Color, data.Tint = "记录不完整", "#b45309", "#fffbeb"
	}
	if income, ok := reportIncome(report); ok {
		data.Difference = income.String()
		if income.IsNegative() {
			data.DifferenceColor = "#b91c1c"
		} else {
			data.DifferenceColor = "#15803d"
		}
	}
	return data
}

func reportIncome(report funding.RunReport) (decimal.Decimal, bool) {
	consolidation := report.DailyConsolidation
	if !report.RecordsRead || !consolidation.Observed || consolidation.Missing || consolidation.LoadFailed ||
		consolidation.BuilderCount <= 0 || consolidation.RecordedBuilders != consolidation.BuilderCount ||
		consolidation.ObservedBuilders != consolidation.BuilderCount {
		return decimal.Zero, false
	}
	claimable, claimableErr := decimal.NewFromString(consolidation.TotalClaimableUSDC)
	payout, payoutErr := decimal.NewFromString(report.PayoutTotal)
	if claimableErr != nil || payoutErr != nil || claimable.IsNegative() || payout.IsNegative() {
		return decimal.Zero, false
	}
	return claimable.Sub(payout), true
}

func subject(report funding.RunReport, statusLabel string) string {
	var parts []string
	if report.Status != funding.ReportStatusSuccess {
		if report.Status == funding.ReportStatusRetrying && report.Attempt > 0 && report.MaxAttempts > 0 {
			statusLabel = fmt.Sprintf("第 %d/%d 次执行失败 · 等待重试", report.Attempt, report.MaxAttempts)
		}
		parts = append(parts, statusLabel)
	}
	if income, ok := reportIncome(report); ok {
		parts = append(parts, "收入 "+income.StringFixed(2)+" USDC")
	}
	if len(parts) == 0 {
		return "收入 — USDC"
	}
	return strings.Join(parts, " · ")
}

func preheader(report funding.RunReport, duration time.Duration) string {
	if report.FailureSummary != "" {
		return strings.Join(nonempty(
			report.FailureSummary,
			settlementBalanceSummary(report.SettlementBalance),
			attemptSummary(report),
			report.NextAction,
		), " · ")
	}
	if report.Outcome == "no_data" {
		return strings.Join(nonempty(
			"本轮没有待处理 funding records，无需执行链上操作。",
			settlementBalanceSummary(report.SettlementBalance),
		), " · ")
	}
	return strings.Join(nonempty(
		recordCount(report)+" 条记录",
		settlementBalanceSummary(report.SettlementBalance),
		"耗时 "+formatDuration(duration),
	), " · ")
}

func buildSettlementView(report funding.RunReport) settlementView {
	balance := report.SettlementBalance
	data := settlementView{
		ShowBalances: balance.Observed && !balance.QueryFailed && !balance.PostPayoutUnconfirmed,
		RateIssue: report.SettlementRateLimitQueryFailed || report.SettlementRateLimitVisibilityUnconfirmed ||
			(report.SettlementRateLimitObserved && report.SettlementRateLimitRemaining < 200),
		Total: displayAmount(balance.Total), Hold: displayAmount(balance.Hold),
		Available: displayAmount(balance.Available),
		Status:    "未观察", Color: "#64748b", Tint: "#f8fafc",
		RateLimit: "未观察", RateColor: "#64748b",
	}
	if hold, err := decimal.NewFromString(balance.Hold); err == nil && !hold.IsZero() {
		data.ShowBreakdown = data.ShowBalances
	}
	switch {
	case balance.PostPayoutUnconfirmed:
		data.Status, data.Color, data.Tint = "Payout 后余额未确认", "#b45309", "#fffbeb"
	case balance.QueryFailed:
		data.Status, data.Color, data.Tint = "查询失败", "#b91c1c", "#fef2f2"
	case balance.Observed:
		data.Status, data.Color, data.Tint = "已观察", "#15803d", "#ecfdf3"
	}
	switch {
	case report.SettlementRateLimitQueryFailed:
		data.RateLimit, data.RateColor = "查询失败", "#dc2626"
	case report.SettlementRateLimitVisibilityUnconfirmed:
		data.RateLimit = fmt.Sprintf(
			"最低观测 %d · 刷新未确认", report.SettlementRateLimitRemaining,
		)
		data.RateColor = "#d97706"
	case report.SettlementRateLimitObserved:
		data.RateLimit = fmt.Sprintf("剩余 %d", report.SettlementRateLimitRemaining)
		if report.SettlementRateLimitRemaining < 200 {
			data.RateColor = "#d97706"
		} else {
			data.RateColor = "#15803d"
		}
	}
	return data
}

func settlementBalanceSummary(balance funding.SettlementBalanceReport) string {
	switch {
	case balance.PostPayoutUnconfirmed:
		return "Settlement Payout 后余额未确认"
	case balance.QueryFailed:
		return "Settlement 结束余额查询失败"
	case balance.Observed:
		return "Settlement available " + displayAmount(balance.Available) + " USDC"
	default:
		return ""
	}
}

func reportStatus(status funding.ReportStatus) (label, color, tint string) {
	switch status {
	case funding.ReportStatusSuccess:
		return "已完成", "#15803d", "#ecfdf3"
	case funding.ReportStatusWarning:
		return "成功但有告警", "#b45309", "#fffbeb"
	case funding.ReportStatusNoData:
		return "无待处理数据", "#2563eb", "#eff6ff"
	case funding.ReportStatusRetrying:
		return "等待自动重试", "#c2410c", "#fff7ed"
	default:
		return "需要人工处理", "#b91c1c", "#fef2f2"
	}
}

func notificationStatus(status funding.ReportStatus) notification.Status {
	switch status {
	case funding.ReportStatusSuccess:
		return notification.StatusSuccess
	case funding.ReportStatusWarning:
		return notification.StatusWarning
	case funding.ReportStatusNoData:
		return notification.StatusInfo
	case funding.ReportStatusRetrying:
		return notification.StatusRetrying
	default:
		return notification.StatusCritical
	}
}

func builderStep(status funding.ReportStepStatus, summary string) (string, string) {
	if summary != "" {
		switch status {
		case funding.ReportStepSuccess:
			return summary, "#15803d"
		case funding.ReportStepWarning, funding.ReportStepFailed:
			return summary, "#b45309"
		default:
			return summary, "#64748b"
		}
	}
	switch status {
	case funding.ReportStepSuccess:
		return "已完成", "#15803d"
	case funding.ReportStepWarning:
		return "有告警", "#b45309"
	case funding.ReportStepSkipped:
		return "已跳过", "#64748b"
	case funding.ReportStepFailed:
		return "失败", "#b91c1c"
	default:
		return "未观察", "#64748b"
	}
}

func triggerLabel(trigger funding.Trigger) string {
	switch trigger {
	case funding.TriggerUTC:
		return "UTC 定时任务"
	case funding.TriggerRunOnStart:
		return "启动时执行"
	default:
		return fallback(string(trigger), "—")
	}
}

func executionLabel(report funding.RunReport) string {
	if report.Recovery {
		return "恢复执行"
	}
	if report.Attempt > 0 && report.MaxAttempts > 0 {
		return fmt.Sprintf("第 %d/%d 次尝试", report.Attempt, report.MaxAttempts)
	}
	return "新运行"
}

func attemptSummary(report funding.RunReport) string {
	if report.Attempt <= 0 || report.MaxAttempts <= 0 {
		return ""
	}
	return fmt.Sprintf("第 %d/%d 次尝试", report.Attempt, report.MaxAttempts)
}

func stageLabel(key string) string {
	switch key {
	case "snapshot":
		return "数据快照"
	case "rewards":
		return "Reward"
	case "sweep":
		return "资金归集"
	case "referral_payout":
		return "Referral Payout"
	case "referral_database":
		return "Referral 数据库"
	case "finalize":
		return "状态归档"
	default:
		return fallback(key, "任务执行")
	}
}

func payoutName(kind funding.PayoutKind) string {
	switch kind {
	case funding.PayoutKindReferral:
		return "Referral Payout"
	default:
		return fallback(string(kind), "Payout")
	}
}

func firstSubmitLabel(attempts int) string {
	if attempts > 0 {
		return "已授权 / 可能已发送"
	}
	return "未授权"
}

func exactReplayLabel(attempts int) string {
	if attempts > 1 {
		return "已授权 / 可能已发送（原请求 / 相同 nonce）"
	}
	return "未授权"
}

func evidenceLabel(evidence string, blocked bool) string {
	switch evidence {
	case "exchange_response":
		return "Exchange 明确确认"
	case "settlement_balance_decreased":
		return "Settlement 精确减少 amount"
	case "settlement_balance_decreased_with_activation_fee":
		return "Settlement 精确减少 amount + 1 USDC（新地址费用）"
	default:
		if blocked {
			return "发送方余额未能确认（blocked）"
		}
		return fallback(evidence, "—")
	}
}

func recordCount(report funding.RunReport) string {
	if !report.RecordsRead {
		return "—"
	}
	return fmt.Sprintf("%d", report.RecordCount)
}

func displayAmount(value string) string {
	return fallback(value, "—")
}

func displayClaimable(value string) string {
	if value == "" {
		return "—"
	}
	return value + " USDC"
}

func shortAddress(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 16 {
		return fallback(value, "—")
	}
	return value[:8] + "…" + value[len(value)-6:]
}

func formatDuration(duration time.Duration) string {
	if duration < time.Second {
		return "<1 秒"
	}
	if duration < time.Minute {
		return fmt.Sprintf("%.1f 秒", duration.Seconds())
	}
	return duration.Round(time.Second).String()
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.UTC().Format("2006-01-02 15:04:05 UTC")
}

func fallback(value, replacement string) string {
	if strings.TrimSpace(value) == "" {
		return replacement
	}
	return strings.TrimSpace(value)
}

func nonempty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

var reportTemplate = template.Must(template.New("funding-report").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <style>
    @media only screen and (max-width:620px) {
      .container { width:100% !important; }
      .pad { padding-left:16px !important; padding-right:16px !important; }
      .builder-table { font-size:12px !important; }
    }
  </style>
</head>
<body style="margin:0;padding:0;background:#f3f5f8;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
  <div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent;">{{.Preheader}}&#847;&zwnj;&#8199;&#65279;&#847;&zwnj;&#8199;&#65279;</div>
  <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="background:#f3f5f8;">
    <tr><td align="center" style="padding:20px 10px;">
      <table role="presentation" class="container" width="680" cellspacing="0" cellpadding="0" border="0" style="width:680px;max-width:680px;background:#ffffff;border:1px solid #e2e8f0;border-radius:14px;overflow:hidden;">
        <tr><td class="pad" style="padding:22px 30px;background:{{.StatusTint}};">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0">
            <tr>
              <td style="font-size:20px;font-weight:700;color:#0f172a;">资金报告</td>
              <td align="right" style="font-size:14px;font-weight:700;color:{{.StatusColor}};">{{.StatusLabel}}</td>
            </tr>
          </table>
        </td></tr>

        {{if .ShowFailure}}
        <tr><td class="pad" style="padding:20px 30px;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:{{.StatusTint}};border:1px solid #e2e8f0;border-radius:10px;">
            <tr><td style="padding:16px;font-size:14px;line-height:1.6;">
              <strong style="color:{{.StatusColor}};">{{.FailureStage}} · {{.FailureSummary}}</strong>
              {{if .NextAction}}<div style="margin-top:8px;color:#475569;">{{.NextAction}}</div>{{end}}
            </td></tr>
          </table>
        </td></tr>
        {{end}}

        {{if or .DailyConsolidation.Show .SingleBuilder}}
        <tr><td class="pad" style="padding:20px 30px;">
          <div style="font-size:15px;font-weight:700;color:#0f172a;">归集对账{{if .DailyConsolidation.Show}} <span style="font-size:12px;font-weight:400;color:#64748b;">{{.DailyConsolidation.UTCDate}} UTC 00:00</span>{{end}}</div>
          {{if .DailyConsolidation.Show}}
          <div style="margin-top:12px;font-size:12px;color:#64748b;">收入（Claimable - Payout）</div>
          <div style="margin-top:2px;font-size:28px;font-weight:700;color:{{.DailyConsolidation.DifferenceColor}};">{{.DailyConsolidation.Difference}} <span style="font-size:13px;font-weight:400;color:#64748b;">USDC</span></div>
          <div style="margin-top:8px;font-size:13px;color:{{.DailyConsolidation.Color}};">{{.DailyConsolidation.Status}} · Builder 覆盖 {{.DailyConsolidation.Coverage}}</div>
          <div style="margin-top:8px;font-size:13px;color:#64748b;">{{.DailyConsolidation.TotalLabel}} · {{.DailyConsolidation.Total}} USDC</div>
          {{end}}
          {{with .SingleBuilder}}
          <div style="margin-top:12px;padding-top:10px;border-top:1px solid #edf1f5;font-size:12px;line-height:1.6;">
            <div style="color:#64748b;">{{.Name}} · {{.Address}}{{if .ClaimableQueries}} · {{.ClaimableQueries}}{{end}}</div>
            <div><span style="color:{{.ClaimColor}};">00:00 Reward · {{.Claim}}</span><span style="color:{{.SweepColor}};"> · Sweep {{.Sweep}}</span></div>
            {{if .RateIssue}}<div style="color:{{.RateColor}};">Rate Limit · {{.RateLimit}}</div>{{end}}
          </div>
          {{end}}
        </td></tr>
        {{end}}

        {{if not .Compact}}
        {{if gt (len .Builders) 1}}
        <tr><td class="pad" style="padding:0 30px 20px;">
          <div style="margin:8px 0;font-size:14px;font-weight:700;color:#0f172a;">Builder 明细</div>
          <table class="builder-table" width="100%" cellspacing="0" cellpadding="0" border="0" style="font-size:13px;border-collapse:collapse;">
            <tr style="background:#f8fafc;color:#64748b;">
              <th align="left" style="padding:8px;">Builder</th>
              <th align="left" style="padding:8px;">00:00 Reward</th>
              <th align="left" style="padding:8px;">Sweep</th>
            </tr>
            {{range .Builders}}
            <tr>
              <td valign="top" style="padding:10px 8px;border-bottom:1px solid #edf1f5;"><strong style="color:#334155;">{{.Name}}</strong><br><span style="font-size:11px;color:#94a3b8;">{{.Address}}</span>{{if .RateIssue}}<br><span style="font-size:11px;color:{{.RateColor}};">Rate Limit · {{.RateLimit}}</span>{{end}}</td>
              <td valign="top" style="padding:10px 8px;border-bottom:1px solid #edf1f5;"><span style="color:#64748b;">{{.Claimable}}</span><br><span style="color:{{.ClaimColor}};">{{.Claim}}</span></td>
              <td valign="top" style="padding:10px 8px;border-bottom:1px solid #edf1f5;color:{{.SweepColor}};">{{.Sweep}}</td>
            </tr>
            {{end}}
          </table>
        </td></tr>
        {{end}}
        {{end}}

        {{if not .Compact}}
        {{if .Payouts}}
        <tr><td class="pad" style="padding:20px 30px 8px;">
          <div style="font-size:15px;font-weight:700;color:#0f172a;">Payout 明细</div>
        </td></tr>
        {{range .Payouts}}
        <tr><td class="pad" style="padding:0 30px 20px;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:13px;line-height:1.6;background:#f8fafc;border-radius:10px;">
            <tr><td colspan="2" style="padding:14px 16px 10px;">
              <div style="font-size:12px;color:#64748b;">{{.Name}}</div>
              <div style="font-size:18px;font-weight:700;color:#0f172a;">{{.Amount}} <span style="font-size:13px;font-weight:400;color:#64748b;">USDC · {{.RecordCount}} 条记录</span></div>
            </td></tr>
            <tr><td colspan="2" style="padding:0 16px 10px;">
              <span style="color:#64748b;">付款状态 </span><strong style="color:{{.StatusColor}};">{{.Status}}</strong>
              <span style="color:#64748b;"> · 数据库状态 </span><strong style="color:{{.DatabaseColor}};">{{.Database}}</strong>
            </td></tr>
            {{if .ShowDiagnostics}}
            <tr><td style="padding:0 16px 10px;color:#64748b;">Raw total</td><td style="padding:0 16px 10px;">{{.RawTotal}} USDC</td></tr>
            <tr><td style="padding:0 16px 10px;color:#64748b;">付款前 total</td><td style="padding:0 16px 10px;">{{.TotalBefore}} USDC</td></tr>
            <tr><td style="padding:0 16px 10px;color:#64748b;">首次 Submit</td><td style="padding:0 16px 10px;">{{.FirstSubmit}}</td></tr>
            <tr><td style="padding:0 16px 10px;color:#64748b;">Exact replay</td><td style="padding:0 16px 10px;">{{.ExactReplay}}</td></tr>
            <tr><td style="padding:0 16px 10px;color:#64748b;">确认依据</td><td style="padding:0 16px 10px;">{{.Evidence}}</td></tr>
            <tr><td style="padding:0 16px 14px;color:#64748b;">Nonce</td><td style="padding:0 16px 14px;">{{.Nonce}}</td></tr>
            {{end}}
            <tr><td colspan="2" style="padding:0 16px 14px;">{{template "settlement-summary" $.Settlement}}</td></tr>
          </table>
        </td></tr>
        {{end}}
        {{end}}
        {{end}}

        {{if or .Compact (not .Payouts)}}
        <tr><td class="pad" style="padding:0 30px 20px;">
          {{if .Compact}}{{template "compact-payout-summary" .Payouts}}{{end}}
          <div style="margin-top:10px;">{{template "settlement-summary" .Settlement}}</div>
        </td></tr>
        {{end}}

        {{if .Warnings}}
        <tr><td class="pad" style="padding:18px 30px;border-top:1px solid #e5e9f0;">
          <div style="font-size:14px;font-weight:700;color:#92400e;">告警</div>
          <ul style="margin:8px 0 0;padding-left:20px;color:#92400e;font-size:13px;line-height:1.6;">{{range .Warnings}}<li>{{.}}</li>{{end}}</ul>
        </td></tr>
        {{end}}

        <tr><td class="pad" style="padding:16px 30px;background:#f8fafc;border-top:1px solid #e5e9f0;font-size:11px;line-height:1.8;color:#64748b;">
          <div>{{.Network}} · {{.FinishedAt}} · 耗时 {{.Duration}}</div>
          <div style="word-break:break-all;">{{.Trigger}} · {{.Execution}} · Run ID: {{.RunID}}{{if .ShowFailure}} · Phase: {{.Phase}}{{end}}</div>
        </td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>{{define "compact-payout-summary"}}{{range .}}
<div style="margin-top:8px;font-size:13px;line-height:1.6;">
  <strong style="color:#0f172a;">Payout {{.Amount}} USDC</strong>
  <span style="color:#64748b;"> · 付款状态 </span><span style="color:{{.StatusColor}};">{{.Status}}</span>
  <span style="color:#64748b;"> · 数据库状态 </span><span style="color:{{.DatabaseColor}};">{{.Database}}</span>
</div>
{{end}}{{end}}{{define "settlement-summary"}}
<div style="font-size:13px;line-height:1.6;">
  <span style="color:#64748b;">Settlement 结束余额 · </span><span style="color:{{.Color}};">{{if .ShowBalances}}Available {{.Available}} USDC{{else}}{{.Status}}{{end}}</span>
</div>
{{if .ShowBreakdown}}<div style="margin-top:3px;font-size:12px;color:#64748b;">Total {{.Total}} USDC · Hold {{.Hold}} USDC</div>{{end}}
{{if .RateIssue}}<div style="margin-top:3px;font-size:12px;color:{{.RateColor}};">Rate Limit · {{.RateLimit}}</div>{{end}}
{{end}}`))
