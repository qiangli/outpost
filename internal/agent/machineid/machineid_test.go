package machineid

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestMachineIDDigest verifies that the digest of a fixed identifier produces
// the expected lowercase hex SHA-256 hash, and that surrounding whitespace is trimmed.
func TestMachineIDDigest(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "fixed test vector",
			input:    "fixed-machine-id-for-testing",
			expected: "663f37fbe7b8460ad70d2084e0bcdaff32f37f97594467385f9c74eb3bc5fbfa",
		},
		{
			name:     "trims whitespace and newline",
			input:    "  fixed-machine-id-for-testing\n\t",
			expected: "663f37fbe7b8460ad70d2084e0bcdaff32f37f97594467385f9c74eb3bc5fbfa",
		},
		{
			name:     "short string",
			input:    "hello",
			expected: "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Hash(tc.input)
			if got != tc.expected {
				t.Errorf("Hash(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

// TestMachineIDFallbackPersistsUUID verifies that when no OS identifier is available,
// a random UUID is generated, persisted in the state directory, and the same value
// is returned on subsequent calls and after reloading from the state directory.
func TestMachineIDFallbackPersistsUUID(t *testing.T) {
	noOSID := func() (string, error) {
		return "", errors.New("no OS identifier on this platform")
	}

	stateDir := t.TempDir()

	// 1. First call: generates fallback UUID and persists to stateDir.
	id1, err := GetWith(noOSID, stateDir)
	if err != nil {
		t.Fatalf("first GetWith failed: %v", err)
	}

	hexRegex := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hexRegex.MatchString(id1) {
		t.Fatalf("id1 %q does not match ^[0-9a-f]{64}$", id1)
	}

	// Verify fallback file was created on disk.
	fallbackFile := filepath.Join(stateDir, fallbackFileName)
	data, err := os.ReadFile(fallbackFile)
	if err != nil {
		t.Fatalf("fallback file %q not created: %v", fallbackFile, err)
	}
	rawUUID := strings.TrimSpace(string(data))
	if rawUUID == "" {
		t.Fatalf("fallback file is empty")
	}
	uuidRegex := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	if !uuidRegex.MatchString(rawUUID) {
		t.Errorf("raw fallback UUID %q is not a valid UUID format", rawUUID)
	}
	if id1 == rawUUID {
		t.Errorf("id1 %q must not equal raw UUID %q", id1, rawUUID)
	}
	if Hash(rawUUID) != id1 {
		t.Errorf("Hash(rawUUID) = %q, want %q", Hash(rawUUID), id1)
	}

	// 2. Second call: must return the exact same value.
	id2, err := GetWith(noOSID, stateDir)
	if err != nil {
		t.Fatalf("second GetWith failed: %v", err)
	}
	if id2 != id1 {
		t.Errorf("id2 %q != id1 %q on second call", id2, id1)
	}

	// 3. Reload from state directory: verify the persisted UUID survives and yields identical digest.
	dataReloaded, err := os.ReadFile(fallbackFile)
	if err != nil {
		t.Fatalf("re-reading fallback file failed: %v", err)
	}
	if strings.TrimSpace(string(dataReloaded)) != rawUUID {
		t.Errorf("persisted UUID changed: %q vs %q", strings.TrimSpace(string(dataReloaded)), rawUUID)
	}

	id3, err := GetWith(noOSID, stateDir)
	if err != nil {
		t.Fatalf("third GetWith after reload failed: %v", err)
	}
	if id3 != id1 {
		t.Errorf("id3 %q != id1 %q after reload from state dir", id3, id1)
	}
}

// TestMachineIDOSIdentifierUsedWhenAvailable verifies that when an OS identifier is available,
// its digest is returned and no fallback file is written to the state directory.
func TestMachineIDOSIdentifierUsedWhenAvailable(t *testing.T) {
	rawID := "test-os-hardware-id-98765"
	osSource := func() (string, error) {
		return rawID, nil
	}

	stateDir := t.TempDir()
	id, err := GetWith(osSource, stateDir)
	if err != nil {
		t.Fatalf("GetWith failed: %v", err)
	}

	expected := Hash(rawID)
	if id != expected {
		t.Errorf("id = %q, want %q", id, expected)
	}

	// Verify no fallback file was created.
	fallbackFile := filepath.Join(stateDir, fallbackFileName)
	if _, err := os.Stat(fallbackFile); !os.IsNotExist(err) {
		t.Errorf("fallback file %q should not exist when OS identifier is present", fallbackFile)
	}
}

// TestMachineIDLiveHost verifies that Get() succeeds on the current platform,
// produces a 64-char lowercase hex digest, and never matches the raw identifier.
func TestMachineIDLiveHost(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	id, err := Get()
	if err != nil {
		t.Fatalf("Get() on live host failed: %v", err)
	}

	re := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !re.MatchString(id) {
		t.Errorf("id %q does not match ^[0-9a-f]{64}$", id)
	}

	raw := RawOSIdentifier()
	if raw != "" {
		if id == raw {
			t.Errorf("id %q must not equal raw OS identifier %q", id, raw)
		}
		if id != Hash(raw) {
			t.Errorf("id %q does not match Hash(raw) %q", id, Hash(raw))
		}
	}
}

// TestMachineIDHook verifies SetOSSourceForTest overrides the source used by Get().
func TestMachineIDHook(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cleanup := SetOSSourceForTest(func() (string, error) {
		return "injected-source-id", nil
	})
	defer cleanup()

	id, err := Get()
	if err != nil {
		t.Fatalf("Get() with hook failed: %v", err)
	}

	want := Hash("injected-source-id")
	if id != want {
		t.Errorf("id = %q, want %q", id, want)
	}

	if raw := RawOSIdentifier(); raw != "injected-source-id" {
		t.Errorf("RawOSIdentifier() = %q, want %q", raw, "injected-source-id")
	}
}

// TestMachineIDFallbackDoesNotTouchRealHome verifies that when the OS source returns
// an error and XDG_CONFIG_HOME is set to a temp dir, Get() writes the fallback UUID
// ONLY under that temp dir, and does not create machine_id in the real home directory.
func TestMachineIDFallbackDoesNotTouchRealHome(t *testing.T) {
	cleanup := SetOSSourceForTest(func() (string, error) {
		return "", errors.New("simulated error: no OS machine identifier")
	})
	defer cleanup()

	tempDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tempDir)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	realHomePath := filepath.Join(home, ".config", "matrix", fallbackFileName)
	_, existedBefore := os.Stat(realHomePath)

	id, err := Get()
	if err != nil {
		t.Fatalf("Get() failed: %v", err)
	}
	if id == "" {
		t.Fatal("Get() returned empty ID")
	}

	// Verify fallback file was created under tempDir.
	tempFallback := filepath.Join(tempDir, "matrix", fallbackFileName)
	data, err := os.ReadFile(tempFallback)
	if err != nil {
		t.Fatalf("expected fallback file at %s, got err: %v", tempFallback, err)
	}
	rawUUID := strings.TrimSpace(string(data))
	if rawUUID == "" {
		t.Fatalf("fallback file at %s is empty", tempFallback)
	}
	if Hash(rawUUID) != id {
		t.Errorf("Hash(%q) = %q, want %q", rawUUID, Hash(rawUUID), id)
	}

	// Assert the real-home path was not created by the test.
	_, existedAfter := os.Stat(realHomePath)
	if os.IsNotExist(existedBefore) && !os.IsNotExist(existedAfter) {
		_ = os.Remove(realHomePath)
		t.Fatalf("real-home path %s was created by the test", realHomePath)
	}
}
