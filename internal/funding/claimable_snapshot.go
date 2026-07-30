package funding

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

const (
	claimableSnapshotQueryAttempts = 3
	claimableSnapshotRetryInterval = 2 * time.Second
)

type ClaimableSnapshot struct {
	UTCDate          string                     `json:"utc_date"`
	CapturedAt       time.Time                  `json:"captured_at"`
	TokenQueryFailed bool                       `json:"token_query_failed,omitempty"`
	Builders         []ClaimableSnapshotBuilder `json:"builders"`
}

type ClaimableSnapshotBuilder struct {
	Address       string `json:"address"`
	ClaimableUSDC string `json:"claimable_usdc,omitempty"`
	QueryFailed   bool   `json:"query_failed,omitempty"`
}

func (s ClaimableSnapshot) Validate() error {
	date, err := time.Parse(time.DateOnly, s.UTCDate)
	if err != nil || date.Format(time.DateOnly) != s.UTCDate {
		return fmt.Errorf("invalid claimable snapshot UTC date %q", s.UTCDate)
	}
	if s.CapturedAt.IsZero() {
		return fmt.Errorf("claimable snapshot capture time is required")
	}
	if capturedDate := s.CapturedAt.UTC().Format(time.DateOnly); capturedDate != s.UTCDate {
		return fmt.Errorf(
			"claimable snapshot capture date %q does not match UTC date %q",
			capturedDate, s.UTCDate,
		)
	}
	if len(s.Builders) == 0 {
		return fmt.Errorf("claimable snapshot builders are required")
	}
	seen := make(map[string]struct{}, len(s.Builders))
	for index, builder := range s.Builders {
		address := strings.ToLower(strings.TrimSpace(builder.Address))
		if address == "" {
			return fmt.Errorf("claimable snapshot builder %d address is required", index)
		}
		if _, duplicate := seen[address]; duplicate {
			return fmt.Errorf("duplicate claimable snapshot builder address %q", builder.Address)
		}
		seen[address] = struct{}{}
		if builder.QueryFailed {
			if strings.TrimSpace(builder.ClaimableUSDC) != "" {
				return fmt.Errorf("failed claimable snapshot builder %q has an amount", builder.Address)
			}
			continue
		}
		amount, err := decimal.NewFromString(builder.ClaimableUSDC)
		if err != nil {
			return fmt.Errorf("parse claimable snapshot amount for %q: %w", builder.Address, err)
		}
		if amount.IsNegative() {
			return fmt.Errorf("claimable snapshot amount for %q is negative", builder.Address)
		}
	}
	if s.TokenQueryFailed {
		for _, builder := range s.Builders {
			if !builder.QueryFailed {
				return fmt.Errorf("token-failed claimable snapshot contains a successful builder query")
			}
		}
	}
	return nil
}

