package signing

import (
	"fmt"
	"math/big"
	"strings"
)

const (
	SpotSendPrimaryType     = "HyperliquidTransaction:SpotSend"
	UserSignDomainName      = "HyperliquidSignTransaction"
	UserSignDomainVersion   = "1"
	DefaultSignatureChainID = "0x66eee"

	spotSendType = "HyperliquidTransaction:SpotSend(string hyperliquidChain,string destination,string token,string amount,uint64 time)"
)

var (
	spotSendTypeHash       = keccak256([]byte(spotSendType))
	userSignDomainNameHash = keccak256([]byte(UserSignDomainName))
	userSignVersionHash    = keccak256([]byte(UserSignDomainVersion))
)

type SpotSendAction struct {
	Type             string `json:"type"`
	HyperliquidChain string `json:"hyperliquidChain"`
	SignatureChainID string `json:"signatureChainId"`
	Destination      string `json:"destination"`
	Token            string `json:"token"`
	Amount           string `json:"amount"`
	Time             uint64 `json:"time"`
}

func SignSpotSend(key PrivateKey, action SpotSendAction) (Signature, error) {
	privateKey, err := key.require()
	if err != nil {
		return Signature{}, err
	}
	digest, err := spotSendDigest(action)
	if err != nil {
		return Signature{}, err
	}
	return signDigest(privateKey, digest)
}

func RecoverSpotSendSigner(action SpotSendAction, signature Signature) (string, error) {
	digest, err := spotSendDigest(action)
	if err != nil {
		return "", err
	}
	return recoverDigestSigner(digest, signature)
}

func spotSendDigest(action SpotSendAction) ([32]byte, error) {
	if action.Type != "spotSend" {
		return [32]byte{}, fmt.Errorf("spot send action type must be %q", "spotSend")
	}
	if action.HyperliquidChain != "Mainnet" && action.HyperliquidChain != "Testnet" {
		return [32]byte{}, fmt.Errorf("hyperliquid chain must be %q or %q", "Mainnet", "Testnet")
	}
	chainID, err := parseSignatureChainID(action.SignatureChainID)
	if err != nil {
		return [32]byte{}, err
	}

	domainEncoded := make([]byte, 0, 32*5)
	domainEncoded = append(domainEncoded, eip712DomainTypeHash[:]...)
	domainEncoded = append(domainEncoded, userSignDomainNameHash[:]...)
	domainEncoded = append(domainEncoded, userSignVersionHash[:]...)
	chainIDWord := uint256Word(chainID)
	domainEncoded = append(domainEncoded, chainIDWord[:]...)
	domainEncoded = append(domainEncoded, make([]byte, 32)...)
	domainSeparator := keccak256(domainEncoded)

	chainHash := keccak256([]byte(action.HyperliquidChain))
	destinationHash := keccak256([]byte(action.Destination))
	tokenHash := keccak256([]byte(action.Token))
	amountHash := keccak256([]byte(action.Amount))
	timeWord := uint256Word(new(big.Int).SetUint64(action.Time))
	messageEncoded := make([]byte, 0, 32*6)
	messageEncoded = append(messageEncoded, spotSendTypeHash[:]...)
	messageEncoded = append(messageEncoded, chainHash[:]...)
	messageEncoded = append(messageEncoded, destinationHash[:]...)
	messageEncoded = append(messageEncoded, tokenHash[:]...)
	messageEncoded = append(messageEncoded, amountHash[:]...)
	messageEncoded = append(messageEncoded, timeWord[:]...)
	messageHash := keccak256(messageEncoded)

	return keccak256([]byte{0x19, 0x01}, domainSeparator[:], messageHash[:]), nil
}

func parseSignatureChainID(value string) (*big.Int, error) {
	if value != DefaultSignatureChainID {
		return nil, fmt.Errorf("signature chain ID must be %q", DefaultSignatureChainID)
	}
	hexValue := strings.TrimPrefix(value, "0x")
	chainID, ok := new(big.Int).SetString(hexValue, 16)
	if !ok || chainID.Sign() < 0 || chainID.BitLen() > 256 {
		return nil, fmt.Errorf("invalid signature chain ID %q", value)
	}
	return chainID, nil
}
