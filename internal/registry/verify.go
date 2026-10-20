package registry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// verifySignature checks that sig is a valid detached signature (binary or
// ASCII-armored) of signed, made by one of the given armored public keys.
func verifySignature(signed, sig []byte, armoredKeys []string) error {
	if len(armoredKeys) == 0 {
		return fmt.Errorf("signature check failed: registry advertises no signing keys")
	}
	var keyring openpgp.EntityList
	for _, k := range armoredKeys {
		es, err := openpgp.ReadArmoredKeyRing(strings.NewReader(k))
		if err != nil {
			return fmt.Errorf("signature check failed: reading signing key: %w", err)
		}
		keyring = append(keyring, es...)
	}
	var err error
	if bytes.HasPrefix(bytes.TrimSpace(sig), []byte("-----BEGIN PGP SIGNATURE")) {
		_, err = openpgp.CheckArmoredDetachedSignature(keyring, bytes.NewReader(signed), bytes.NewReader(sig), nil)
	} else {
		_, err = openpgp.CheckDetachedSignature(keyring, bytes.NewReader(signed), bytes.NewReader(sig), nil)
	}
	if err != nil {
		return fmt.Errorf("signature check failed: SHA256SUMS signature invalid: %w", err)
	}
	return nil
}

// shasumFor returns the hex digest listed for filename in a SHA256SUMS file.
func shasumFor(sums []byte, filename string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == filename {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s not listed in SHA256SUMS", filename)
}

// hashFile returns the hex SHA256 of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
