package funding

import (
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

const (
	PhasePrepared         Phase   = "prepared"
	PhasePayoutPrepared   Phase   = "payout_prepared"
	PhasePayoutSubmitting Phase   = "payout_submitting"
	PhasePayoutConfirmed  Phase   = "payout_confirmed"
	PhaseBlocked          Phase   = "blocked"
	TriggerUTC            Trigger = "utc_midnight"
	TriggerRunOnStart     Trigger = "run_on_start"
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
	Prepared    exchange.PreparedAction `json:"prepared"`
	TotalBefore string                  `json:"total_before"`
}

type RunState struct {
	RunID         string         `json:"run_id"`
	Trigger       Trigger        `json:"trigger"`
	UTCDate       string         `json:"utc_date"`
	Phase         Phase          `json:"phase"`
	Manifest      Manifest       `json:"manifest"`
	Payout        *PayoutJournal `json:"payout,omitempty"`
	BlockedReason string         `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}
