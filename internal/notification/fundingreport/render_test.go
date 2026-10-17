package fundingreport

import (
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/notification"
)

func TestRenderKeepsWarningDetailsAndEscapesValues(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	report := funding.RunReport{
		Status: funding.ReportStatusWarning, Trigger: funding.TriggerUTC,
		Network: "mainnet", RunID: "abcd1234", UTCDate: "2026-07-23",
		Phase: funding.PhasePayoutConfirmed, StartedAt: started, FinishedAt: started.Add(3200 * time.Millisecond),
		RecordsRead: true, RecordCount: 3, RawTotal: "1.4999999", PayoutTotal: "1.5",
		Stages: []funding.ReportStage{
			{Label: "数据快照", Status: funding.ReportStepSuccess, Summary: "2 条记录"},
			{Label: "Reward", Status: funding.ReportStepWarning, Summary: "1 个告警"},
		},
		Builders: []funding.BuilderReport{{
			Name: "<builder-1>", Address: "0x1234567890abcdef1234567890abcdef12345678",
			ClaimableUSDC: "1.2", ClaimableObserved: true, ClaimableQueryAttempts: 1,
			ClaimAttempts: 3, ClaimStatus: funding.ReportStepWarning,
			ClaimSummary: "Reward 领取结果不确定",
			SweepAmount:  "1.5", SweepStatus: funding.ReportStepSuccess,
			SweepSummary: "资金已归集", RateLimitObserved: true, RateLimitRemaining: 999,
		}},
		DailyConsolidation: funding.DailyConsolidationReport{
			UTCDate: "2026-07-23", Status: funding.DailyConsolidationCompleted,
			ClaimablesCapturedAt: started.Add(-time.Hour), CompletedAt: started.Add(-30 * time.Minute),
			TotalClaimableUSDC: "1.2", BuilderCount: 1,
			RecordedBuilders: 1, ObservedBuilders: 1, Observed: true,
			BelowPayout: true, ShortfallUSDC: "0.3",
		},
		Payouts: []funding.PayoutTargetReport{
			{
				Kind: funding.PayoutKindReferral, RecordsRead: true,
				RecordCount: 3, RawTotal: "1.4999999", PayoutTotal: "1.5",
				Payout: funding.PayoutReport{
					Status: funding.ReportStepSuccess, Amount: "1.5",
					Settlement: "0xsettlement", Recipient: "0xreferralrecipient",
					SettlementTotalBefore: "3", SubmitAttempts: 2,
					ConfirmationEvidence: "settlement_balance_decreased_with_activation_fee",
					Nonce:                124,
				},
				DatabaseStatus: funding.ReportStepSuccess,
			},
		},
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", Total: "0.5", Hold: "0.1", Available: "0.4", Observed: true,
		},
		Warnings: []string{
			"builder-1: Reward 领取结果不确定",
			"UTC 00:00 Claimable 合计 1.2 USDC，低于 Referral Payout 1.5 USDC，短缺 0.3 USDC，请人工核查结算口径",
		},
	}

	message := Render(report)
	assertNoRequestHashHTML(t, message.HTMLBody)
	if message.Status != notification.StatusWarning {
		t.Fatalf("message status = %q", message.Status)
	}
	if message.Subject != "成功但有告警 · Payout 1.50 USDC" {
		t.Fatalf("subject = %q", message.Subject)
	}
	for _, want := range []string{
		"成功但有告警", "1.5 USDC", "Builder 明细", "Payout 明细",
		"Referral Payout",
		"Settlement 精确减少 amount", "1 USDC（新地址费用）",
		"首次 Submit", "Exact replay", "已授权 / 可能已发送（原请求 / 相同 nonce）",
		"0x123456…345678", "builder-1: Reward 领取结果不确定",
		"Settlement 结束余额", "0.4 USDC",
		"归集对账", "零点 Claimable 合计", "1.2 USDC",
		"00:00 Reward", "对账差额（Claimable - Payout）", "-0.3 USDC",
		"提交 3 次", "完整但低于 Payout", "短缺 0.3 USDC",
	} {
		if !strings.Contains(message.HTMLBody, want) {
			t.Fatalf("HTML body missing %q", want)
		}
	}
	if strings.Contains(message.HTMLBody, "<builder-1>") || strings.Count(message.HTMLBody, "&lt;builder-1&gt;") != 1 {
		t.Fatalf("builder name must be escaped and displayed once: %s", message.HTMLBody)
	}
	if strings.Contains(message.HTMLBody, "ZgotmplZ") {
		t.Fatalf("HTML template rejected a trusted style value: %s", message.HTMLBody)
	}
	if count := strings.Count(message.HTMLBody, "付款状态"); count != 1 {
		t.Fatalf("payout card count = %d, want 1", count)
	}
	if count := strings.Count(message.HTMLBody, "数据库状态"); count != 1 {
		t.Fatalf("database status count = %d, want 1", count)
	}

	view := buildView(report)
	if len(view.Payouts) != 1 {
		t.Fatalf("len(Payouts) = %d, want 1", len(view.Payouts))
	}
	wants := []struct {
		name, records, raw, amount, firstSubmit, exactReplay, evidence, status, database string
	}{
		{
			name: "Referral Payout", records: "3", raw: "1.4999999", amount: "1.5",
			firstSubmit: "已授权 / 可能已发送",
			exactReplay: "已授权 / 可能已发送（原请求 / 相同 nonce）",
			evidence:    "Settlement 精确减少 amount + 1 USDC（新地址费用）",
			status:      "已完成", database: "已完成",
		},
	}
	for index, want := range wants {
		got := view.Payouts[index]
		if got.Name != want.name || got.RecordCount != want.records || got.RawTotal != want.raw ||
			got.Amount != want.amount || got.Status != want.status ||
			got.FirstSubmit != want.firstSubmit || got.ExactReplay != want.exactReplay ||
			got.Evidence != want.evidence || got.Database != want.database {
			t.Fatalf("Payouts[%d] = %#v, want %#v", index, got, want)
		}
	}
}