// CaptureClaimableSnapshot records one immutable, read-only claimable-USDC
// observation for the current UTC date. It never claims or moves funds.
func (o *Orchestrator) CaptureClaimableSnapshot(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("claimable snapshot context is nil")
	}
	if o == nil || o.claimableSnapshots == nil || o.chain == nil ||
		o.clock == nil || o.sleeper == nil {
		return fmt.Errorf("claimable snapshot runtime is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	capturedAt := o.clock.Now().UTC()
	utcDate := capturedAt.Format(time.DateOnly)
	existing, err := o.claimableSnapshots.LoadClaimableSnapshot(ctx, utcDate)
	if err != nil {
		return fmt.Errorf("check existing claimable snapshot: %w", err)
	}
	if existing != nil {
		o.info(ctx, "claimable snapshot already exists",
			slog.String("event", "funding_claimable_snapshot_skipped"),
			slog.String("utc_date", utcDate),
			slog.Time("captured_at", existing.CapturedAt))
		return nil
	}

	snapshot := ClaimableSnapshot{
		UTCDate:    utcDate,
		CapturedAt: capturedAt,
		Builders:   make([]ClaimableSnapshotBuilder, len(o.builders)),
	}
	for index, address := range o.builders {
		snapshot.Builders[index].Address = address
	}
	o.info(ctx, "claimable snapshot capture started",
		slog.String("event", "funding_claimable_snapshot_started"),
		slog.String("utc_date", utcDate),
		slog.Int("builder_count", len(o.builders)))

	token, tokenAttempts, err := o.querySnapshotToken(ctx, utcDate)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		snapshot.TokenQueryFailed = true
		for index := range snapshot.Builders {
			snapshot.Builders[index].QueryFailed = true
		}
		o.warn(ctx, "claimable snapshot token query failed",
			slog.String("event", "funding_claimable_snapshot_token_query_failed"),
			slog.String("utc_date", utcDate),
			slog.Int("attempt_count", tokenAttempts))
		if saveErr := o.claimableSnapshots.SaveClaimableSnapshot(ctx, snapshot); saveErr != nil {
			return fmt.Errorf("save token-failed claimable snapshot: %w", saveErr)
		}
		o.logClaimableSnapshotCompleted(ctx, snapshot, 0)
		return fmt.Errorf(
			"claimable snapshot incomplete: canonical USDC query failed after %d attempts",
			tokenAttempts,
		)
	}

	failed := 0
	for index, builder := range o.builders {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimable, attempts, queryErr := o.querySnapshotClaimable(
			ctx, utcDate, builder, token,
		)
		if queryErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			failed++
			snapshot.Builders[index].QueryFailed = true
			o.warn(ctx, "builder claimable snapshot query failed",
				slog.String("event", "funding_claimable_snapshot_builder_query_failed"),
				slog.String("utc_date", utcDate),
				slog.String("builder", builder),
				slog.Int("attempt_count", attempts))
			continue
		}
		snapshot.Builders[index].ClaimableUSDC = claimable.String()
		o.info(ctx, "builder claimable snapshot observed",
			slog.String("event", "funding_claimable_snapshot_builder_observed"),
			slog.String("utc_date", utcDate),
			slog.String("builder", builder),
			slog.Int("attempt", attempts),
			slog.String("claimable_usdc", claimable.String()))
	}
	if err := o.claimableSnapshots.SaveClaimableSnapshot(ctx, snapshot); err != nil {
		return fmt.Errorf("save claimable snapshot: %w", err)
	}
	o.logClaimableSnapshotCompleted(ctx, snapshot, len(o.builders)-failed)
	if failed > 0 {
		return fmt.Errorf(
			"claimable snapshot incomplete: %d of %d builder queries failed",
			failed, len(o.builders),
		)
	}
	return nil
}

func (o *Orchestrator) querySnapshotToken(
	ctx context.Context,
	utcDate string,
) (info.Token, int, error) {
	var lastErr error
	for attempt := 1; attempt <= claimableSnapshotQueryAttempts; attempt++ {
		token, err := o.chain.CanonicalUSDC(ctx)
		if err == nil {
			return token, attempt, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return info.Token{}, attempt, ctxErr
		}
		lastErr = err
		willRetry := attempt < claimableSnapshotQueryAttempts
		o.warn(ctx, "claimable snapshot token query attempt failed",
			slog.String("event", "funding_claimable_snapshot_token_query_attempt_failed"),
			slog.String("utc_date", utcDate),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", claimableSnapshotQueryAttempts),
			slog.Bool("will_retry", willRetry))
		if willRetry {
			if err := o.sleeper.Sleep(ctx, claimableSnapshotRetryInterval); err != nil {
				return info.Token{}, attempt, err
			}
		}
	}
	return info.Token{}, claimableSnapshotQueryAttempts, lastErr
}

