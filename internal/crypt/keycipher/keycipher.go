package keycipher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/scrypt"

	"hyperliquid-builder-code-bot/internal/secret"
)

const (
	scryptN  = 32768
	scryptR  = 8
	scryptP  = 1
	keyLen   = 32
	saltLen  = 16
	nonceLen = 12
)

// Encrypt encrypts a private key with AES-256-GCM using a scrypt-derived key.
func Encrypt(privateKey secret.SecretString, password secret.SecretString) (secret.SecretString, error) {
	plaintext := []byte(strings.TrimSpace(privateKey.Reveal()))
	defer zeroBytes(plaintext)
	if len(plaintext) == 0 {
		return secret.SecretString{}, fmt.Errorf("private key is required")
	}
	if password.Reveal() == "" {
		return secret.SecretString{}, fmt.Errorf("password is required")
	}

	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return secret.SecretString{}, fmt.Errorf("generate salt: %w", err)
	}
	key, err := deriveKey(password.Reveal(), salt)
	if err != nil {
		return secret.SecretString{}, err
	}
	defer zeroBytes(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("create gcm: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return secret.SecretString{}, fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	encoded := make([]byte, 0, len(salt)+len(nonce)+len(ciphertext))
	encoded = append(encoded, salt...)
	encoded = append(encoded, nonce...)
	encoded = append(encoded, ciphertext...)
	return secret.NewString(base64.RawStdEncoding.EncodeToString(encoded)), nil
}

// Decrypt decrypts a raw Base64 salt, nonce, and ciphertext payload.
func Decrypt(encrypted secret.SecretString, password secret.SecretString) (secret.SecretString, error) {
	token := strings.TrimSpace(encrypted.Reveal())
	if token == "" {
		return secret.SecretString{}, fmt.Errorf("encrypted private key is required")
	}
	if password.Reveal() == "" {
		return secret.SecretString{}, fmt.Errorf("password is required")
	}
	decoded, err := base64.RawStdEncoding.DecodeString(token)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("decode encrypted private key: %w", err)
	}
	if len(decoded) <= saltLen+nonceLen {
		return secret.SecretString{}, fmt.Errorf("encrypted private key is too short")
	}

	salt := decoded[:saltLen]
	nonce := decoded[saltLen : saltLen+nonceLen]
	ciphertext := decoded[saltLen+nonceLen:]
	key, err := deriveKey(password.Reveal(), salt)
	if err != nil {
		return secret.SecretString{}, err
	}
	defer zeroBytes(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("create gcm: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return secret.SecretString{}, fmt.Errorf("decrypt private key: authentication failed")
	}
	defer zeroBytes(plaintext)
	return secret.NewString(string(plaintext)), nil
}

func deriveKey(password string, salt []byte) ([]byte, error) {
	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive encryption key: %w", err)
	}
	return key, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
