package fundingreport

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"time"

	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/notification"
)

type view struct {
	StatusLabel    string
	StatusColor    string
	StatusTint     string
	Subject        string
	Preheader      string
	Compact        bool
	PayoutTotal    string
	RawTotal       string
	RecordCount    string
	BuilderCount   int
	Duration       string
	Stages         []stageView
	Builders       []builderView
	Settlement     settlementView
	ShowPayout     bool
	Payout         payoutView
	Warnings       []string
	ShowFailure    bool
	FailureStage   string
	FailureSummary string
	NextAction     string
	RunID          string
	Trigger        string
	Execution      string
	Network        string
	UTCDate        string
	Phase          string
	StartedAt      string
	FinishedAt     string
}

type stageView struct {
	Label   string
	Summary string
	Symbol  string
	Color   string
	Tint    string
}

type builderView struct {
	Name       string
	Address    string
	Claimable  string
	Claim      string
	ClaimColor string
	Sweep      string
	SweepColor string
	RateLimit  string
	RateColor  string
}

type payoutView struct {
	Amount      string
	Settlement  string
	Recipient   string
	TotalBefore string
	Evidence    string
	RequestHash string
	Nonce       string
	Status      string
	StatusColor string
	RateLimit   string
	RateColor   string
}

type settlementView struct {
	Address   string
	Total     string
	Hold      string
	Available string
	Status    string
	Color     string
	Tint      string
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
		StatusLabel:    statusLabel,
		StatusColor:    statusColor,
		StatusTint:     statusTint,
		Subject:        subject(report, statusLabel),
		Preheader:      preheader(report, duration),
		Compact:        report.Compact,
		PayoutTotal:    displayAmount(report.PayoutTotal),
		RawTotal:       displayAmount(report.RawTotal),
		RecordCount:    recordCount(report),
		BuilderCount:   len(report.Builders),
		Duration:       formatDuration(duration),
		Warnings:       append([]string(nil), report.Warnings...),
		ShowFailure:    report.FailureSummary != "",
		FailureStage:   stageLabel(report.FailureStage),
		FailureSummary: report.FailureSummary,
		NextAction:     report.NextAction,
		RunID:          fallback(report.RunID, "—"),
		Trigger:        triggerLabel(report.Trigger),
		Execution:      executionLabel(report),
		Network:        fallback(strings.ToUpper(report.Network), "—"),
		UTCDate:        fallback(report.UTCDate, "—"),
		Phase:          fallback(string(report.Phase), "—"),
		StartedAt:      formatTime(report.StartedAt),
		FinishedAt:     formatTime(report.FinishedAt),
		Settlement:     buildSettlementView(report.SettlementBalance),
	}

	for _, stage := range report.Stages {
		symbol, color, tint := stepStyle(stage.Status)
		data.Stages = append(data.Stages, stageView{
			Label: stage.Label, Summary: fallback(stage.Summary, "尚未执行"),
			Symbol: symbol, Color: color, Tint: tint,
		})
	}
	for _, builder := range report.Builders {
		claimText, claimColor := builderStep(builder.ClaimStatus, builder.ClaimSummary)
		sweepText, sweepColor := builderStep(builder.SweepStatus, builder.SweepSummary)
		if builder.SweepAmount != "" {
			sweepText = builder.SweepAmount + " USDC · " + sweepText
		}
		rateText, rateColor := "未观察", "#64748b"
		switch {
		case builder.RateLimitQueryFailed:
			rateText, rateColor = "查询失败", "#dc2626"
		case builder.RateLimitObserved:
			rateText = fmt.Sprintf("剩余 %d", builder.RateLimitRemaining)
			if builder.RateLimitRemaining < 200 {
				rateColor = "#d97706"
			} else {
				rateColor = "#15803d"
			}
		}
		data.Builders = append(data.Builders, builderView{
			Name: fallback(builder.Name, "Builder"), Address: shortAddress(builder.Address),
			Claimable: displayClaimable(builder.ClaimableUSDC),
			Claim:     claimText, ClaimColor: claimColor,
			Sweep: sweepText, SweepColor: sweepColor,
			RateLimit: rateText, RateColor: rateColor,
		})
	}

	data.ShowPayout = report.Payout.Amount != "" || report.Payout.Status != funding.ReportStepPending
	if data.ShowPayout {
		payoutStatus, payoutColor := builderStep(report.Payout.Status, "")
		data.Payout = payoutView{
			Amount:      displayAmount(report.Payout.Amount),
			Settlement:  fallback(report.Payout.Settlement, "—"),
			Recipient:   fallback(report.Payout.Recipient, "—"),
			TotalBefore: displayAmount(report.Payout.SettlementTotalBefore),
			Evidence:    evidenceLabel(report.Payout.ConfirmationEvidence),
			RequestHash: fallback(report.Payout.RequestHash, "—"),
			Status:      payoutStatus,
			StatusColor: payoutColor,
			RateLimit:   "未观察",
			RateColor:   "#64748b",
		}
		switch {
		case report.SettlementRateLimitQueryFailed:
			data.Payout.RateLimit, data.Payout.RateColor = "查询失败", "#dc2626"
		case report.SettlementRateLimitObserved:
			data.Payout.RateLimit = fmt.Sprintf("剩余 %d", report.SettlementRateLimitRemaining)
			if report.SettlementRateLimitRemaining < 200 {
				data.Payout.RateColor = "#d97706"
			} else {
				data.Payout.RateColor = "#15803d"
			}
		}
		if report.Payout.Nonce != 0 {
			data.Payout.Nonce = fmt.Sprintf("%d", report.Payout.Nonce)
		} else {
			data.Payout.Nonce = "—"
		}
	}
	return data
}

