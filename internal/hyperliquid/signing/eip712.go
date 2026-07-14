package signing

import (
	"fmt"
	"math/big"

	"builder-code-bot/internal/hyperliquid"
)

var (
	eip712DomainTypeHash = keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	agentTypeHash        = keccak256([]byte("Agent(string source,bytes32 connectionId)"))
	exchangeNameHash     = keccak256([]byte("Exchange"))
	exchangeVersionHash  = keccak256([]byte("1"))
	mainnetSourceHash    = keccak256([]byte("a"))
	testnetSourceHash    = keccak256([]byte("b"))
	l1DomainSeparator    = buildL1DomainSeparator()
)

func networkSourceHash(network hyperliquid.Network) ([32]byte, error) {
	switch hyperliquid.NormalizeNetwork(network) {
	case hyperliquid.NetworkMainnet:
		return mainnetSourceHash, nil
	case hyperliquid.NetworkTestnet:
		return testnetSourceHash, nil
	default:
		return [32]byte{}, fmt.Errorf("unsupported network %q", network)
	}
}

func buildL1DomainSeparator() [32]byte {
	chainID := uint256Word(big.NewInt(1337))
	encoded := make([]byte, 0, 32*5)
	encoded = append(encoded, eip712DomainTypeHash[:]...)
	encoded = append(encoded, exchangeNameHash[:]...)
	encoded = append(encoded, exchangeVersionHash[:]...)
	encoded = append(encoded, chainID[:]...)
	encoded = append(encoded, make([]byte, 32)...)
	return keccak256(encoded)
}

func l1AgentDigest(connectionID [32]byte, network hyperliquid.Network) ([32]byte, error) {
	sourceHash, err := networkSourceHash(network)
	if err != nil {
		return [32]byte{}, err
	}
	encoded := make([]byte, 0, 32*3)
	encoded = append(encoded, agentTypeHash[:]...)
	encoded = append(encoded, sourceHash[:]...)
	encoded = append(encoded, connectionID[:]...)
	agentHash := keccak256(encoded)
	return keccak256([]byte{0x19, 0x01}, l1DomainSeparator[:], agentHash[:]), nil
}

func uint256Word(value *big.Int) [32]byte {
	var out [32]byte
	bytes := value.Bytes()
	copy(out[32-len(bytes):], bytes)
	return out
}
