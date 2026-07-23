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
			ClaimableUSDC: "1.25", ClaimStatus: funding.ReportStepWarning,
			ClaimSummary: "Reward 领取结果不确定",
			SweepAmount:  "1.5", SweepStatus: funding.ReportStepSuccess,
			SweepSummary: "资金已归集", RateLimitObserved: true, RateLimitRemaining: 999,
		}},
		Payout: funding.PayoutReport{
			Status: funding.ReportStepSuccess, Amount: "1.5",
			Settlement: "0xsettlement", Recipient: "0xrecipient",
			ConfirmationEvidence: "exchange_response", RequestHash: "hash", Nonce: 123,
		},
		SettlementBalance: funding.SettlementBalanceReport{
			Address: "0xsettlement", Total: "0.25", Hold: "0.1", Available: "0.15", Observed: true,
		},
		Warnings: []string{"builder-1：Reward 领取结果不确定"},
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
		"0x123456…345678", "builder-1：Reward 领取结果不确定",
		"Settlement 结束余额", "0.15 USDC",
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
	if strings.Contains(message.HTMLBody, "Builder 明细") {
		t.Fatalf("compact retry unexpectedly contains full builder details")
	}
}
