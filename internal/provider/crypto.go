package provider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/scrypt"
)

// scrypt parameters per the package's own recommendation for interactive
// (not batch/server-to-server) use as of writing.
const (
	scryptN    = 1 << 15
	scryptR    = 8
	scryptP    = 1
	saltSize   = 16
	aesKeySize = 32
)

func deriveKey(passphrase string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, aesKeySize)
}

// encryptBytes encrypts plaintext under a key derived from passphrase and a
// freshly generated random salt, returning the salt, the AES-GCM nonce, and
// the ciphertext — none of which are secret on their own; the passphrase is
// the only thing that must stay secret.
func encryptBytes(plaintext []byte, passphrase string) (salt, nonce, ciphertext []byte, err error) {
	salt = make([]byte, saltSize)
	if _, err = rand.Read(salt); err != nil {
		return nil, nil, nil, err
	}

	key, err := deriveKey(passphrase, salt)
	if err != nil {
		return nil, nil, nil, err
	}

	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, nil, err
	}

	nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, nil, err
	}

	ciphertext = gcm.Seal(nil, nonce, plaintext, nil)
	return salt, nonce, ciphertext, nil
}

func decryptBytes(salt, nonce, ciphertext []byte, passphrase string) ([]byte, error) {
	key, err := deriveKey(passphrase, salt)
	if err != nil {
		return nil, err
	}

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed (wrong master key?): %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