func TestBuildViewShowsFinalBlockedPayoutAfterExactReplay(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{Payouts: []funding.PayoutTargetReport{{
		Kind: funding.PayoutKindReferral, RecordsRead: true, RecordCount: 2,
		Payout: funding.PayoutReport{
			Status: funding.ReportStepFailed, Amount: "3.25", SubmitAttempts: 2, Blocked: true,
		},
		DatabaseStatus: funding.ReportStepPending,
	}}}

	view := buildView(report)
	if len(view.Payouts) != 1 {
		t.Fatalf("len(Payouts) = %d, want 1", len(view.Payouts))
	}
	payout := view.Payouts[0]
	if payout.FirstSubmit != "已授权 / 可能已发送" ||
		payout.ExactReplay != "已授权 / 可能已发送（原请求 / 相同 nonce）" ||
		payout.Evidence != "发送方余额未能确认（blocked）" ||
		payout.Status != "失败" {
		t.Fatalf("blocked payout view = %#v", payout)
	}
}

func TestRenderHidesUnconfirmedSettlementAmounts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, want          string
		unconfirmed, failed bool
	}{
		{name: "post payout unconfirmed", want: "Payout 后余额未确认", unconfirmed: true},
		{name: "query failed", want: "查询失败", failed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := funding.RunReport{
				Status: funding.ReportStatusWarning,
				SettlementBalance: funding.SettlementBalanceReport{
					Address: "0xsettlement", Total: "987.65", Hold: "10.01", Available: "977.64",
					Observed: true, PostPayoutUnconfirmed: tt.unconfirmed, QueryFailed: tt.failed,
				},
			}
			message := Render(report)
			if !strings.Contains(message.HTMLBody, tt.want) {
				t.Fatalf("HTML body missing settlement warning %q", tt.want)
			}
			for _, stale := range []string{"987.65", "10.01", "977.64", "— USDC"} {
				if strings.Contains(message.HTMLBody, stale) {
					t.Fatalf("unconfirmed settlement contains amount %q", stale)
				}
			}
		})
	}
}

func TestBuildViewKeepsReferralPayoutAndDatabaseStatusesIndependent(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{Payouts: []funding.PayoutTargetReport{{
		Kind: funding.PayoutKindReferral, RecordsRead: true,
		Payout:         funding.PayoutReport{Status: funding.ReportStepSuccess, SubmitAttempts: 1, ConfirmationEvidence: "exchange_response"},
		DatabaseStatus: funding.ReportStepFailed,
	}}}

	view := buildView(report)
	if len(view.Payouts) != 1 {
		t.Fatalf("len(Payouts) = %d, want 1", len(view.Payouts))
	}
	if got := view.Payouts[0]; got.Status != "已完成" || got.Database != "失败" ||
		got.ExactReplay != "未授权" || got.Evidence != "Exchange 明确确认" {
		t.Fatalf("referral statuses = %#v", got)
	}
}

