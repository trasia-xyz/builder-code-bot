package signing

import "golang.org/x/crypto/sha3"

func keccak256(chunks ...[]byte) [32]byte {
	hash := sha3.NewLegacyKeccak256()
	for _, chunk := range chunks {
		_, _ = hash.Write(chunk)
	}

	var out [32]byte
	hash.Sum(out[:0])
	return out
}
