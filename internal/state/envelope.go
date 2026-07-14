package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"builder-code-bot/internal/funding"
)

const schemaVersion = 1

type envelope struct {
	SchemaVersion int              `json:"schema_version"`
	Checksum      string           `json:"checksum"`
	State         funding.RunState `json:"state"`
}

func marshalEnvelope(state funding.RunState) ([]byte, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("encode run state: %w", err)
	}
	digest := sha256.Sum256(stateJSON)
	data, err := json.Marshal(envelope{
		SchemaVersion: schemaVersion,
		Checksum:      hex.EncodeToString(digest[:]),
		State:         state,
	})
	if err != nil {
		return nil, fmt.Errorf("encode state envelope: %w", err)
	}
	return data, nil
}

func unmarshalEnvelope(data []byte) (*funding.RunState, error) {
	var saved envelope
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("decode state envelope: %w", err)
	}
	if saved.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("unsupported state schema version %d", saved.SchemaVersion)
	}
	stateJSON, err := json.Marshal(saved.State)
	if err != nil {
		return nil, fmt.Errorf("encode state for checksum: %w", err)
	}
	digest := sha256.Sum256(stateJSON)
	wantChecksum := hex.EncodeToString(digest[:])
	if saved.Checksum != wantChecksum {
		return nil, fmt.Errorf("state checksum mismatch")
	}
	return &saved.State, nil
}