func TestRenderDoesNotPresentUnconfirmedRateLimitAsHealthy(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{
		Builders: []funding.BuilderReport{{
			Name: "builder-1", RateLimitObserved: true, RateLimitRemaining: 201,
			RateLimitVisibilityUnconfirmed: true,
		}},
		SettlementRateLimitObserved: true, SettlementRateLimitRemaining: 200,
		SettlementRateLimitVisibilityUnconfirmed: true,
	}

	message := Render(report)
	for _, want := range []string{
		"最低观测 201 · 刷新未确认", "最低观测 200 · 刷新未确认",
	} {
		if !strings.Contains(message.HTMLBody, want) {
			t.Fatalf("HTML body missing %q", want)
		}
	}
}

func TestBuildDailyConsolidationViewShowsCoveredPayoutAsNormal(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		claimable  string
		payout     string
		difference string
	}{
		{name: "greater", claimable: "1.6", payout: "1.5", difference: "0.1"},
		{name: "equal", claimable: "1.5", payout: "1.5", difference: "0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			view := buildDailyConsolidationView(funding.RunReport{
				PayoutTotal: tt.payout,
				DailyConsolidation: funding.DailyConsolidationReport{
					UTCDate: "2026-07-23", Status: funding.DailyConsolidationCompleted,
					TotalClaimableUSDC: tt.claimable,
					BuilderCount:       1, RecordedBuilders: 1, ObservedBuilders: 1, Observed: true,
					CoversPayout: true,
				},
			})
			if view.Status != "完整，Claimable ≥ Payout" ||
				view.Difference != tt.difference ||
				view.DifferenceColor != "#15803d" {
				t.Fatalf("snapshot view = %#v", view)
			}
		})
	}
}

func TestBuildDailyConsolidationViewDoesNotPresentMissingClaimableAsZero(t *testing.T) {
	t.Parallel()

	none := buildDailyConsolidationView(funding.RunReport{
		DailyConsolidation: funding.DailyConsolidationReport{
			UTCDate: "2026-07-23", Status: funding.DailyConsolidationCompleted,
			BuilderCount: 1, RecordedBuilders: 1, Observed: true,
		},
	})
	if none.Total != "—" || none.TotalLabel != "零点 Claimable 合计" ||
		none.Status != "零点数据不完整" {
		t.Fatalf("unobserved claimable view = %#v", none)
	}

	partial := buildDailyConsolidationView(funding.RunReport{
		DailyConsolidation: funding.DailyConsolidationReport{
			UTCDate: "2026-07-23", Status: funding.DailyConsolidationCompleted,
			TotalClaimableUSDC: "1.2", BuilderCount: 2,
			RecordedBuilders: 2, ObservedBuilders: 1, Observed: true,
		},
	})
	if partial.Total != "1.2" || partial.TotalLabel != "已观测 Claimable 小计" ||
		partial.Status != "零点数据不完整" {
		t.Fatalf("partial claimable view = %#v", partial)
	}
}