func subject(report funding.RunReport, statusLabel string) string {
	parts := []string{statusLabel}
	if report.Status == funding.ReportStatusRetrying && report.Attempt > 0 && report.MaxAttempts > 0 {
		parts[0] = fmt.Sprintf("第 %d/%d 次执行失败 · 等待重试", report.Attempt, report.MaxAttempts)
	}
	if report.PayoutTotal != "" && report.PayoutTotal != "0" {
		parts = append(parts, report.PayoutTotal+" USDC")
	}
	if report.RecordsRead && report.RecordCount > 0 {
		parts = append(parts, fmt.Sprintf("%d 条记录", report.RecordCount))
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
	return fmt.Sprintf(
		"Payout %s USDC · %d 条记录 · Settlement available %s · 耗时 %s",
		displayAmount(report.PayoutTotal), report.RecordCount,
		displayAmount(report.SettlementBalance.Available), formatDuration(duration),
	)
}

func buildSettlementView(balance funding.SettlementBalanceReport) settlementView {
	data := settlementView{
		Address: fallback(balance.Address, "—"),
		Total:   displayAmount(balance.Total), Hold: displayAmount(balance.Hold),
		Available: displayAmount(balance.Available),
		Status:    "未观察", Color: "#64748b", Tint: "#f8fafc",
	}
	switch {
	case balance.QueryFailed:
		data.Status, data.Color, data.Tint = "查询失败", "#b91c1c", "#fef2f2"
	case balance.Observed:
		data.Status, data.Color, data.Tint = "已观察", "#15803d", "#ecfdf3"
	}
	return data
}

func settlementBalanceSummary(balance funding.SettlementBalanceReport) string {
	if balance.Observed {
		return "Settlement available " + balance.Available + " USDC"
	}
	if balance.QueryFailed {
		return "Settlement 结束余额查询失败"
	}
	return ""
}

func reportStatus(status funding.ReportStatus) (label, color, tint string) {
	switch status {
	case funding.ReportStatusSuccess:
		return "资金任务成功", "#15803d", "#ecfdf3"
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

func stepStyle(status funding.ReportStepStatus) (symbol, color, tint string) {
	switch status {
	case funding.ReportStepSuccess:
		return "✓", "#15803d", "#ecfdf3"
	case funding.ReportStepWarning:
		return "!", "#b45309", "#fffbeb"
	case funding.ReportStepSkipped:
		return "–", "#64748b", "#f1f5f9"
	case funding.ReportStepFailed:
		return "×", "#b91c1c", "#fef2f2"
	default:
		return "·", "#64748b", "#f1f5f9"
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
	case "payout":
		return "Payout"
	case "database":
		return "数据库"
	default:
		return fallback(key, "任务执行")
	}
}

func evidenceLabel(evidence string) string {
	switch evidence {
	case "exchange_response":
		return "Exchange 明确确认"
	case "settlement_balance_decreased":
		return "Settlement total 下降确认"
	default:
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
      .metric { display:block !important; width:auto !important; border-right:0 !important; border-bottom:1px solid #e5e9f0 !important; }
      .builder-table { font-size:12px !important; }
    }
  </style>
</head>
<body style="margin:0;padding:0;background:#f3f5f8;color:#172033;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;">
  <div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent;">{{.Preheader}}&#847;&zwnj;&#8199;&#65279;&#847;&zwnj;&#8199;&#65279;</div>
  <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0" style="background:#f3f5f8;">
    <tr><td align="center" style="padding:20px 10px;">
      <table role="presentation" class="container" width="680" cellspacing="0" cellpadding="0" border="0" style="width:680px;max-width:680px;background:#ffffff;border:1px solid #e2e8f0;border-radius:16px;overflow:hidden;">
        <tr>
          <td class="pad" style="padding:28px 30px;background:{{.StatusTint}};border-bottom:1px solid #e2e8f0;">
            <div style="font-size:12px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;color:{{.StatusColor}};">BUILDER CODE FUNDING</div>
            <div style="margin-top:8px;font-size:26px;line-height:1.25;font-weight:750;color:{{.StatusColor}};">{{.StatusLabel}}</div>
            <div style="margin-top:8px;font-size:14px;line-height:1.5;color:#475569;">{{.Preheader}}</div>
          </td>
        </tr>

        {{if .Compact}}
        <tr><td class="pad" style="padding:24px 30px;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#fff7ed;border:1px solid #fed7aa;border-radius:12px;">
            <tr><td style="padding:18px;">
              <div style="font-size:13px;font-weight:700;color:#9a3412;">{{.FailureStage}}</div>
              <div style="margin-top:6px;font-size:16px;font-weight:650;color:#7c2d12;">{{.FailureSummary}}</div>
              {{if .NextAction}}<div style="margin-top:10px;font-size:14px;line-height:1.55;color:#9a3412;">{{.NextAction}}</div>{{end}}
            </td></tr>
          </table>
	          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="margin-top:12px;background:{{.Settlement.Tint}};border:1px solid #e2e8f0;border-radius:12px;">
	            <tr><td style="padding:14px 18px;font-size:13px;color:#475569;">
	              <strong style="color:{{.Settlement.Color}};">Settlement 结束余额 · {{.Settlement.Status}}</strong><br>
	              <span style="display:inline-block;margin-top:6px;word-break:break-all;">{{.Settlement.Address}}</span><br>
	              <span style="display:inline-block;margin-top:6px;">Total {{.Settlement.Total}} USDC · Hold {{.Settlement.Hold}} USDC · Available {{.Settlement.Available}} USDC</span>
	            </td></tr>
	          </table>
        </td></tr>
        {{else}}
        <tr><td style="padding:0;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0">
            <tr>
              <td class="metric" width="25%" align="center" style="padding:20px 8px;border-right:1px solid #e5e9f0;">
                <div style="font-size:20px;font-weight:750;color:#0f172a;">{{.PayoutTotal}}</div><div style="margin-top:4px;font-size:11px;color:#64748b;">PAYOUT USDC</div>
              </td>
              <td class="metric" width="25%" align="center" style="padding:20px 8px;border-right:1px solid #e5e9f0;">
                <div style="font-size:20px;font-weight:750;color:#0f172a;">{{.RecordCount}}</div><div style="margin-top:4px;font-size:11px;color:#64748b;">RECORDS</div>
              </td>
              <td class="metric" width="25%" align="center" style="padding:20px 8px;border-right:1px solid #e5e9f0;">
                <div style="font-size:20px;font-weight:750;color:#0f172a;">{{.BuilderCount}}</div><div style="margin-top:4px;font-size:11px;color:#64748b;">BUILDERS</div>
              </td>
              <td class="metric" width="25%" align="center" style="padding:20px 8px;">
                <div style="font-size:20px;font-weight:750;color:#0f172a;">{{.Duration}}</div><div style="margin-top:4px;font-size:11px;color:#64748b;">DURATION</div>
              </td>
            </tr>
          </table>
        </td></tr>

        <tr><td class="pad" style="padding:26px 30px 8px;border-top:1px solid #e5e9f0;">
          <div style="font-size:16px;font-weight:750;color:#0f172a;">执行流程</div>
        </td></tr>
        <tr><td class="pad" style="padding:4px 30px 22px;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" border="0">
            {{range .Stages}}
            <tr>
              <td width="34" valign="top" style="padding:7px 0;">
                <div style="width:24px;height:24px;line-height:24px;text-align:center;border-radius:50%;font-size:13px;font-weight:800;color:{{.Color}};background:{{.Tint}};">{{.Symbol}}</div>
              </td>
              <td style="padding:7px 0;border-bottom:1px solid #f1f5f9;">
                <span style="font-size:14px;font-weight:700;color:#334155;">{{.Label}}</span>
                <span style="margin-left:8px;font-size:13px;color:#64748b;">{{.Summary}}</span>
              </td>
            </tr>
            {{end}}
          </table>
        </td></tr>

        <tr><td class="pad" style="padding:22px 30px 10px;border-top:1px solid #e5e9f0;">
          <div style="font-size:16px;font-weight:750;color:#0f172a;">Settlement 结束余额</div>
        </td></tr>
        <tr><td class="pad" style="padding:4px 30px 24px;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:13px;line-height:1.55;background:{{.Settlement.Tint}};border:1px solid #e2e8f0;border-radius:10px;">
            <tr><td style="padding:14px 16px;color:#64748b;">状态</td><td style="padding:14px 16px;color:{{.Settlement.Color}};font-weight:700;">{{.Settlement.Status}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">地址</td><td style="padding:0 16px 12px;word-break:break-all;">{{.Settlement.Address}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Total</td><td style="padding:0 16px 12px;font-weight:700;">{{.Settlement.Total}} USDC</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Hold</td><td style="padding:0 16px 12px;">{{.Settlement.Hold}} USDC</td></tr>
            <tr><td style="padding:0 16px 14px;color:#64748b;">Available</td><td style="padding:0 16px 14px;font-weight:700;">{{.Settlement.Available}} USDC</td></tr>
          </table>
        </td></tr>

        {{if .Builders}}
        <tr><td class="pad" style="padding:22px 30px 10px;border-top:1px solid #e5e9f0;">
          <div style="font-size:16px;font-weight:750;color:#0f172a;">Builder 明细</div>
        </td></tr>
        <tr><td class="pad" style="padding:4px 30px 24px;">
          <table role="presentation" class="builder-table" width="100%" cellspacing="0" cellpadding="0" border="0" style="font-size:13px;border-collapse:collapse;">
            <tr style="background:#f8fafc;color:#64748b;">
              <th align="left" style="padding:10px 8px;">Builder</th>
              <th align="left" style="padding:10px 8px;">Reward</th>
              <th align="left" style="padding:10px 8px;">Sweep</th>
              <th align="left" style="padding:10px 8px;">Rate Limit</th>
            </tr>
            {{range .Builders}}
            <tr>
              <td valign="top" style="padding:12px 8px;border-bottom:1px solid #edf1f5;"><strong style="color:#334155;">{{.Name}}</strong><br><span style="font-size:11px;color:#94a3b8;">{{.Address}}</span></td>
              <td valign="top" style="padding:12px 8px;border-bottom:1px solid #edf1f5;"><span style="color:#64748b;">{{.Claimable}}</span><br><span style="color:{{.ClaimColor}};">{{.Claim}}</span></td>
              <td valign="top" style="padding:12px 8px;border-bottom:1px solid #edf1f5;color:{{.SweepColor}};">{{.Sweep}}</td>
              <td valign="top" style="padding:12px 8px;border-bottom:1px solid #edf1f5;color:{{.RateColor}};">{{.RateLimit}}</td>
            </tr>
            {{end}}
          </table>
        </td></tr>
        {{end}}

        {{if .ShowPayout}}
        <tr><td class="pad" style="padding:22px 30px 10px;border-top:1px solid #e5e9f0;">
          <div style="font-size:16px;font-weight:750;color:#0f172a;">Payout</div>
        </td></tr>
        <tr><td class="pad" style="padding:4px 30px 24px;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:13px;line-height:1.55;background:#f8fafc;border-radius:10px;">
            <tr><td style="padding:14px 16px;color:#64748b;">状态</td><td style="padding:14px 16px;color:{{.Payout.StatusColor}};font-weight:700;">{{.Payout.Status}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">金额</td><td style="padding:0 16px 12px;font-weight:700;">{{.Payout.Amount}} USDC</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Raw total</td><td style="padding:0 16px 12px;">{{.RawTotal}} USDC</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">付款前 total</td><td style="padding:0 16px 12px;">{{.Payout.TotalBefore}} USDC</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">确认依据</td><td style="padding:0 16px 12px;">{{.Payout.Evidence}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Rate Limit</td><td style="padding:0 16px 12px;color:{{.Payout.RateColor}};">{{.Payout.RateLimit}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Settlement</td><td style="padding:0 16px 12px;word-break:break-all;">{{.Payout.Settlement}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Recipient</td><td style="padding:0 16px 12px;word-break:break-all;">{{.Payout.Recipient}}</td></tr>
            <tr><td style="padding:0 16px 12px;color:#64748b;">Request hash</td><td style="padding:0 16px 12px;word-break:break-all;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:11px;">{{.Payout.RequestHash}}</td></tr>
            <tr><td style="padding:0 16px 14px;color:#64748b;">Nonce</td><td style="padding:0 16px 14px;">{{.Payout.Nonce}}</td></tr>
          </table>
        </td></tr>
        {{end}}

        {{if .Warnings}}
        <tr><td class="pad" style="padding:22px 30px;border-top:1px solid #e5e9f0;">
          <div style="font-size:15px;font-weight:750;color:#92400e;">告警</div>
          <ul style="margin:10px 0 0;padding-left:20px;color:#92400e;font-size:13px;line-height:1.6;">{{range .Warnings}}<li>{{.}}</li>{{end}}</ul>
        </td></tr>
        {{end}}

        {{if .ShowFailure}}
        <tr><td class="pad" style="padding:22px 30px;border-top:1px solid #e5e9f0;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#fef2f2;border:1px solid #fecaca;border-radius:12px;">
            <tr><td style="padding:18px;">
              <div style="font-size:13px;font-weight:700;color:#991b1b;">{{.FailureStage}}</div>
              <div style="margin-top:6px;font-size:15px;font-weight:700;color:#7f1d1d;">{{.FailureSummary}}</div>
              {{if .NextAction}}<div style="margin-top:10px;font-size:13px;line-height:1.55;color:#991b1b;"><strong>下一步：</strong>{{.NextAction}}</div>{{end}}
            </td></tr>
          </table>
        </td></tr>
        {{end}}
        {{end}}

        <tr><td class="pad" style="padding:22px 30px;background:#f8fafc;border-top:1px solid #e5e9f0;">
          <table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="font-size:11px;line-height:1.7;color:#64748b;">
            <tr><td>Run ID</td><td align="right" style="font-family:ui-monospace,SFMono-Regular,Menlo,monospace;">{{.RunID}}</td></tr>
            <tr><td>触发 / 执行</td><td align="right">{{.Trigger}} · {{.Execution}}</td></tr>
            <tr><td>Network / Phase</td><td align="right">{{.Network}} · {{.Phase}}</td></tr>
            <tr><td>开始</td><td align="right">{{.StartedAt}}</td></tr>
            <tr><td>结束</td><td align="right">{{.FinishedAt}}</td></tr>
          </table>
        </td></tr>
      </table>
    </td></tr>
  </table>
</body>
</html>`))
