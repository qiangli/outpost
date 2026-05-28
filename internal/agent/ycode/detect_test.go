package ycode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withHomeOverride lets a test point HOME at a temp dir without
// stepping on the parent test environment. Returns a teardown.
func withHomeOverride(t *testing.T) (homeDir string, teardown func()) {
	t.Helper()
	dir := t.TempDir()
	prevHome := os.Getenv("HOME")
	t.Setenv("HOME", dir)
	return dir, func() {
		if prevHome != "" {
			t.Setenv("HOME", prevHome)
		}
	}
}

func writeManifest(t *testing.T, home, apiURL string) {
	t.Helper()
	manifestDir := filepath.Join(home, ".agents", "ycode")
	if err := os.MkdirAll(manifestDir, 0o700); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"endpoints": map[string]string{
			"api": apiURL,
		},
	})
	if err := os.WriteFile(filepath.Join(manifestDir, "manifest.json"), body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestDetect_RunningWhenManifestAndAPIRespond(t *testing.T) {
	home, td := withHomeOverride(t)
	defer td()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	writeManifest(t, home, srv.URL+"/")

	info := Detect()
	if info.State != StateRunning {
		t.Errorf("State = %q, want %q", info.State, StateRunning)
	}
	if !strings.HasPrefix(info.APIEndpoint, srv.URL) {
		t.Errorf("APIEndpoint = %q, want prefix %q", info.APIEndpoint, srv.URL)
	}
	// DownloadURL is always populated for upgrade visibility.
	if info.DownloadURL == "" {
		t.Error("DownloadURL should always be populated")
	}
}

func TestDetect_StaleManifestWhenAPIDown(t *testing.T) {
	home, td := withHomeOverride(t)
	defer td()
	// Point at a port nothing is listening on. Use httptest to grab
	// a port, then close immediately so the bind is real but the
	// listener is gone.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	deadURL := srv.URL
	srv.Close()
	writeManifest(t, home, deadURL+"/")

	info := Detect()
	if info.State != StateStaleManifest && info.State != StateInstalled && info.State != StateNotInstalled {
		t.Errorf("State = %q, want stale_manifest (or installed/not_installed depending on PATH)", info.State)
	}
	// APIEndpoint should still be the dead URL — the manifest was
	// readable, the operator may want to see what we tried.
	if info.APIEndpoint != deadURL+"/" {
		t.Errorf("APIEndpoint = %q, want %q", info.APIEndpoint, deadURL+"/")
	}
}

func TestDetect_NotInstalledWhenNoManifestAndNoBinary(t *testing.T) {
	home, td := withHomeOverride(t)
	defer td()
	// Also clear PATH so LookPath doesn't find a real ycode
	// installed system-wide on the dev machine.
	t.Setenv("PATH", "")
	info := Detect()
	if info.State != StateNotInstalled {
		t.Errorf("State = %q, want %q", info.State, StateNotInstalled)
	}
	if info.BinaryPath != "" {
		t.Errorf("BinaryPath = %q, want empty when not installed (home was %q)", info.BinaryPath, home)
	}
}

func TestDetect_InstalledWhenBinaryUnderHomeBin(t *testing.T) {
	home, td := withHomeOverride(t)
	defer td()
	t.Setenv("PATH", "") // ensure we hit the $HOME/bin fallback path

	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	binName := "ycode"
	if runtime.GOOS == "windows" {
		binName = "ycode.exe"
	}
	binPath := filepath.Join(binDir, binName)
	// Use a minimal shell script so probeVersion doesn't crash;
	// the test doesn't care about the version content, just that
	// locateBinary finds the file.
	body := []byte("#!/bin/sh\necho v0.0.0-test\n")
	if err := os.WriteFile(binPath, body, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	info := Detect()
	if info.State != StateInstalled {
		t.Errorf("State = %q, want %q", info.State, StateInstalled)
	}
	if info.BinaryPath != binPath {
		t.Errorf("BinaryPath = %q, want %q", info.BinaryPath, binPath)
	}
}

func TestReleaseAssetName_SupportedAndUnsupported(t *testing.T) {
	// The function reads runtime.GOOS+GOARCH directly, so we can't
	// override it without build-tagged shims. Sanity check: when
	// PlatformSupported(), the name is non-empty; when not, empty.
	supp := platformSupported()
	name := ReleaseAssetName()
	if supp && name == "" {
		t.Errorf("supported platform should yield a non-empty asset name")
	}
	if !supp && name != "" {
		t.Errorf("unsupported platform should yield empty asset name, got %q", name)
	}
}

func TestPlatformSupported_KnownMatrix(t *testing.T) {
	// Just a sanity bind: the three release-pipeline targets must
	// be true; the two excluded ones false. If ycode's CI matrix
	// changes (e.g. windows-amd64 gets unlocked), platformSupported
	// here needs the same update — this test fails loudly when they
	// drift.
	tests := map[string]bool{
		"linux/amd64":   true,
		"linux/arm64":   true,
		"darwin/arm64":  true,
		"darwin/amd64":  false,
		"windows/amd64": false,
	}
	// We can't toggle runtime.GOOS at test time, so just exercise
	// the current platform and trust the constant table.
	key := runtime.GOOS + "/" + runtime.GOARCH
	want, ok := tests[key]
	if !ok {
		t.Skipf("unknown platform %q — add to test table", key)
	}
	if got := platformSupported(); got != want {
		t.Errorf("platformSupported on %s = %v, want %v", key, got, want)
	}
}
