package fundingreport

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"builder-code-bot/internal/funding"
)

func TestRenderIncomeUsesCompleteSnapshotRegardlessOfCollectionOutcome(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name        string
		claimable   string
		payout      string
		adjust      func(*funding.RunReport)
		wantSubject string
		wantIncome  string
	}{
		{
			name: "zero amounts", claimable: "0", payout: "0",
			wantSubject: "收入 0.00 USDC", wantIncome: "0",
		},
		{
			name: "equal amounts", claimable: "1.234568", payout: "1.234568",
			wantSubject: "收入 0.00 USDC", wantIncome: "0",
		},
		{
			name: "tiny income", claimable: "1.234569", payout: "1.234568",
			wantSubject: "收入 0.00 USDC", wantIncome: "0.000001",
		},
		{
			name: "negative income", claimable: "1.2", payout: "1.234568",
			wantSubject: "收入 -0.03 USDC", wantIncome: "-0.034568",
		},
		{
			name: "no funding records", claimable: "1.234568", payout: "0",
			adjust: func(report *funding.RunReport) {
				report.Status, report.Outcome = funding.ReportStatusNoData, "no_data"
				report.RecordCount, report.Payouts = 0, nil
			},
			wantSubject: "无待处理数据 · 收入 1.23 USDC", wantIncome: "1.234568",
		},
		{
			name: "no funding records with warning", claimable: "1.234568", payout: "0",
			adjust: func(report *funding.RunReport) {
				report.Status, report.Outcome = funding.ReportStatusWarning, "no_data"
				report.RecordCount, report.Payouts = 0, nil
			},
			wantSubject: "无数据但有告警 · 收入 1.23 USDC", wantIncome: "1.234568",
		},
		{
			name: "collection incomplete", claimable: "1.5", payout: "1.234568",
			adjust: func(report *funding.RunReport) {
				report.DailyConsolidation.Status = funding.DailyConsolidationClaimablesCaptured
			},
			wantSubject: "收入 0.27 USDC", wantIncome: "0.265432",
		},
		{
			name: "claim uncertain", claimable: "1.5", payout: "1.234568",
			adjust: func(report *funding.RunReport) {
				report.DailyConsolidation.UncertainClaims = 1
			},
			wantSubject: "收入 0.27 USDC", wantIncome: "0.265432",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report := successfulReport()
			report.DailyConsolidation.TotalClaimableUSDC = tt.claimable
			report.PayoutTotal = tt.payout
			if tt.adjust != nil {
				tt.adjust(&report)
			}
			message := Render(report)
			if message.Subject != tt.wantSubject {
				t.Fatalf("subject = %q, want %q", message.Subject, tt.wantSubject)
			}
			if got := buildDailyConsolidationView(report).Difference; got != tt.wantIncome {
				t.Fatalf("income = %q, want %q", got, tt.wantIncome)
			}
			bodyText := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(message.HTMLBody, "")
			bodyText = strings.Join(strings.Fields(html.UnescapeString(bodyText)), " ")
			if !strings.Contains(bodyText, "收入（Claimable - Payout） "+tt.wantIncome+" USDC") {
				t.Fatalf("body lost exact income %q", tt.wantIncome)
			}
		})
	}
}

func TestRenderIncomeRequiresCompleteAndValidSourceAmounts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		adjust func(*funding.RunReport)
	}{
		{name: "snapshot absent", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation = funding.DailyConsolidationReport{}
		}},
		{name: "snapshot not observed", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.Observed = false
		}},
		{name: "snapshot missing", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.Missing = true
		}},
		{name: "snapshot load failed", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.LoadFailed = true
		}},
		{name: "no builders", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.BuilderCount = 0
			report.DailyConsolidation.RecordedBuilders = 0
			report.DailyConsolidation.ObservedBuilders = 0
		}},
		{name: "builder record missing", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.RecordedBuilders = 0
		}},
		{name: "extra builder record", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.RecordedBuilders++
		}},
		{name: "builder observation missing", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.ObservedBuilders = 0
		}},
		{name: "extra builder observation", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.ObservedBuilders++
		}},
		{name: "funding records not read", adjust: func(report *funding.RunReport) {
			report.RecordsRead = false
		}},
		{name: "claimable absent", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.TotalClaimableUSDC = ""
		}},
		{name: "claimable invalid", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.TotalClaimableUSDC = "invalid"
		}},
		{name: "claimable negative", adjust: func(report *funding.RunReport) {
			report.DailyConsolidation.TotalClaimableUSDC = "-1.5"
		}},
		{name: "payout absent", adjust: func(report *funding.RunReport) {
			report.PayoutTotal = ""
		}},
		{name: "payout invalid", adjust: func(report *funding.RunReport) {
			report.PayoutTotal = "invalid"
		}},
		{name: "payout negative", adjust: func(report *funding.RunReport) {
			report.PayoutTotal = "-1.234568"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			report := successfulReport()
			tt.adjust(&report)
			if got := Render(report).Subject; got != "收入 — USDC" {
				t.Fatalf("subject = %q, want unknown income", got)
			}
			if got := buildDailyConsolidationView(report); got.Show && got.Difference != "—" {
				t.Fatalf("incomplete data produced income %q", got.Difference)
			}
		})
	}
}
