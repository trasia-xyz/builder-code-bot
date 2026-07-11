package funding

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"

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

func NewOrchestrator(cfg OrchestratorConfig) (*Orchestrator, error) {
	if cfg.Repository == nil || cfg.Store == nil || cfg.Chain == nil || cfg.Clock == nil || cfg.Nonce == nil {
		return nil, fmt.Errorf("funding orchestrator dependencies are required")
	}
	if len(cfg.Builders) == 0 || strings.TrimSpace(cfg.Settlement) == "" || strings.TrimSpace(cfg.Recipient) == "" {
		return nil, fmt.Errorf("funding accounts are required")
	}
	seenBuilders := make(map[string]struct{}, len(cfg.Builders))
	for _, builder := range cfg.Builders {
		address := strings.ToLower(strings.TrimSpace(builder.Address))
		if strings.TrimSpace(builder.Name) == "" || address == "" {
			return nil, fmt.Errorf("builder name and address are required")
		}
		if address == strings.ToLower(strings.TrimSpace(cfg.Settlement)) {
			return nil, fmt.Errorf("settlement account must not be a builder")
		}
		if _, duplicate := seenBuilders[address]; duplicate {
			return nil, fmt.Errorf("duplicate builder address %q", builder.Address)
		}
		seenBuilders[address] = struct{}{}
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Settlement), strings.TrimSpace(cfg.Recipient)) {
		return nil, fmt.Errorf("settlement and recipient accounts must differ")
	}
	builders := append([]Builder(nil), cfg.Builders...)
	sleeper := cfg.Sleeper
	if sleeper == nil {
		sleeper = timerSleeper{}
	}
	return &Orchestrator{
		repository: cfg.Repository,
		store:      cfg.Store,
		chain:      cfg.Chain,
		notifier:   cfg.Notifier,
		logger:     cfg.Logger,
		builders:   builders,
		settlement: cfg.Settlement,
		recipient:  cfg.Recipient,
		clock:      cfg.Clock,
		nonce:      cfg.Nonce,
		sleeper:    sleeper,
		alerts:     make(map[string]struct{}),
	}, nil
}

func (o *Orchestrator) RunNew(ctx context.Context, trigger Trigger) error {
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
	run, err := o.newState(trigger)
	if err != nil {
		return err
	}
	o.info(ctx, "funding run started",
		slog.String("event", "funding_run_started"),
		slog.String("run_id", run.RunID),
		slog.String("trigger", string(trigger)),
	)
	o.info(ctx, "funding snapshot loaded",
		slog.String("event", "funding_snapshot_loaded"),
		slog.String("run_id", run.RunID),
		slog.Int("record_count", len(records)),
	)
	manifestInput := ManifestInput{
		Records: records, Builders: o.builderAddresses(), Settlement: o.settlement, Recipient: o.recipient,
	}
	if len(records) == 0 {
		run.Manifest, err = buildTerminalArchiveManifest(manifestInput, false)
		if err != nil {
			return err
		}
		return o.store.Archive(ctx, run, "no_data")
	}
	_, payout, validationErr := CalculateTotals(records)
	if validationErr != nil {
		run.Manifest, err = buildTerminalArchiveManifest(manifestInput, true)
		if err != nil {
			return err
		}
		if archiveErr := o.store.Archive(ctx, run, "failed_validation"); archiveErr != nil {
			o.alertOnce(ctx, run.RunID, "validation_failed", "funding snapshot validation failed")
			return fmt.Errorf("validate records: %w; archive failure: %v", validationErr, archiveErr)
		}
		o.warn(ctx, "funding snapshot validation failed",
			slog.String("event", "funding_validation_failed"),
			slog.String("run_id", run.RunID),
		)
		o.alertOnce(ctx, run.RunID, "validation_failed", "funding snapshot validation failed")
		return validationErr
	}

	if payout != "0" {
		token, tokenErr := o.chain.CanonicalUSDC(ctx)
		if tokenErr != nil {
			return tokenErr
		}
		manifestInput.Token = &token
	}
	manifest, err := BuildManifest(manifestInput)
	if err != nil {
		return err
	}
	run.Manifest = manifest
	run.Builders = o.builderProgress()
	if err := o.save(ctx, &run, PhasePrepared); err != nil {
		return err
	}
	if payout == "0" {
		return o.completeDatabase(ctx, &run)
	}
	return o.runPositive(ctx, &run)
}

