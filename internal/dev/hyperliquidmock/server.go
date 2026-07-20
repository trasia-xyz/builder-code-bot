// Package hyperliquidmock provides a deterministic local Hyperliquid API for tests.
package hyperliquidmock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"builder-code-bot/internal/hyperliquid"
	"builder-code-bot/internal/hyperliquid/info"
	"builder-code-bot/internal/hyperliquid/signing"
)

// FailureMode controls the response to the next exchange request.
type FailureMode int

const (
	failureNone FailureMode = iota
	FailureRejected
	FailureAmbiguous
	FailureAmbiguousApplied
	FailureHTTPError
	FailureHTTPErrorApplied
)

// RecordedRequest is a sanitized, decoded request record.
type RecordedRequest struct {
	ActionType  string
	Destination string
}

type balanceKey struct {
	address string
	token   string
}

type exchangeOutcome struct {
	hash       string
	statusCode int
	body       []byte
}

type spotBalance struct {
	total decimal.Decimal
	hold  decimal.Decimal
}

// Server is an in-process Hyperliquid HTTP server with mutable account state.
type Server struct {
	URL string

	server *httptest.Server
	mu     sync.Mutex

	balances     map[balanceKey]spotBalance
	claimRewards map[balanceKey]decimal.Decimal
	rateLimits   map[string]info.UserRateLimit
	requests     []RecordedRequest
	nextFailure  FailureMode
	outcomes     map[string]exchangeOutcome
}

// New starts a mock server and registers its cleanup with t.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		balances:     make(map[balanceKey]spotBalance),
		claimRewards: make(map[balanceKey]decimal.Decimal),
		rateLimits:   make(map[string]info.UserRateLimit),
		outcomes:     make(map[string]exchangeOutcome),
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	s.URL = s.server.URL
	t.Cleanup(s.server.Close)
	return s
}

// SetSpotBalance sets an exact spot balance for an account and wire token.
func (s *Server) SetSpotBalance(address, token, amount string) {
	s.SetSpotBalanceWithHold(address, token, amount, "0")
}

// SetSpotBalanceWithHold sets exact total and held spot balances.
func (s *Server) SetSpotBalanceWithHold(address, token, total, hold string) {
	totalValue, err := decimal.NewFromString(total)
	if err != nil {
		panic(fmt.Sprintf("invalid mock total spot balance %q: %v", total, err))
	}
	holdValue, err := decimal.NewFromString(hold)
	if err != nil {
		panic(fmt.Sprintf("invalid mock held spot balance %q: %v", hold, err))
	}
	s.mu.Lock()
	s.balances[balanceKey{address: normalizeAddress(address), token: token}] = spotBalance{total: totalValue, hold: holdValue}
	s.mu.Unlock()
}

// SetClaimReward sets the balance credited by the next successful claim for an account.
func (s *Server) SetClaimReward(address, token, amount string) {
	value, err := decimal.NewFromString(amount)
	if err != nil {
		panic(fmt.Sprintf("invalid mock claim reward %q: %v", amount, err))
	}
	s.mu.Lock()
	s.claimRewards[balanceKey{address: normalizeAddress(address), token: token}] = value
	s.mu.Unlock()
}

// SetUserRateLimit sets the address-based action request counters for an account.
func (s *Server) SetUserRateLimit(address string, used, cap uint64) {
	s.mu.Lock()
	s.rateLimits[normalizeAddress(address)] = info.UserRateLimit{
		NRequestsUsed: used,
		NRequestsCap:  cap,
	}
	s.mu.Unlock()
}

// FailNextExchange injects one failure into the next exchange request.
func (s *Server) FailNextExchange(mode FailureMode) {
	s.mu.Lock()
	s.nextFailure = mode
	s.mu.Unlock()
}

// Requests returns a copy of all requests received by the server.
func (s *Server) Requests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RecordedRequest(nil), s.requests...)
}

func (s *Server) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	switch request.URL.Path {
	case "/info":
		s.handleInfo(w, body)
	case "/exchange":
		s.handleExchange(w, body)
	default:
		http.NotFound(w, request)
	}
}

