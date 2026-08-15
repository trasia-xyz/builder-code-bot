package fundingreport

import (
	"strings"
	"testing"
	"time"

	"builder-code-bot/internal/funding"
	"builder-code-bot/internal/notification"
)

func TestRenderBuildsRichDualPayoutEmailAndEscapesValues(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	report := funding.RunReport{
		Status: funding.ReportStatusWarning, Trigger: funding.TriggerUTC,
		Network: "mainnet", RunID: "abcd1234", UTCDate: "2026-07-23",
		Phase: funding.PhasePayoutConfirmed, StartedAt: started, FinishedAt: started.Add(3200 * time.Millisecond),
		RecordsRead: true, RecordCount: 3, RawTotal: "1.4999998", PayoutTotal: "1.5",
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
				Kind: funding.PayoutKindPointsRebate, RecordsRead: true,
				RecordCount: 2, RawTotal: "1.0999999", PayoutTotal: "1.1",
				Payout: funding.PayoutReport{
					Status: funding.ReportStepSuccess, Amount: "1.1",
					Settlement: "0xsettlement", Recipient: "0xpointsrecipient",
					SettlementTotalBefore: "2", SubmitAttempts: 1,
					ConfirmationEvidence: "exchange_response",
					RequestHash:          "points-hash", Nonce: 123,
				},
				DatabaseStatus: funding.ReportStepSuccess,
			},
			{
				Kind: funding.PayoutKindReferral, RecordsRead: true,
				RecordCount: 1, RawTotal: "0.3999999", PayoutTotal: "0.4",
				Payout: funding.PayoutReport{
					Status: funding.ReportStepSuccess, Amount: "0.4",
					Settlement: "0xsettlement", Recipient: "0xreferralrecipient",
					SettlementTotalBefore: "0.9", SubmitAttempts: 2,
					ConfirmationEvidence: "settlement_balance_decreased_with_activation_fee",
					RequestHash:          "referral-hash", Nonce: 124,
				},
				DatabaseStatus: funding.ReportStepSuccess,
			},
		},
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", Total: "0.5", Hold: "0.1", Available: "0.4", Observed: true,
		},
		Warnings: []string{
			"builder-1: Reward 领取结果不确定",
			"UTC 00:00 Claimable 合计 1.2 USDC，低于两类数据库 Payout 合计 1.5 USDC，短缺 0.3 USDC，请人工核查结算口径",
		},
	}

	message := Render(report)
	if message.Status != notification.StatusWarning {
		t.Fatalf("message status = %q", message.Status)
	}
	if message.Subject != "成功但有告警 · 1.5 USDC · 3 条记录" {
		t.Fatalf("subject = %q", message.Subject)
	}
	for _, want := range []string{
		"成功但有告警", "Payout 1.5 USDC", "Builder 明细", "Payout 明细",
		"Points Rebate Payout", "Referral Payout", "Exchange 明确确认",
		"Settlement 精确减少 amount", "1 USDC（新地址费用）",
		"首次 Submit", "Exact replay", "已授权 / 可能已发送（原请求 / 相同 nonce）",
		"0xpointsrecipient", "0xreferralrecipient", "points-hash", "referral-hash",
		"0x123456…345678", "builder-1: Reward 领取结果不确定",
		"Settlement 结束余额", "0.4 USDC",
		"UTC 00:00 Claim 与资金归集", "零点 Claimable 合计", "1.2 USDC",
		"00:00 Reward", "Claimable 作为对账基准",
		"本轮两类数据库 Payout 合计", "对账差额（Claimable - Payout 合计）", "-0.3 USDC",
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
	if count := strings.Count(message.HTMLBody, "付款状态"); count != 2 {
		t.Fatalf("payout card count = %d, want 2", count)
	}
	if count := strings.Count(message.HTMLBody, "数据库状态"); count != 2 {
		t.Fatalf("database status count = %d, want 2", count)
	}

	view := buildView(report)
	if len(view.Payouts) != 2 {
		t.Fatalf("len(Payouts) = %d, want 2", len(view.Payouts))
	}
	wants := []struct {
		name, records, raw, amount, recipient, firstSubmit, exactReplay, evidence, status, database string
	}{
		{
			name: "Points Rebate Payout", records: "2", raw: "1.0999999", amount: "1.1",
			recipient: "0xpointsrecipient", firstSubmit: "已授权 / 可能已发送", exactReplay: "未授权",
			evidence: "Exchange 明确确认", status: "已完成", database: "已完成",
		},
		{
			name: "Referral Payout", records: "1", raw: "0.3999999", amount: "0.4",
			recipient: "0xreferralrecipient", firstSubmit: "已授权 / 可能已发送",
			exactReplay: "已授权 / 可能已发送（原请求 / 相同 nonce）",
			evidence:    "Settlement 精确减少 amount + 1 USDC（新地址费用）",
			status:      "已完成", database: "已完成",
		},
	}
	for index, want := range wants {
		got := view.Payouts[index]
		if got.Name != want.name || got.RecordCount != want.records || got.RawTotal != want.raw ||
			got.Amount != want.amount || got.Recipient != want.recipient || got.Status != want.status ||
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

func TestRenderShowsUnconfirmedPostPayoutBalanceWithoutAmounts(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{
		Status: funding.ReportStatusWarning,
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", PostPayoutUnconfirmed: true,
		},
	}

	settlement := buildSettlementView(report)
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

func TestBuildViewKeepsPayoutTargetStatusesIndependent(t *testing.T) {
	t.Parallel()
	report := funding.RunReport{Payouts: []funding.PayoutTargetReport{
		{
			Kind: funding.PayoutKindPointsRebate, RecordsRead: true,
			Payout:         funding.PayoutReport{Status: funding.ReportStepSuccess},
			DatabaseStatus: funding.ReportStepFailed,
		},
		{
			Kind: funding.PayoutKindReferral, RecordsRead: true,
			Payout:         funding.PayoutReport{Status: funding.ReportStepWarning},
			DatabaseStatus: funding.ReportStepSkipped,
		},
	}}

	view := buildView(report)
	if len(view.Payouts) != 2 {
		t.Fatalf("len(Payouts) = %d, want 2", len(view.Payouts))
	}
	if view.Payouts[0].Status != "已完成" || view.Payouts[0].Database != "失败" {
		t.Fatalf("points rebate statuses = %#v", view.Payouts[0])
	}
	if view.Payouts[1].Status != "有告警" || view.Payouts[1].Database != "已跳过" {
		t.Fatalf("referral statuses = %#v", view.Payouts[1])
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