func (o *Orchestrator) querySnapshotClaimable(
	ctx context.Context,
	utcDate, builder string,
	token info.Token,
) (decimal.Decimal, int, error) {
	var lastErr error
	for attempt := 1; attempt <= claimableSnapshotQueryAttempts; attempt++ {
		claimable, err := o.chain.ClaimableUSDC(ctx, builder, token)
		if err == nil {
			return claimable, attempt, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return decimal.Zero, attempt, ctxErr
		}
		lastErr = err
		willRetry := attempt < claimableSnapshotQueryAttempts
		o.warn(ctx, "builder claimable snapshot query attempt failed",
			slog.String("event", "funding_claimable_snapshot_builder_query_attempt_failed"),
			slog.String("utc_date", utcDate),
			slog.String("builder", builder),
			slog.Int("attempt", attempt),
			slog.Int("max_attempts", claimableSnapshotQueryAttempts),
			slog.Bool("will_retry", willRetry))
		if willRetry {
			if err := o.sleeper.Sleep(ctx, claimableSnapshotRetryInterval); err != nil {
				return decimal.Zero, attempt, err
			}
		}
	}
	return decimal.Zero, claimableSnapshotQueryAttempts, lastErr
}

func (o *Orchestrator) logClaimableSnapshotCompleted(
	ctx context.Context,
	snapshot ClaimableSnapshot,
	observed int,
) {
	o.info(ctx, "claimable snapshot capture completed",
		slog.String("event", "funding_claimable_snapshot_completed"),
		slog.String("utc_date", snapshot.UTCDate),
		slog.Time("captured_at", snapshot.CapturedAt),
		slog.Int("builder_count", len(snapshot.Builders)),
		slog.Int("observed_count", observed),
		slog.Bool("complete", observed == len(snapshot.Builders)))
}

func (o *Orchestrator) attachClaimableSnapshot(
	ctx context.Context,
	report *RunReport,
	utcDate string,
) {
	if o == nil || report == nil || o.claimableSnapshots == nil {
		return
	}
	summary := ClaimableSnapshotReport{
		UTCDate:      utcDate,
		BuilderCount: len(report.Builders),
	}
	snapshot, err := o.claimableSnapshots.LoadClaimableSnapshot(ctx, utcDate)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		summary.LoadFailed = true
		report.ClaimableSnapshot = summary
		o.warn(ctx, "claimable snapshot load failed",
			slog.String("event", "funding_claimable_snapshot_load_failed"),
			slog.String("utc_date", utcDate))
		if report.Trigger == TriggerUTC {
			report.addWarning("UTC 00:00 Claimable 快照：读取失败")
		}
		return
	}
	if snapshot == nil {
		summary.Missing = true
		report.ClaimableSnapshot = summary
		if report.Trigger == TriggerUTC {
			report.addWarning(fmt.Sprintf("UTC 00:00 Claimable 快照：未找到 %s 快照", utcDate))
		}
		return
	}

	summary.Observed = true
	summary.CapturedAt = snapshot.CapturedAt
	summary.TokenQueryFailed = snapshot.TokenQueryFailed
	entries := make(map[string]ClaimableSnapshotBuilder, len(snapshot.Builders))
	for _, builder := range snapshot.Builders {
		entries[strings.ToLower(strings.TrimSpace(builder.Address))] = builder
	}
	total := decimal.Zero
	for index := range report.Builders {
		builder := &report.Builders[index]
		entry, found := entries[strings.ToLower(strings.TrimSpace(builder.Address))]
		if !found || entry.QueryFailed {
			builder.SnapshotQueryFailed = true
			continue
		}
		amount, parseErr := decimal.NewFromString(entry.ClaimableUSDC)
		if parseErr != nil || amount.IsNegative() {
			builder.SnapshotQueryFailed = true
			continue
		}
		builder.SnapshotObserved = true
		builder.SnapshotClaimableUSDC = amount.String()
		summary.ObservedBuilders++
		total = total.Add(amount)
	}
	if summary.ObservedBuilders > 0 {
		summary.TotalClaimableUSDC = total.String()
	}
	report.ClaimableSnapshot = summary
	if report.Trigger == TriggerUTC && summary.ObservedBuilders != summary.BuilderCount {
		report.addWarning(fmt.Sprintf(
			"UTC 00:00 Claimable 快照：仅成功查询 %d/%d 个 Builder",
			summary.ObservedBuilders, summary.BuilderCount,
		))
	}
}
