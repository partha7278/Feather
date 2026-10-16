// Package secrets encrypts the credentials feather keeps in its database.
//
// Cloud keys and Mongo connection strings are the most dangerous thing feather
// stores: a leaked database file would otherwise hand over every bucket and
// every cluster. They are sealed with AES-256-GCM under a master key that lives
// outside the database, so the two must be stolen together to be useful.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the AES-256 key length in bytes.
const KeySize = 32

// Box seals and opens secrets under one master key.
type Box struct {
	aead cipher.AEAD
	// Source records where the key came from, for startup logging.
	Source string
}

// ErrKeyMismatch means a stored value could not be decrypted with the current
// master key — almost always a lost or swapped key file rather than corruption.
var ErrKeyMismatch = errors.New("stored secret could not be decrypted with the current master key")

// New builds a Box from a raw 32-byte key.
func New(key []byte, source string) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, Source: source}, nil
}

// Resolve loads the master key, in order of preference:
//
//  1. the FEATHER_MASTER_KEY environment variable (64 hex characters), which is
//     how a container or a systemd unit should supply it;
//  2. the key file at path;
//  3. a freshly generated key, written to path with 0600.
//
// Generating on first start is deliberate. The alternative — refusing to boot
// until an operator produces a key — means a fresh install cannot reach its own
// setup screen.
func Resolve(envValue, path string) (*Box, error) {
	if raw := strings.TrimSpace(envValue); raw != "" {
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("FEATHER_MASTER_KEY: %w", err)
		}
		return New(key, "FEATHER_MASTER_KEY")
	}

	if path == "" {
		return nil, errors.New("no master key path configured")
	}

	switch data, err := os.ReadFile(path); {
	case err == nil:
		return fromKeyFile(path, data)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("reading master key %s: %w", path, err)
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generating master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating key directory: %w", err)
	}
	// O_EXCL: two processes starting at once must not both think they wrote the
	// key, or one of them would seal secrets the other cannot open.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the race; read what it wrote. Once only: a
			// path that exists for O_EXCL but not for reading is a dangling
			// symlink, and retrying would recurse forever rather than write
			// through it.
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				if os.IsNotExist(readErr) {
					return nil, fmt.Errorf("master key %s exists but points at nothing (a dangling symlink?) — refusing to write a new key through it", path)
				}
				return nil, fmt.Errorf("reading master key %s: %w", path, readErr)
			}
			return fromKeyFile(path, data)
		}
		return nil, fmt.Errorf("writing master key %s: %w", path, err)
	}
	if _, err := file.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		file.Close()
		return nil, fmt.Errorf("writing master key %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return New(key, path+" (newly generated)")
}

// fromKeyFile builds a Box from the contents of the key file at path.
func fromKeyFile(path string, data []byte) (*Box, error) {
	key, err := decodeKey(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := checkPermissions(path); err != nil {
		return nil, err
	}
	return New(key, path)
}

// Seal encrypts a secret. An empty input seals to nothing, so "no session
// token" stays distinguishable from "a token that happens to be empty".
func (b *Box) Seal(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a sealed secret.
func (b *Box) Open(sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if len(sealed) < b.aead.NonceSize() {
		return "", ErrKeyMismatch
	}
	nonce, body := sealed[:b.aead.NonceSize()], sealed[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", ErrKeyMismatch
	}
	return string(plain), nil
}

// GenerateKeyHex produces a key suitable for FEATHER_MASTER_KEY.
func GenerateKeyHex() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return hex.EncodeToString(key), nil
}

func decodeKey(raw string) ([]byte, error) {
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("must be %d hex characters (feather keygen prints one)", KeySize*2)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("must decode to %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// checkPermissions refuses a world-readable key file. A key everyone on the box
// can read is not protecting the database sitting next to it.
func checkPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("master key %s is mode %04o — it must not be readable by group or other (chmod 600 %s)", path, mode, path)
	}
	return nil
}
