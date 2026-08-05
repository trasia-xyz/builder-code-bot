package fundingreport

import (
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/notification"
)

func TestRenderBuildsRichSuccessEmailAndEscapesValues(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	report := funding.RunReport{
		Status: funding.ReportStatusWarning, Trigger: funding.TriggerUTC,
		Network: "mainnet", RunID: "abcd1234", UTCDate: "2026-07-23",
		Phase: funding.PhasePayoutConfirmed, StartedAt: started, FinishedAt: started.Add(3200 * time.Millisecond),
		RecordsRead: true, RecordCount: 2, RawTotal: "1.4999999", PayoutTotal: "1.5",
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
		Payout: funding.PayoutReport{
			Status: funding.ReportStepSuccess, Amount: "1.5",
			Settlement: "0xsettlement", Recipient: "0xrecipient",
			ConfirmationEvidence: "exchange_response", RequestHash: "hash", Nonce: 123,
		},
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", Total: "0.25", Hold: "0.1", Available: "0.15", Observed: true,
		},
		Warnings: []string{
			"builder-1: Reward 领取结果不确定",
			"UTC 00:00 Claimable 合计 1.2 USDC，低于数据库 Payout 1.5 USDC，短缺 0.3 USDC，请人工核查结算口径",
		},
	}

	message := Render(report)
	if message.Status != notification.StatusWarning {
		t.Fatalf("message status = %q", message.Status)
	}
	if message.Subject != "成功但有告警 · 1.5 USDC · 2 条记录" {
		t.Fatalf("subject = %q", message.Subject)
	}
	for _, want := range []string{
		"成功但有告警", "Payout 1.5 USDC", "Builder 明细", "Exchange 明确确认",
		"0x123456…345678", "builder-1: Reward 领取结果不确定",
		"Settlement 结束余额", "0.15 USDC",
		"UTC 00:00 Claim 与资金归集", "零点 Claimable 合计", "1.2 USDC",
		"00:00 Reward", "Claimable 作为对账基准", "对账差额", "-0.3 USDC",
		"查询 1 次", "提交 3 次", "完整但低于 Payout", "短缺 0.3 USDC",
	} {
		if !strings.Contains(message.HTMLBody, want) {
			t.Fatalf("HTML body missing %q", want)
		}
	}
	if strings.Contains(message.HTMLBody, "<builder-1>") || !strings.Contains(message.HTMLBody, "&lt;builder-1&gt;") {
		t.Fatalf("builder name was not escaped: %s", message.HTMLBody)
	}
	if strings.Contains(message.HTMLBody, "ZgotmplZ") {
		t.Fatalf("HTML template rejected a trusted style value: %s", message.HTMLBody)
	}
}

func TestRenderShowsUnconfirmedPostPayoutBalanceWithoutAmounts(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{
		Status: funding.ReportStatusWarning,
		Payout: funding.PayoutReport{Status: funding.ReportStepPending},
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", PostPayoutUnconfirmed: true,
		},
	}

	settlement := buildSettlementView(report.SettlementBalance)
	if settlement.Status != "Payout 后余额未确认" ||
		settlement.Total != "—" || settlement.Available != "—" {
		t.Fatalf("settlement view = %#v", settlement)
	}

	message := Render(report)
	if !strings.Contains(message.HTMLBody, "Payout 后余额未确认") {
		t.Fatalf("HTML body missing unconfirmed post-payout status")
	}
	if count := strings.Count(message.HTMLBody, ">— USDC</td>"); count != 3 {
		t.Fatalf("settlement placeholder count = %d, want 3", count)
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
		Builders: []funding.BuilderReport{{Name: "builder-1"}},
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
	if !strings.Contains(message.HTMLBody, "0xsettlement") {
		t.Fatalf("compact HTML body missing settlement address")
	}
	if !strings.Contains(message.HTMLBody, "UTC 00:00 Claim 与资金归集") ||
		!strings.Contains(message.HTMLBody, "未记录") {
		t.Fatalf("compact HTML body missing daily consolidation")
	}
	if strings.Contains(message.HTMLBody, "Builder 明细") {
		t.Fatalf("compact retry unexpectedly contains full builder details")
	}
}
