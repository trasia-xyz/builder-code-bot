package signing

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
)

type ActionHashInput struct {
	Action       any
	Nonce        uint64
	VaultAddress *string
	ExpiresAfter *uint64
}

func ActionHash(input ActionHashInput) ([32]byte, error) {
	var zero [32]byte
	if input.Action == nil {
		return zero, fmt.Errorf("action is nil")
	}
	actionBytes, err := packMsgpack(input.Action)
	if err != nil {
		return zero, fmt.Errorf("pack action: %w", err)
	}
	var buf bytes.Buffer
	buf.Write(actionBytes)
	writeUint64(&buf, input.Nonce)
	if input.VaultAddress == nil {
		buf.WriteByte(0x00)
	} else {
		address, err := parseAddressBytes(*input.VaultAddress)
		if err != nil {
			return zero, fmt.Errorf("vault address: %w", err)
		}
		buf.WriteByte(0x01)
		buf.Write(address[:])
	}
	if input.ExpiresAfter != nil {
		buf.WriteByte(0x00)
		writeUint64(&buf, *input.ExpiresAfter)
	}
	return keccak256(buf.Bytes()), nil
}

func ActionHashHex(input ActionHashInput) (string, error) {
	hash, err := ActionHash(input)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(hash[:]), nil
}

func parseAddressBytes(value string) ([20]byte, error) {
	var out [20]byte
	hexValue := strings.TrimPrefix(value, "0x")
	if len(hexValue) != 40 {
		return out, fmt.Errorf("expected 20-byte hex address")
	}
	decoded, err := hex.DecodeString(hexValue)
	if err != nil {
		return out, err
	}
	copy(out[:], decoded)
	return out, nil
}
