package hyperliquid

import (
	"fmt"
	"strings"
)

type Network string

const (
	NetworkMainnet Network = "mainnet"
	NetworkTestnet Network = "testnet"
	DefaultNetwork         = NetworkTestnet
)

func CanonicalNetwork(network Network) Network {
	return Network(strings.ToLower(strings.TrimSpace(string(network))))
}

func NormalizeNetwork(network Network) Network {
	network = CanonicalNetwork(network)
	if network == "" {
		return DefaultNetwork
	}
	return network
}

func ValidateNetwork(network Network) error {
	switch CanonicalNetwork(network) {
	case NetworkMainnet, NetworkTestnet:
		return nil
	default:
		return fmt.Errorf("hyperliquid network must be %q or %q", NetworkMainnet, NetworkTestnet)
	}
}
