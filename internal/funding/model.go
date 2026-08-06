package funding

import (
	"fmt"
	"time"

	"builder-code-bot/internal/hyperliquid/exchange"
	"builder-code-bot/internal/hyperliquid/info"
)

type Record struct {
	ID            uint64 `json:"id"`
	PeriodStartAt uint64 `json:"period_start_at"`
	Amount        string `json:"amount"`
}

type Phase string

type Trigger string

type PayoutKind string

const (
	PhasePrepared           Phase   = "prepared"
	PhasePayoutPrepared     Phase   = "payout_prepared"
	PhasePayoutSubmitting   Phase   = "payout_submitting"
	PhasePayoutReplaying    Phase   = "payout_replaying"
	PhasePayoutConfirmed    Phase   = "payout_confirmed"
	PhaseCompleted          Phase   = "completed"
	PhaseBlocked            Phase   = "blocked"
	TriggerUTC              Trigger = "utc_payout"
	TriggerUTCConsolidation Trigger = "utc_consolidation"
	TriggerRunOnStart       Trigger = "run_on_start"
	TriggerConsolidateOnly  Trigger = "consolidate_only"

	PayoutKindPointsRebate PayoutKind = "points_rebate"
	PayoutKindReferral     PayoutKind = "referral"
)

type Manifest struct {
	Records     []Record    `json:"records"`
	RawTotal    string      `json:"raw_total"`
	PayoutTotal string      `json:"payout_total"`
	Token       *info.Token `json:"token,omitempty"`
	Settlement  string      `json:"settlement"`
	Recipient   string      `json:"recipient"`
}

type ManifestInput struct {
	Records    []Record
	Token      *info.Token
	Settlement string
	Recipient  string
}

// PayoutJournal is the only chain action persisted for recovery. Builder
// claims and sweeps only move funds between operator-controlled accounts and
// are therefore safe to repeat from current balances.
type PayoutJournal struct {
	Prepared       exchange.PreparedAction `json:"prepared"`
	TotalBefore    string                  `json:"total_before"`
	SubmitAttempts int                     `json:"submit_attempts"`
}

// PayoutTargetState freezes one database-backed payout independently from the
// other target. Record IDs are only meaningful within the target repository,
// so the kind is persisted alongside the manifest and recovery journal.
type PayoutTargetState struct {
	Kind                 PayoutKind     `json:"kind"`
	Phase                Phase          `json:"phase"`
	Manifest             Manifest       `json:"manifest"`
	Payout               *PayoutJournal `json:"payout,omitempty"`
	BlockedReason        string         `json:"blocked_reason,omitempty"`
	ConfirmationEvidence string         `json:"confirmation_evidence,omitempty"`
}

type RunState struct {
	RunID     string              `json:"run_id"`
	Trigger   Trigger             `json:"trigger"`
	UTCDate   string              `json:"utc_date"`
	Targets   []PayoutTargetState `json:"targets,omitempty"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`

	// Manifest is transient scratch state for the UTC 00:00 internal
	// consolidation path. That path never persists RunState.
	Manifest Manifest `json:"-"`
}

func (s *RunState) activePayoutTarget() (*PayoutTargetState, int, error) {
	if s == nil {
		return nil, -1, fmt.Errorf("funding run state is nil")
	}
	if len(s.Targets) != 2 || s.Targets[0].Kind != PayoutKindPointsRebate ||
		s.Targets[1].Kind != PayoutKindReferral {
		return nil, -1, fmt.Errorf("funding payout targets are missing or out of order")
	}
	for index := range s.Targets {
		if s.Targets[index].Phase != PhaseCompleted {
			return &s.Targets[index], index, nil
		}
	}
	return nil, len(s.Targets), nil
}