func TestRenderBuildsCompactRetryEmail(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{
		Status: funding.ReportStatusRetrying, Compact: true,
		Attempt: 2, MaxAttempts: 6, Trigger: funding.TriggerUTC,
		FailureStage: "sweep", FailureSummary: "Settlement 余额不足。",
		NextAction: "服务将在约 1 分钟后自动重试。",
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", Total: "1", Hold: "0.25", Available: "0.75", Observed: true,
		},
		Builders: []funding.BuilderReport{{Name: "builder-only-in-full-report"}},
		Payouts: []funding.PayoutTargetReport{{
			Kind:   funding.PayoutKindReferral,
			Payout: funding.PayoutReport{Amount: "1.123456", Recipient: "recipient-only-in-full-report"},
		}},
		DailyConsolidation: funding.DailyConsolidationReport{
			UTCDate: "2026-07-23", Missing: true, BuilderCount: 1,
		},
	}
	message := Render(report)
	if message.Status != notification.StatusRetrying {
		t.Fatalf("message status = %q", message.Status)
	}
	if message.Subject != "第 2/6 次执行失败 · 等待重试" {
		t.Fatalf("subject = %q", message.Subject)
	}
	if !strings.Contains(message.HTMLBody, "Settlement 余额不足") {
		t.Fatalf("HTML body missing failure summary")
	}
	if !strings.Contains(message.HTMLBody, "Available 0.75 USDC") {
		t.Fatalf("compact HTML body missing final settlement balance")
	}
	if strings.Contains(message.HTMLBody, "0xsettlement") {
		t.Fatalf("compact HTML body unexpectedly contains settlement address")
	}
	if !strings.Contains(message.HTMLBody, "归集对账") ||
		!strings.Contains(message.HTMLBody, "未记录") {
		t.Fatalf("compact HTML body missing daily consolidation")
	}
	for _, omitted := range []string{"Builder 明细", "Payout 明细", "builder-only-in-full-report", "recipient-only-in-full-report"} {
		if strings.Contains(message.HTMLBody, omitted) {
			t.Fatalf("compact retry unexpectedly contains %q", omitted)
		}
	}
}

func TestRenderCompactRetryKeepsExactPayoutAndIndependentStatuses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, amount, stage, summary, wantDatabase string
		database                                   funding.ReportStepStatus
	}{
		{
			name: "database completion failed", amount: "1.234568", stage: "referral_database",
			summary:  "MySQL funding records 未能标记完成。",
			database: funding.ReportStepFailed, wantDatabase: "失败",
		},
		{
			name: "archive failed", amount: "1.234568", stage: "finalize",
			summary:  "运行历史归档失败。",
			database: funding.ReportStepSuccess, wantDatabase: "已完成",
		},
		{
			name: "tiny payout", amount: "0.000001", stage: "referral_database",
			summary:  "MySQL funding records 未能标记完成。",
			database: funding.ReportStepFailed, wantDatabase: "失败",
		},
		{
			name: "without failure summary", amount: "1.234568",
			database: funding.ReportStepSuccess, wantDatabase: "已完成",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := successfulReport()
			report.Status, report.Compact = funding.ReportStatusRetrying, true
			report.Attempt, report.MaxAttempts = 2, 6
			report.FailureStage, report.FailureSummary = tt.stage, tt.summary
			report.PayoutTotal, report.Payouts[0].Payout.Amount = tt.amount, tt.amount
			report.Payouts[0].DatabaseStatus = tt.database

			message := Render(report)
			bodyText := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(message.HTMLBody, "")
			bodyText = strings.Join(strings.Fields(html.UnescapeString(bodyText)), " ")
			for _, want := range []string{
				"Payout " + tt.amount + " USDC", "付款状态 已完成", "数据库状态 " + tt.wantDatabase,
			} {
				if !strings.Contains(bodyText, want) {
					t.Fatalf("compact retry missing %q", want)
				}
			}
			if count := strings.Count(message.HTMLBody, tt.amount); count != 1 {
				t.Fatalf("exact payout amount appears %d times, want once", count)
			}
			if tt.name == "tiny payout" && !strings.HasSuffix(message.Subject, "Payout 0.00 USDC") {
				t.Fatalf("tiny payout subject = %q", message.Subject)
			}
			for _, omitted := range []string{
				"Payout 明细", "Builder 明细", "Raw total", "付款前 total", "首次 Submit",
				"Exact replay", "确认依据", "Nonce", "recipient-for-report", "0xsettlement",
			} {
				if strings.Contains(message.HTMLBody, omitted) {
					t.Fatalf("compact retry unexpectedly contains %q", omitted)
				}
			}
		})
	}
}

func TestRenderCompactEarlyFailureDoesNotInventPayout(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{
		Status: funding.ReportStatusRetrying, Compact: true, Attempt: 2, MaxAttempts: 6,
		FailureStage: "snapshot", FailureSummary: "数据库 pending funding records 查询失败。",
		Payouts: []funding.PayoutTargetReport{{
			Kind:           funding.PayoutKindReferral,
			Payout:         funding.PayoutReport{Status: funding.ReportStepPending},
			DatabaseStatus: funding.ReportStepPending,
		}},
		SettlementBalance: funding.SettlementBalanceReport{
			Total: "0.75", Hold: "0", Available: "0.75", Observed: true,
		},
	}
	message := Render(report)
	if message.Subject != "第 2/6 次执行失败 · 等待重试" ||
		!strings.Contains(message.HTMLBody, report.FailureSummary) ||
		!strings.Contains(message.HTMLBody, "Available 0.75 USDC") {
		t.Fatalf("early failure lost its status or settlement summary: %#v", message)
	}
	for _, omitted := range []string{"Payout ", "付款状态", "数据库状态"} {
		if strings.Contains(message.HTMLBody, omitted) {
			t.Fatalf("early failure invented payout details %q", omitted)
		}
	}
}

