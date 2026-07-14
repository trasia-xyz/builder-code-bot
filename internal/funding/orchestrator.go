package funding

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"

	"github.com/shopspring/decimal"
)

type Builder struct {
	Name    string
	Address string
}

type OrchestratorConfig struct {
	Repository Repository
	Store      StateStore
	Chain      Chain
	Notifier   Notifier
	Logger     Logger
	Builders   []Builder
	Settlement string
	Recipient  string
	Clock      Clock
	Nonce      NonceSource
	Sleeper    Sleeper
}

type Orchestrator struct {
	repository Repository
	store      StateStore
	chain      Chain
	notifier   Notifier
	logger     Logger
	builders   []Builder
	settlement string
	recipient  string
	clock      Clock
	nonce      NonceSource
	sleeper    Sleeper
	alertMu    sync.Mutex
	alerts     map[string]struct{}
}

const (
	builderConvergenceAttempts = 5
	builderConvergenceInterval = time.Second
)

func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Repository == nil || cfg.Store == nil || cfg.Chain == nil || cfg.Clock == nil || cfg.Nonce == nil {
		return nil, fmt.Errorf("funding orchestrator dependencies are required")
	}
	if len(cfg.Builders) == 0 || strings.TrimSpace(cfg.Settlement) == "" || strings.TrimSpace(cfg.Recipient) == "" {
		return nil, fmt.Errorf("funding accounts are required")
	}
	seen := make(map[string]struct{}, len(cfg.Builders))
	for _, builder := range cfg.Builders {
		address := strings.ToLower(strings.TrimSpace(builder.Address))
		if strings.TrimSpace(builder.Name) == "" || address == "" {
			return nil, fmt.Errorf("builder name and address are required")
		}
		if strings.EqualFold(address, strings.TrimSpace(cfg.Settlement)) {
			return nil, fmt.Errorf("settlement account must not be a builder")
		}
		if _, duplicate := seen[address]; duplicate {
			return nil, fmt.Errorf("duplicate builder address %q", builder.Address)
		}
		seen[address] = struct{}{}
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Settlement), strings.TrimSpace(cfg.Recipient)) {
		return nil, fmt.Errorf("settlement and recipient accounts must differ")
	}
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	return &Orchestrator{
		repository: cfg.Repository, store: cfg.Store, chain: cfg.Chain,
		notifier: cfg.Notifier, logger: cfg.Logger,
		builders:   append([]Builder(nil), cfg.Builders...),
		settlement: cfg.Settlement, recipient: cfg.Recipient,
		clock: cfg.Clock, nonce: cfg.Nonce, sleeper: sleeper,
		alerts: make(map[string]struct{}),
	}, nil
}

type runReport struct {
	trigger     Trigger
	state       *RunState
	recordCount int
	recordsRead bool
	outcome     string
}

func (o *Orchestrator) RunNew(ctx context.Context, trigger Trigger) (err error) {
	report := runReport{trigger: trigger}
	defer func() { o.reportRun(ctx, report, err) }()
	return o.runNew(ctx, trigger, &report)
}

func (o *Orchestrator) runNew(ctx context.Context, trigger Trigger, report *runReport) error {
	current, _, err := o.loadCurrent(ctx)
	if err != nil {
		return err
	}
	if current != nil {
		return fmt.Errorf("current funding run %s must be recovered before starting a new run", current.RunID)
	}
	records, err := o.repository.ListPending(ctx)
	if err != nil {
		return err
	}
	report.recordCount, report.recordsRead = len(records), true
	if len(records) == 0 {
		report.outcome = "no_data"
		o.info(ctx, "no pending funding records", slog.String("event", "funding_no_data"))
		return nil
	}
	run, err := o.newState(trigger)
	if err != nil {
		return err
	}
	report.state = &run
	o.info(ctx, "funding run started",
		slog.String("event", "funding_run_started"), slog.String("run_id", run.RunID),
		slog.String("trigger", string(trigger)))

	input := ManifestInput{Records: records, Settlement: o.settlement, Recipient: o.recipient}
	_, payout, validationErr := CalculateTotals(records)
	if validationErr != nil {
		report.outcome = "failed_validation"
		run.Manifest = buildValidationArchiveManifest(input)
		if archiveErr := o.store.Archive(ctx, run, "failed_validation"); archiveErr != nil {
			return fmt.Errorf("validate records: %w; archive failure: %v", validationErr, archiveErr)
		}
		o.alertOnce(ctx, run.RunID, "validation_failed", "funding snapshot validation failed")
		return validationErr
	}
	if payout != "0" {
		token, tokenErr := o.chain.CanonicalUSDC(ctx)
		if tokenErr != nil {
			return tokenErr
		}
		input.Token = &token
	}
	run.Manifest, err = BuildManifest(input)
	if err != nil {
		return err
	}
	report.outcome = "completed"
	if err := o.save(ctx, &run, PhasePrepared); err != nil {
		return err
	}
	if payout == "0" {
		return o.completeDatabase(ctx, &run)
	}
	return o.runPositive(ctx, &run)
}

