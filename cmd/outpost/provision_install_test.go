package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// installFile must create a fresh destination with the right bytes and mode.
func TestInstallFile_CreatesFresh(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "sub", "prima-server")
	if err := installFile(dst, []byte("verified bytes"), 0o755); err != nil {
		t.Fatalf("installFile: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "verified bytes" {
		t.Fatalf("dst holds %q, %v", data, err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("installed binary is not executable: %v", fi.Mode())
		}
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "sub", ".prima-install-*")); len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

// installFile must replace a live destination via rename-then-replace — the
// Windows file-lock path — leaving the new bytes, no backup, no temp files.
func TestInstallFile_ReplacesLockedDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "prima-server")
	if err := os.WriteFile(dst, []byte("stuck old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installFile(dst, []byte("new verified bytes"), 0o755); err != nil {
		t.Fatalf("installFile over a live destination: %v", err)
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "new verified bytes" {
		t.Fatalf("dst holds %q, %v", data, err)
	}
	if _, err := os.Stat(dst + ".stale"); !os.IsNotExist(err) {
		t.Fatalf("aside backup left behind: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "prima-server" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("dir holds %v, want only the replaced binary", names)
	}
}

// The role install must overwrite stale on-disk binaries with the verified
// bytes (rename-then-replace, not write-through).
func TestInstallVerifiedPrima_OverwritesStaleRoles(t *testing.T) {
	dir := t.TempDir()
	server := filepath.Join(dir, "verified-prima-server")
	worker := filepath.Join(dir, "verified-prima")
	if err := os.WriteFile(server, []byte("verified server bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker, []byte("verified worker bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	bins := shard.ServeBins{
		ServerBin: filepath.Join(dir, "roles", "llama-server"),
		WorkerBin: filepath.Join(dir, "roles", "llama-cli"),
	}
	// Stale binaries from a previous (possibly stuck) provision.
	for dst := range map[string]string{bins.ServerBin: "stuck server", bins.WorkerBin: "stuck worker"} {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("stuck"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := installVerifiedPrima(server, worker, bins); err != nil {
		t.Fatalf("install over stale roles: %v", err)
	}
	for dst, want := range map[string]string{
		bins.ServerBin: "verified server bytes",
		bins.WorkerBin: "verified worker bytes",
	} {
		data, err := os.ReadFile(dst)
		if err != nil || string(data) != want {
			t.Fatalf("%s holds %q, want %q (%v)", dst, data, want, err)
		}
	}
}

// provisionShard must attempt the tracked-PID reap BEFORE the binaries gate:
// even when provisioning refuses (unsigned cache), a stale tracked PID for
// the role bins is signaled and forgotten — the wedge never survives.
func TestProvisionShard_ReapsTrackedBeforeGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a unix true(1) helper; the reap-before-gate order is covered by the portable install tests on Windows")
	}
	isolatePrimaVerifier(t)
	dir := t.TempDir()
	bins := shard.ServeBins{
		ServerBin: filepath.Join(dir, "llama-server"),
		WorkerBin: filepath.Join(dir, "llama-cli"),
	}
	// A deterministically-dead PID: start a process, reap it, then track its
	// number. The kill fails (ESRCH) but the entry must still be forgotten.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	dead := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	shard.TrackPrimaProc(dead, bins.WorkerBin)

	_, err := provisionShard(context.Background(), bins, "", "llama3.3:70b", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "binaries:") {
		t.Fatalf("provisioning must still fail at the binaries gate, got %v", err)
	}
	if pids := shard.TrackedPrimaPIDs(bins.WorkerBin); len(pids) != 0 {
		t.Fatalf("tracked PID survived a refused provision: %v", pids)
	}
}
