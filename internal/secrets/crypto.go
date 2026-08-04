package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

const requiredKeyLength = 32 // AES-256

func Encrypt(key, plaintext []byte) ([]byte, error) {
	if len(key) != requiredKeyLength {
		return nil, fmt.Errorf("key must be %d bytes, got %d", requiredKeyLength, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}
	ciphertext := gcm.Seal(nil, nil, plaintext, nil)
	return ciphertext, nil
}

func Decrypt(key, ciphertext []byte) ([]byte, error) {
	if len(key) != requiredKeyLength {
		return nil, fmt.Errorf("key must be %d bytes, got %d", requiredKeyLength, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}
	minLen := gcm.NonceSize() + gcm.Overhead()
	if len(ciphertext) < minLen {
		return nil, fmt.Errorf("ciphertext too short: got %d bytes, need at least %d", len(ciphertext), minLen)
	}
	plaintext, err := gcm.Open(nil, nil, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}
	return plaintext, nil
}
