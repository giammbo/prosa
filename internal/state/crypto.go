package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/scrypt"
)

const (
	magic     = "PRST1\n"
	saltLen   = 16
	nonceLen  = 12
	scryptN   = 32768
	scryptR   = 8
	scryptP   = 1
	keyLen    = 32
	headerLen = len(magic) + saltLen + nonceLen
)

func newGCM(passphrase string, salt []byte) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive state key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encrypt returns magic + salt + nonce + ciphertext. The intent name is bound
// as additional authenticated data, so a renamed file does not decrypt.
func encrypt(passphrase, intent string, plaintext []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, headerLen+len(plaintext)+gcm.Overhead())
	out = append(out, magic...)
	out = append(out, salt...)
	out = append(out, nonce...)
	return gcm.Seal(out, nonce, plaintext, []byte(intent)), nil
}

func decrypt(passphrase, intent string, data []byte) ([]byte, error) {
	if len(data) < headerLen || string(data[:len(magic)]) != magic {
		return nil, errors.New("not a prosa state file")
	}
	salt := data[len(magic) : len(magic)+saltLen]
	nonce := data[len(magic)+saltLen : headerLen]
	gcm, err := newGCM(passphrase, salt)
	if err != nil {
		return nil, err
	}
	pt, err := gcm.Open(nil, nonce, data[headerLen:], []byte(intent))
	if err != nil {
		return nil, fmt.Errorf("decrypt state for intent %q: wrong passphrase, renamed or corrupted file", intent)
	}
	return pt, nil
}