func TestRenderSubjectUsesRoundedAmountWithoutChangingBodyPrecision(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, amount, outcome, want string
		status                      funding.ReportStatus
	}{
		{name: "success", status: funding.ReportStatusSuccess, amount: "123.45", want: "Payout 123.45 USDC"},
		{name: "missing amount", status: funding.ReportStatusSuccess, want: "Payout — USDC"},
		{name: "round up", status: funding.ReportStatusSuccess, amount: "1.235", want: "Payout 1.24 USDC"},
		{name: "tiny payout", status: funding.ReportStatusSuccess, amount: "0.000001", want: "Payout 0.00 USDC"},
		{name: "warning", status: funding.ReportStatusWarning, amount: "1.5", want: "成功但有告警 · Payout 1.50 USDC"},
		{name: "critical", status: funding.ReportStatusCritical, amount: "1.5", want: "需要人工处理 · Payout 1.50 USDC"},
		{name: "retry", status: funding.ReportStatusRetrying, amount: "1.234568", want: "第 2/6 次执行失败 · 等待重试 · Payout 1.23 USDC"},
		{name: "exhausted", status: funding.ReportStatusCritical, outcome: "retry_exhausted", amount: "1.5", want: "自动重试已耗尽 · Payout 1.50 USDC"},
		{name: "no data", status: funding.ReportStatusNoData, outcome: "no_data", amount: "0", want: "无待处理数据"},
		{name: "no data warning", status: funding.ReportStatusWarning, outcome: "no_data", amount: "0", want: "无数据但有告警"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := successfulReport()
			report.Status, report.Outcome = tt.status, tt.outcome
			report.Compact = tt.status == funding.ReportStatusRetrying
			report.PayoutTotal = tt.amount
			report.Payouts[0].Payout.Amount = tt.amount
			report.Attempt, report.MaxAttempts = 2, 6
			if tt.outcome == "no_data" {
				report.RecordCount, report.Payouts = 0, nil
			}
			message := Render(report)
			if message.Subject != tt.want {
				t.Fatalf("subject = %q, want %q", message.Subject, tt.want)
			}
			if tt.amount != "" && tt.outcome != "no_data" && !strings.Contains(message.HTMLBody, tt.amount) {
				t.Fatalf("body lost original payout precision %q", tt.amount)
			}
		})
	}
}

func TestRenderSuccessfulReportKeepsOneConciseSetOfDetails(t *testing.T) {
	t.Parallel()
	report := successfulReport()
	message := Render(report)
	assertNoRequestHashHTML(t, message.HTMLBody)
	for _, want := range []string{
		"Builder 明细", "00:00 Reward", "Reward 已领取", "资金已归集",
		"Payout 明细", "付款状态", "数据库状态",
		"Settlement 结束余额", "Available", "8.765432 USDC",
		"归集对账", "2026-07-23", "完整，Claimable ≥ Payout",
	} {
		if !strings.Contains(message.HTMLBody, want) {
			t.Fatalf("successful body missing %q", want)
		}
	}
	for _, duplicate := range []string{"builder-for-report", html.EscapeString(buildView(report).Preheader)} {
		if count := strings.Count(message.HTMLBody, duplicate); count != 1 {
			t.Fatalf("%q appears %d times, want once", duplicate, count)
		}
	}
	for _, omitted := range []string{
		"执行流程", "PAYOUT USDC", "RECORDS", "BUILDERS", "DURATION",
		"本轮 Referral Payout", "Claimable 作为对账基准", "查询 1 次", "提交 1 次",
		"Rate Limit", "剩余 999", "剩余 888", "首次 Submit", "Exact replay",
		"Raw total", "付款前", "Exchange 明确确认", "Nonce",
		">Total<", ">Hold<", "Recipient", "recipient-for-report", "0xsettlement", "付款账户",
	} {
		if strings.Contains(message.HTMLBody, omitted) {
			t.Fatalf("successful body unexpectedly contains %q", omitted)
		}
	}
}