func buildTerminalArchiveManifest(input ManifestInput, validationFailed bool) (Manifest, error) {
	if !validationFailed {
		return BuildManifest(input)
	}
	records := append([]Record(nil), input.Records...)
	sort.Slice(records, func(i, j int) bool {
		if records[i].PeriodStartAt != records[j].PeriodStartAt {
			return records[i].PeriodStartAt < records[j].PeriodStartAt
		}
		return records[i].ID < records[j].ID
	})
	manifest := Manifest{
		Records:     records,
		RawTotal:    "unavailable",
		PayoutTotal: "unavailable",
		Builders:    append([]string(nil), input.Builders...),
		Settlement:  input.Settlement,
		Recipient:   input.Recipient,
	}
	hash, err := HashManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.ManifestHash = hash
	return manifest, nil
}

func (o *Orchestrator) runPositive(ctx context.Context, state *RunState) error {
	if err := o.save(ctx, state, PhaseConsolidating); err != nil {
		return err
	}
	for i := range state.Builders {
		if err := o.executeClaim(ctx, state, i); err != nil {
			return err
		}
	}
	for i := range state.Builders {
		if err := o.executeSweep(ctx, state, i); err != nil {
			return err
		}
	}

	token := *state.Manifest.Token
	balance, err := o.chain.AvailableSpotBalance(ctx, state.Manifest.Settlement, token)
	if err != nil {
		o.alertOnce(ctx, state.RunID, "settlement_balance_failed", "settlement balance query failed")
		return err
	}
	payout, err := decimalFromString(state.Manifest.PayoutTotal)
	if err != nil {
		return err
	}
	if balance.LessThan(payout) {
		state.BlockedReason = fmt.Sprintf("settlement balance %s is below payout %s", balance, payout)
		if err := o.save(ctx, state, PhaseBlocked); err != nil {
			return err
		}
		o.alertOnce(ctx, state.RunID, "settlement_underfunded", "settlement balance is below required payout")
		return fmt.Errorf("%s", state.BlockedReason)
	}
	if err := o.save(ctx, state, PhaseFunded); err != nil {
		return err
	}
	return o.executePayout(ctx, state, balance, payout)
}

