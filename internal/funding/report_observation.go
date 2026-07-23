package funding

import (
	"context"
	"log/slog"
	"strings"
)

func (o *Orchestrator) observeSettlementFinalBalance(ctx context.Context, report *RunReport) {
	if o == nil || report == nil {
		return
	}
	token := report.token
	if token == nil {
		resolved, err := o.chain.CanonicalUSDC(ctx)
		if err != nil {
			report.SettlementBalance.QueryFailed = true
			report.addWarning("Settlement：任务结束余额查询失败")
			o.warn(ctx, "final settlement token query failed",
				slog.String("event", "funding_settlement_final_balance_query_failed"),
				slog.String("query_kind", "canonical_usdc"))
			return
		}
		token = &resolved
		report.token = cloneToken(token)
	}

	settlement := strings.TrimSpace(report.SettlementBalance.Address)
	if settlement == "" {
		settlement = o.settlement
	}
	balance, err := o.chain.SpotBalance(ctx, settlement, *token)
	if err != nil {
		report.SettlementBalance.QueryFailed = true
		report.addWarning("Settlement：任务结束余额查询失败")
		o.warn(ctx, "final settlement balance query failed",
			slog.String("event", "funding_settlement_final_balance_query_failed"),
			slog.String("query_kind", "spot_balance"),
			slog.String("settlement", settlement))
		return
	}

	hold := balance.Total.Sub(balance.Available)
	report.SettlementBalance = SettlementBalanceReport{
		Address: settlement, Total: balance.Total.String(),
		Hold: hold.String(), Available: balance.Available.String(), Observed: true,
	}
	o.info(ctx, "final settlement balance observed",
		slog.String("event", "funding_settlement_final_balance_observed"),
		slog.String("settlement", settlement),
		slog.String("total", balance.Total.String()),
		slog.String("hold", hold.String()),
		slog.String("available", balance.Available.String()))
}