func buildValidationArchiveManifest(input ManifestInput) Manifest {
	records := append([]Record(nil), input.Records...)
	slices.SortFunc(records, func(a, b Record) int {
		if a.PeriodStartAt != b.PeriodStartAt {
			return cmp.Compare(a.PeriodStartAt, b.PeriodStartAt)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return Manifest{
		Records: records, RawTotal: "unavailable", PayoutTotal: "unavailable",
		Settlement: input.Settlement, Recipient: input.Recipient,
	}
}

func (o *Orchestrator) runPositive(ctx context.Context, state *RunState) error {
	for _, builder := range o.builders {
		if err := ctx.Err(); err != nil {
			return err
		}
		prepared, err := o.chain.PrepareClaim(builder.Address, o.nonce.Next())
		if err != nil {
			o.builderFailure(ctx, state, "claimRewards", "prepare")
			continue
		}
		result, _ := o.chain.Submit(ctx, prepared)
		if !result.Accepted {
			o.builderFailure(ctx, state, "claimRewards", submitResultName(result))
		}
	}
	token := *state.Manifest.Token
	payout, err := decimalFromString(state.Manifest.PayoutTotal)
	if err != nil {
		return err
	}
	var settlement info.SpotBalanceAmounts
	for attempt := 1; attempt <= builderConvergenceAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, builder := range o.builders {
			if err := ctx.Err(); err != nil {
				return err
			}
			balance, balanceErr := o.chain.SpotBalance(ctx, builder.Address, token)
			if balanceErr != nil {
				o.builderFailure(ctx, state, "spotBalance", "query")
				continue
			}
			if !balance.Available.IsPositive() {
				continue
			}
			prepared, prepareErr := o.chain.PrepareSpotSend(builder.Address, state.Manifest.Settlement, token, balance.Available, o.nonce.Next())
			if prepareErr != nil {
				o.builderFailure(ctx, state, "spotSend", "prepare")
				continue
			}
			result, _ := o.chain.Submit(ctx, prepared)
			if !result.Accepted {
				o.builderFailure(ctx, state, "spotSend", submitResultName(result))
			}
		}
		settlement, err = o.chain.SpotBalance(ctx, state.Manifest.Settlement, token)
		if err == nil && !settlement.Available.LessThan(payout) {
			return o.executePayout(ctx, state, settlement.Total, payout)
		}
		if attempt < builderConvergenceAttempts {
			if sleepErr := o.sleeper.Sleep(ctx, builderConvergenceInterval); sleepErr != nil {
				return sleepErr
			}
		}
	}
	o.alertOnce(ctx, state.RunID, "settlement_underfunded", "settlement available balance is below required payout")
	if err != nil {
		return fmt.Errorf("query settlement spot balance after %d attempts: %w", builderConvergenceAttempts, err)
	}
	return fmt.Errorf("settlement available balance %s is below payout %s after %d attempts", settlement.Available, payout, builderConvergenceAttempts)
}

func submitResultName(result exchange.SubmitResult) string {
	if result.Rejected {
		return "rejected"
	}
	return "unknown"
}

func (o *Orchestrator) builderFailure(ctx context.Context, state *RunState, action, outcome string) {
	o.warn(ctx, "builder action did not complete",
		slog.String("event", "funding_builder_action_failed"),
		slog.String("run_id", state.RunID), slog.String("action_kind", action),
		slog.String("outcome", outcome))
	o.alertOnce(ctx, state.RunID, "builder_"+action+"_failed", action+" action did not complete")
}

func (o *Orchestrator) newState(trigger Trigger) (RunState, error) {
	runID, err := NewRunID()
	if err != nil {
		return RunState{}, err
	}
	now := o.clock.Now().UTC()
	return RunState{RunID: runID, Trigger: trigger, UTCDate: now.Format(time.DateOnly), CreatedAt: now, UpdatedAt: now}, nil
}

func (o *Orchestrator) save(ctx context.Context, state *RunState, phase Phase) error {
	state.Phase = phase
	state.UpdatedAt = o.clock.Now().UTC()
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.info(ctx, "funding phase persisted",
		slog.String("event", "funding_phase_persisted"), slog.String("run_id", state.RunID),
		slog.String("phase", string(phase)))
	return nil
}

func (o *Orchestrator) completeDatabase(ctx context.Context, state *RunState) error {
	ids := make([]uint64, len(state.Manifest.Records))
	for i, record := range state.Manifest.Records {
		ids[i] = record.ID
	}
	o.info(ctx, "funding database completion started",
		slog.String("event", "funding_database_completion_started"),
		slog.String("run_id", state.RunID), slog.Int("record_count", len(ids)))
	if err := o.repository.Complete(ctx, ids); err != nil {
		o.alertOnce(ctx, state.RunID, "database_completion_failed", "funding database completion failed")
		return err
	}
	if err := o.store.Archive(ctx, *state, "completed"); err != nil {
		return err
	}
	if err := o.store.Clear(ctx); err != nil {
		return err
	}
	o.forgetAlerts(state.RunID)
	return nil
}

func (o *Orchestrator) reportRun(ctx context.Context, report runReport, runErr error) {
	if o.notifier == nil {
		return
	}
	status, subject := "succeeded", "Funding run succeeded"
	if runErr != nil {
		status, subject = "failed", "Funding run failed"
		if report.outcome == "" || report.outcome == "completed" {
			report.outcome = "failed"
		}
	}
	if report.outcome == "" {
		report.outcome = "completed"
	}
	lines := []string{"status: " + status, "trigger: " + string(report.trigger), "outcome: " + report.outcome}
	if report.recordsRead {
		lines = append(lines, fmt.Sprintf("record count: %d", report.recordCount))
	}
	if report.state != nil {
		lines = append(lines, "run id: "+report.state.RunID, "utc date: "+report.state.UTCDate)
		if report.state.Phase != "" {
			lines = append(lines, "phase: "+string(report.state.Phase))
		}
		if report.state.Manifest.PayoutTotal != "" {
			lines = append(lines, "payout total: "+report.state.Manifest.PayoutTotal)
		}
	}
	if err := o.notifier.Report(ctx, subject, strings.Join(lines, "\n")); err != nil {
		o.error(ctx, "funding report delivery failed", slog.String("event", "funding_report_delivery_failed"))
	}
}

func (o *Orchestrator) alert(ctx context.Context, key, message string) {
	if o.notifier != nil {
		_ = o.notifier.Alert(ctx, key, message)
	}
}

func (o *Orchestrator) alertOnce(ctx context.Context, runID, key, message string) {
	dedupeKey := runID + ":" + key
	o.alertMu.Lock()
	if _, exists := o.alerts[dedupeKey]; exists {
		o.alertMu.Unlock()
		return
	}
	o.alerts[dedupeKey] = struct{}{}
	o.alertMu.Unlock()
	o.alert(ctx, dedupeKey, message)
}

func (o *Orchestrator) forgetAlerts(runID string) {
	o.alertMu.Lock()
	defer o.alertMu.Unlock()
	for key := range o.alerts {
		if strings.HasPrefix(key, runID+":") {
			delete(o.alerts, key)
		}
	}
}

func (o *Orchestrator) info(ctx context.Context, message string, attrs ...slog.Attr) {
	if o.logger != nil {
		o.logger.Info(ctx, message, attrs...)
	}
}
func (o *Orchestrator) warn(ctx context.Context, message string, attrs ...slog.Attr) {
	if o.logger != nil {
		o.logger.Warn(ctx, message, attrs...)
	}
}
func (o *Orchestrator) error(ctx context.Context, message string, attrs ...slog.Attr) {
	if o.logger != nil {
		o.logger.Error(ctx, message, attrs...)
	}
}

func (o *Orchestrator) loadCurrent(ctx context.Context) (*RunState, StateLoadMetadata, error) {
	current, metadata, err := o.store.LoadWithMetadata(ctx)
	if err == nil && metadata.RecoveredFromBackup {
		runID := "state-store"
		if current != nil && current.RunID != "" {
			runID = current.RunID
		}
		o.warn(ctx, "funding state recovered from backup",
			slog.String("event", "state_snapshot_recovered_from_backup"),
			slog.String("run_id", runID), slog.Bool("primary_invalid", metadata.PrimaryInvalid))
		o.alertOnce(ctx, runID, "state_corruption", "funding state recovered from backup")
	}
	return current, metadata, err
}

func decimalFromString(value string) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse persisted payout total: %w", err)
	}
	return parsed, nil
}

func clonePrepared(action exchange.PreparedAction) exchange.PreparedAction {
	action.RequestBody = append([]byte(nil), action.RequestBody...)
	return action
}
