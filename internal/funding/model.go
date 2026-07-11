package funding

import (
	"encoding/json"
	"time"

	"hyperliquid-builder-code-bot/internal/hyperliquid/exchange"
	"hyperliquid-builder-code-bot/internal/hyperliquid/info"
)

type Record struct {
	ID            uint64 `json:"id"`
	PeriodStartAt uint64 `json:"period_start_at"`
	Amount        string `json:"amount"`
}

type Phase string

type Trigger string

type ActionPhase string

const (
	ActionPrepared    ActionPhase = "prepared"
	ActionSubmitting  ActionPhase = "submitting"
	ActionAccepted    ActionPhase = "accepted"
	ActionRejected    ActionPhase = "rejected"
	ActionUnknown     ActionPhase = "unknown"
	ActionZeroBalance ActionPhase = "zero_balance"
)

const (
	PhasePrepared         Phase   = "prepared"
	PhaseConsolidating    Phase   = "consolidating"
	PhaseFunded           Phase   = "funded"
	PhasePayoutSubmitting Phase   = "payout_submitting"
	PhasePayoutAccepted   Phase   = "payout_accepted"
	PhaseDBUpdating       Phase   = "db_updating"
	PhaseCompleted        Phase   = "completed"
	PhaseBlocked          Phase   = "blocked"
	TriggerUTC            Trigger = "utc_midnight"
	TriggerRunOnStart     Trigger = "run_on_start"
)

type Manifest struct {
	Records      []Record    `json:"records"`
	RawTotal     string      `json:"raw_total"`
	PayoutTotal  string      `json:"payout_total"`
	Token        *info.Token `json:"token,omitempty"`
	Builders     []string    `json:"builders"`
	Settlement   string      `json:"settlement"`
	Recipient    string      `json:"recipient"`
	ManifestHash string      `json:"manifest_hash"`
}

type ManifestInput struct {
	Records    []Record
	Token      *info.Token
	Builders   []string
	Settlement string
	Recipient  string
}

type ActionProgress struct {
	Phase          ActionPhase              `json:"phase"`
	Prepared       *exchange.PreparedAction `json:"prepared,omitempty"`
	SubmitAttempts int                      `json:"submit_attempts,omitempty"`
	Unconfirmable  bool                     `json:"unconfirmable,omitempty"`
	BalanceBefore  string                   `json:"balance_before,omitempty"`
	BalanceAfter   string                   `json:"balance_after,omitempty"`
	Response       json.RawMessage          `json:"response,omitempty"`
	Evidence       *info.LedgerUpdate       `json:"evidence,omitempty"`
}

type BuilderProgress struct {
	Name    string         `json:"name"`
	Address string         `json:"address"`
	Claim   ActionProgress `json:"claim"`
	Sweep   ActionProgress `json:"sweep"`
}

type RunState struct {
	RunID         string            `json:"run_id"`
	Trigger       Trigger           `json:"trigger"`
	UTCDate       string            `json:"utc_date"`
	Phase         Phase             `json:"phase"`
	Manifest      Manifest          `json:"manifest"`
	Builders      []BuilderProgress `json:"builders"`
	FinalPayout   *ActionProgress   `json:"final_payout,omitempty"`
	DBCompleted   bool              `json:"db_completed"`
	BlockedReason string            `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}