func (s *Server) handleInfo(w http.ResponseWriter, body []byte) {
	var request struct {
		Type      string `json:"type"`
		User      string `json:"user"`
		StartTime uint64 `json:"startTime"`
		EndTime   uint64 `json:"endTime"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	s.mu.Lock()
	switch request.Type {
	case "spotMeta":
		token := canonicalToken()
		response := struct {
			Tokens []info.SpotToken `json:"tokens"`
		}{Tokens: []info.SpotToken{token}}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
	case "spotClearinghouseState":
		token := canonicalToken()
		wireToken := token.Name + ":" + token.TokenID
		key := balanceKey{address: normalizeAddress(request.User), token: wireToken}
		value := s.balances[key]
		response := struct {
			Balances []info.SpotBalance `json:"balances"`
		}{Balances: []info.SpotBalance{{Coin: token.Name, Token: token.Index, Total: value.total.String(), Hold: value.hold.String()}}}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
	case "referral":
		token := canonicalToken()
		wireToken := token.Name + ":" + token.TokenID
		reward := s.claimRewards[balanceKey{address: normalizeAddress(request.User), token: wireToken}]
		tokenState := struct {
			UnclaimedRewards string `json:"unclaimedRewards"`
		}{UnclaimedRewards: reward.String()}
		tokenToState := make([][2]any, 0, 1)
		if !reward.IsZero() {
			tokenToState = append(tokenToState, [2]any{token.Index, tokenState})
		}
		response := struct {
			UnclaimedRewards string   `json:"unclaimedRewards"`
			TokenToState     [][2]any `json:"tokenToState"`
		}{
			UnclaimedRewards: reward.String(),
			TokenToState:     tokenToState,
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
	case "userRateLimit":
		response, exists := s.rateLimits[normalizeAddress(request.User)]
		if !exists {
			response.NRequestsCap = 10_000
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, response)
	default:
		s.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported info type"})
	}
}

func (s *Server) handleExchange(w http.ResponseWriter, body []byte) {
	var request struct {
		Action    json.RawMessage   `json:"action"`
		Nonce     uint64            `json:"nonce"`
		Signature signing.Signature `json:"signature"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "err", "response": "invalid request"})
		return
	}
	var actionType struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(request.Action, &actionType); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "err", "response": "invalid action"})
		return
	}

	record := RecordedRequest{ActionType: actionType.Type}
	bodyHash := hashBytes(body)
	var (
		signerAddress string
		apply         func() error
	)
	switch actionType.Type {
	case "claimRewards":
		action := signing.Object{signing.F("type", "claimRewards")}
		var err error
		signerAddress, err = signing.RecoverL1ActionSigner(signing.L1ActionRecoverInput{
			Action: action, Nonce: request.Nonce, Network: hyperliquid.NetworkTestnet, Signature: request.Signature,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "err", "response": "invalid signature"})
			return
		}
		apply = func() error {
			token := canonicalToken()
			wireToken := token.Name + ":" + token.TokenID
			key := balanceKey{address: normalizeAddress(signerAddress), token: wireToken}
			reward := s.claimRewards[key]
			if !reward.GreaterThan(decimal.NewFromInt(1)) {
				return fmt.Errorf("must have more than 1 USDC of rewards to claim")
			}
			balance := s.balances[key]
			balance.total = balance.total.Add(reward)
			s.balances[key] = balance
			delete(s.claimRewards, key)
			return nil
		}
	case "spotSend":
		var action signing.SpotSendAction
		if err := json.Unmarshal(request.Action, &action); err != nil || action.Time != request.Nonce {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "err", "response": "invalid spot send"})
			return
		}
		if action.HyperliquidChain != "Testnet" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "err", "response": "wrong signing network"})
			return
		}
		var err error
		signerAddress, err = signing.RecoverSpotSendSigner(action, request.Signature)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"status": "err", "response": "invalid signature"})
			return
		}
		record.Destination = action.Destination
		apply = func() error { return s.applySpotSend(signerAddress, action) }
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"status": "err", "response": "unsupported action"})
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, record)
	idempotencyKey := normalizeAddress(signerAddress) + ":" + fmt.Sprint(request.Nonce)
	if prior, exists := s.outcomes[idempotencyKey]; exists {
		if prior.hash != bodyHash {
			s.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]string{"status": "err", "response": "nonce already used"})
			return
		}
		s.mu.Unlock()
		writeRaw(w, prior.statusCode, prior.body)
		return
	}
	mode := s.nextFailure
	s.nextFailure = failureNone
	statusCode := http.StatusOK
	response := []byte(`{"status":"ok","response":{"type":"default"}}`)
	shouldApply := mode != FailureRejected && mode != FailureAmbiguous && mode != FailureHTTPError
	switch mode {
	case FailureRejected:
		response = []byte(`{"status":"err","response":"injected rejection"}`)
	case FailureAmbiguous, FailureAmbiguousApplied:
		response = []byte(`{"status":"unknown","response":"injected ambiguity"}`)
	case FailureHTTPError, FailureHTTPErrorApplied:
		statusCode = http.StatusServiceUnavailable
		response = []byte(`{"status":"unknown","response":"injected transport failure"}`)
	}
	if shouldApply {
		if err := apply(); err != nil {
			response = []byte(`{"status":"err","response":"mutation rejected"}`)
			statusCode = http.StatusOK
		}
	}
	s.outcomes[idempotencyKey] = exchangeOutcome{hash: bodyHash, statusCode: statusCode, body: append([]byte(nil), response...)}
	s.mu.Unlock()
	writeRaw(w, statusCode, response)
}

func (s *Server) applySpotSend(signerAddress string, action signing.SpotSendAction) error {
	amount, err := decimal.NewFromString(action.Amount)
	if err != nil || !amount.IsPositive() {
		return fmt.Errorf("invalid amount")
	}
	source := balanceKey{address: normalizeAddress(signerAddress), token: action.Token}
	destination := balanceKey{address: normalizeAddress(action.Destination), token: action.Token}
	sourceBalance := s.balances[source]
	if sourceBalance.total.Sub(sourceBalance.hold).LessThan(amount) {
		return fmt.Errorf("insufficient balance")
	}
	sourceBalance.total = sourceBalance.total.Sub(amount)
	s.balances[source] = sourceBalance
	destinationBalance := s.balances[destination]
	destinationBalance.total = destinationBalance.total.Add(amount)
	s.balances[destination] = destinationBalance
	return nil
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func normalizeAddress(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func canonicalToken() info.SpotToken {
	return info.SpotToken{
		Name: "USDC", TokenID: "0", Index: 0, WeiDecimals: 6, IsCanonical: true,
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
		return
	}
	writeRaw(w, status, body)
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