func TestRenderRetainsPayoutDiagnosticsWhenInvestigationIsNeeded(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		status   funding.ReportStatus
		recovery bool
		attempts int
	}{
		{name: "warning", status: funding.ReportStatusWarning, attempts: 1},
		{name: "critical", status: funding.ReportStatusCritical, attempts: 1},
		{name: "recovery", status: funding.ReportStatusSuccess, recovery: true, attempts: 1},
		{name: "exact replay", status: funding.ReportStatusSuccess, attempts: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			report := successfulReport()
			report.Status, report.Recovery = tt.status, tt.recovery
			report.Payouts[0].Payout.SubmitAttempts = tt.attempts
			message := Render(report)
			assertNoRequestHashHTML(t, message.HTMLBody)
			for _, want := range []string{
				"1.234567000000000001", "10.123456 USDC", "首次 Submit", "Exact replay",
				"Exchange 明确确认", "123456789",
			} {
				if !strings.Contains(message.HTMLBody, want) {
					t.Fatalf("diagnostic body missing %q", want)
				}
			}
		})
	}
}

func TestRenderKeepsExceptionalCountsRateLimitsAndHold(t *testing.T) {
	t.Parallel()
	report := successfulReport()
	report.Status = funding.ReportStatusWarning
	report.Builders[0].ClaimableQueryAttempts = 2
	report.Builders[0].ClaimAttempts = 3
	report.Builders[0].RateLimitRemaining = 199
	report.SettlementRateLimitQueryFailed = true
	report.SettlementBalance.Hold = "1.25"
	report.SettlementBalance.Total = "10.015432"
	message := Render(report)
	for _, want := range []string{"查询 2 次", "提交 3 次", "剩余 199", "查询失败", "10.015432 USDC", "1.25 USDC"} {
		if !strings.Contains(message.HTMLBody, want) {
			t.Fatalf("exceptional body missing %q", want)
		}
	}
}

func successfulReport() funding.RunReport {
	return funding.RunReport{
		Status: funding.ReportStatusSuccess, RecordsRead: true, RecordCount: 3,
		PayoutTotal: "1.234568", RawTotal: "1.234567000000000001",
		Builders: []funding.BuilderReport{{
			Name: "builder-for-report", ClaimableUSDC: "1.5", ClaimableObserved: true,
			ClaimableQueryAttempts: 1, ClaimAttempts: 1,
			ClaimStatus: funding.ReportStepSuccess, ClaimSummary: "Reward 已领取",
			SweepAmount: "1.5", SweepStatus: funding.ReportStepSuccess, SweepSummary: "资金已归集",
			RateLimitObserved: true, RateLimitRemaining: 999,
		}},
		DailyConsolidation: funding.DailyConsolidationReport{
			UTCDate: "2026-07-23", Status: funding.DailyConsolidationCompleted,
			TotalClaimableUSDC: "1.5", BuilderCount: 1, RecordedBuilders: 1,
			ObservedBuilders: 1, Observed: true, CoversPayout: true,
		},
		Payouts: []funding.PayoutTargetReport{{
			Kind: funding.PayoutKindReferral, RecordsRead: true, RecordCount: 3,
			RawTotal: "1.234567000000000001", PayoutTotal: "1.234568",
			Payout: funding.PayoutReport{
				Status: funding.ReportStepSuccess, Amount: "1.234568", Recipient: "recipient-for-report",
				SettlementTotalBefore: "10.123456", SubmitAttempts: 1,
				ConfirmationEvidence: "exchange_response", Nonce: 123456789,
			},
			DatabaseStatus: funding.ReportStepSuccess,
		}},
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", Total: "8.765432", Hold: "0", Available: "8.765432", Observed: true,
		},
		SettlementRateLimitObserved: true, SettlementRateLimitRemaining: 888,
	}
}

func assertNoRequestHashHTML(t *testing.T, body string) {
	t.Helper()
	for _, removed := range []string{"Request hash", "referral-hash", "hash-for-report"} {
		if strings.Contains(body, removed) {
			t.Fatalf("HTML contains removed local request hash %q", removed)
		}
	}
}
