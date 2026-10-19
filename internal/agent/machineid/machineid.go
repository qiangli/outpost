// Package machineid provides a stable, privacy-preserving identifier for the
// host machine.
//
// The machine ID is the lowercase hex SHA-256 digest of the underlying OS
// machine identifier. The raw identifier is never transmitted or logged.
// On systems where no OS identifier is available, a random UUID is generated
// once, persisted in the agent's state directory, and its digest is used.
package machineid

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// fallbackFileName is the file in the agent state directory where
// a generated fallback UUID is persisted if no OS identifier is available.
const fallbackFileName = "machine_id"

var osSourceHook func() (string, error)

// SetOSSourceForTest overrides the OS identifier source for tests and returns
// a cleanup function to restore the previous source.
func SetOSSourceForTest(fn func() (string, error)) func() {
	prev := osSourceHook
	osSourceHook = fn
	return func() { osSourceHook = prev }
}

// Hash returns the lowercase hex-encoded SHA-256 digest of the raw identifier.
// Leading and trailing whitespace is trimmed before hashing.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(raw)))
	return hex.EncodeToString(sum[:])
}

// Get returns the stable, privacy-preserving machine ID for this host.
// If an OS machine identifier is present, its lowercase hex SHA-256 digest
// is returned. If no OS identifier is available, a random UUID is generated
// once, persisted in the state directory, and its digest is returned.
func Get() (string, error) {
	stateDir, err := conf.DefaultConfigDir()
	if err != nil {
		return "", fmt.Errorf("state dir: %w", err)
	}
	source := osMachineID
	if osSourceHook != nil {
		source = osSourceHook
	}
	return GetWith(source, stateDir)
}

// RawOSIdentifier returns the raw machine identifier from the OS (before hashing),
// or empty string if not available. Never send or log this value.
func RawOSIdentifier() string {
	source := osMachineID
	if osSourceHook != nil {
		source = osSourceHook
	}
	raw, err := source()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(raw)
}

// GetWith resolves the machine ID using the provided OS identifier source
// and state directory.
func GetWith(osSource func() (string, error), stateDir string) (string, error) {
	if osSource != nil {
		if raw, err := osSource(); err == nil {
			raw = strings.TrimSpace(raw)
			if raw != "" {
				return Hash(raw), nil
			}
		}
	}

	if stateDir == "" {
		return "", errors.New("machineid: state directory is empty")
	}

	rawUUID, err := loadOrGenerateFallbackUUID(stateDir)
	if err != nil {
		return "", err
	}
	return Hash(rawUUID), nil
}

// loadOrGenerateFallbackUUID reads the existing fallback UUID from
// filepath.Join(stateDir, fallbackFileName), or generates and persists a new random UUID.
func loadOrGenerateFallbackUUID(stateDir string) (string, error) {
	fallbackPath := filepath.Join(stateDir, fallbackFileName)
	if data, err := os.ReadFile(fallbackPath); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "" {
			return s, nil
		}
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state dir: %w", err)
	}

	newUUID, err := newRandomUUID()
	if err != nil {
		return "", fmt.Errorf("generate random uuid: %w", err)
	}

	if err := os.WriteFile(fallbackPath, []byte(newUUID+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist fallback uuid: %w", err)
	}

	return newUUID, nil
}

// newRandomUUID generates an RFC 4122 version 4 UUID.
func newRandomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10xx (RFC 4122)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