func (o *Orchestrator) newState(trigger Trigger) (RunState, error) {
	runID, err := NewRunID()
	if err != nil {
		return RunState{}, err
	}
	now := o.clock.Now().UTC()
	return RunState{
		RunID:     runID,
		Trigger:   trigger,
		UTCDate:   now.Format(time.DateOnly),
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func (o *Orchestrator) save(ctx context.Context, state *RunState, phase Phase) error {
	state.Phase = phase
	state.UpdatedAt = o.clock.Now().UTC()
	if err := o.store.Save(ctx, *state); err != nil {
		return err
	}
	o.info(ctx, "funding phase persisted",
		slog.String("event", "funding_phase_persisted"),
		slog.String("run_id", state.RunID),
		slog.String("phase", string(phase)),
	)
	return nil
}

func (o *Orchestrator) completeDatabase(ctx context.Context, state *RunState) error {
	if err := o.save(ctx, state, PhaseDBUpdating); err != nil {
		return o.databaseFailure(ctx, state, err)
	}
	o.info(ctx, "funding database completion started",
		slog.String("event", "funding_database_completion_started"),
		slog.String("run_id", state.RunID),
		slog.Int("record_count", len(state.Manifest.Records)),
	)
	ids := make([]uint64, len(state.Manifest.Records))
	for i, record := range state.Manifest.Records {
		ids[i] = record.ID
	}
	if err := o.repository.Complete(ctx, ids); err != nil {
		return o.databaseFailure(ctx, state, err)
	}
	state.DBCompleted = true
	if err := o.save(ctx, state, PhaseCompleted); err != nil {
		return o.databaseFailure(ctx, state, err)
	}
	o.info(ctx, "funding database completed",
		slog.String("event", "funding_database_completed"),
		slog.String("run_id", state.RunID),
		slog.Int("record_count", len(ids)),
	)
	if err := o.store.Archive(ctx, *state, "completed"); err != nil {
		return o.databaseFailure(ctx, state, err)
	}
	if err := o.store.Clear(ctx); err != nil {
		return o.databaseFailure(ctx, state, err)
	}
	o.forgetAlerts(state.RunID)
	return nil
}

func (o *Orchestrator) builderAddresses() []string {
	addresses := make([]string, len(o.builders))
	for i, builder := range o.builders {
		addresses[i] = builder.Address
	}
	return addresses
}

func (o *Orchestrator) builderProgress() []BuilderProgress {
	progress := make([]BuilderProgress, len(o.builders))
	for i, builder := range o.builders {
		progress[i] = BuilderProgress{Name: builder.Name, Address: builder.Address}
	}
	return progress
}

func (o *Orchestrator) alert(ctx context.Context, key, message string) {
	if o.notifier != nil {
		if err := o.notifier.Alert(ctx, key, message); err != nil {
			o.error(ctx, "funding notification delivery failed",
				slog.String("event", "funding_notification_delivery_failed"),
				slog.String("alert_key", key),
			)
		}
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
	prefix := runID + ":"
	o.alertMu.Lock()
	for key := range o.alerts {
		if strings.HasPrefix(key, prefix) {
			delete(o.alerts, key)
		}
	}
	o.alertMu.Unlock()
}

func (o *Orchestrator) databaseFailure(ctx context.Context, state *RunState, err error) error {
	o.error(ctx, "funding database completion failed",
		slog.String("event", "funding_database_completion_failed"),
		slog.String("run_id", state.RunID),
		slog.String("phase", string(state.Phase)),
	)
	o.alertOnce(ctx, state.RunID, "database_completion_failed", "funding database completion failed")
	return err
}

func (o *Orchestrator) actionEvent(ctx context.Context, state *RunState, event, kind string, phase ActionPhase) {
	o.info(ctx, "funding action state changed",
		slog.String("event", event),
		slog.String("run_id", state.RunID),
		slog.String("action_kind", kind),
		slog.String("action_phase", string(phase)),
	)
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
	var (
		current  *RunState
		metadata StateLoadMetadata
		err      error
	)
	if store, ok := o.store.(StateStoreWithLoadMetadata); ok {
		current, metadata, err = store.LoadWithMetadata(ctx)
	} else {
		current, err = o.store.Load(ctx)
	}
	if err != nil {
		o.warn(ctx, "funding state load failed",
			slog.String("event", "state_load_failed"),
		)
		o.alertOnce(ctx, "state-store", "state_load_failed", "funding state could not be loaded")
		return nil, StateLoadMetadata{}, err
	}
	if metadata.RecoveredFromBackup {
		runID := "state-store"
		if current != nil && current.RunID != "" {
			runID = current.RunID
		}
		o.warn(ctx, "funding state recovered from backup",
			slog.String("event", "state_snapshot_recovered_from_backup"),
			slog.String("run_id", runID),
			slog.Bool("primary_invalid", metadata.PrimaryInvalid),
		)
		if metadata.PrimaryInvalid {
			o.alertOnce(ctx, runID, "state_corruption", "primary funding state was invalid; recovered from backup")
		}
	}
	return current, metadata, nil
}

func submitPhase(result exchange.SubmitResult) ActionPhase {
	if result.Accepted {
		return ActionAccepted
	}
	if result.Rejected {
		return ActionRejected
	}
	return ActionUnknown
}

func decimalFromString(value string) (decimal.Decimal, error) {
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, fmt.Errorf("parse persisted payout total: %w", err)
	}
	return parsed, nil
}

func clonePrepared(action exchange.PreparedAction) exchange.PreparedAction {
	action.RequestBody = append(json.RawMessage(nil), action.RequestBody...)
	return action
}

func ptrPrepared(action exchange.PreparedAction) *exchange.PreparedAction { return &action }
