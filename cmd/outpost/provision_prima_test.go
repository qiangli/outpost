package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/shard"
)

// The story contract (Sprint 436, story 625828548647): first-party source
// tag, generated release, and per-platform raw-executable asset names. These
// constants must track the admitted release, not drift from it.
func TestPrimaContractConstants(t *testing.T) {
	if primaSourceTag != "bashy-prima-0.1.0" {
		t.Fatalf("primaSourceTag = %q, want bashy-prima-0.1.0", primaSourceTag)
	}
	if !strings.HasPrefix(primaSourceCommit, "f4ea9845") || len(primaSourceCommit) != 40 {
		t.Fatalf("primaSourceCommit = %q, want the full f4ea9845 peel", primaSourceCommit)
	}
	if primaReleaseTag != "bashy-bashy-prima-0.1.0-1" {
		t.Fatalf("primaReleaseTag = %q, want bashy-bashy-prima-0.1.0-1", primaReleaseTag)
	}
	for _, plat := range []struct{ goos, goarch string }{
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
	} {
		want := "prima-" + plat.goos + "-" + plat.goarch
		if got := primaAssetName(plat.goos, plat.goarch); got != want {
			t.Fatalf("primaAssetName(%q, %q) = %q, want %q", plat.goos, plat.goarch, got, want)
		}
	}
}

// Until the signed tool declaration exists (workflow identity, release repo,
// all four reviewed SHA pins, real bundles), the declaration itself refuses.
func TestPrimaSignedToolRefusesUntilAdmitted(t *testing.T) {
	if _, err := primaSignedTool(); err == nil || !strings.Contains(err.Error(), "bashy-built, signed") {
		t.Fatalf("unadmitted tool declaration must refuse, got %v", err)
	}
}

// ensureBinaries must refuse a legacy on-disk prima (even with a matching
// VERSION marker) and must never fall back to a $PATH executable: no legacy
// cache, PATH, upstream release, or ad-hoc codesign consults the gate.
func TestEnsureBinariesRefusesLegacyCacheAndPATH(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"llama-server", "llama-cli"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old upstream binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("nng-3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A decoy on PATH that a fallback would happily execute.
	binDir := t.TempDir()
	for _, name := range []string{"prima", "llama-server", "llama-cli"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\necho decoy\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "llama-server"), WorkerBin: filepath.Join(dir, "llama-cli")}
	if err := ensureBinaries(context.Background(), bins); err == nil || !strings.Contains(err.Error(), "bashy-built, signed") {
		t.Fatalf("legacy cache and PATH must be refused until a signed release is admitted, got %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("refusal must not touch the bins dir: %d entries before, %d after", len(before), len(after))
	}
}

// The verified binary wires both required shard roles: server (leader rank 0)
// and worker. Each role path is required and receives the verified bytes.
func TestInstallVerifiedPrimaWiresBothRoles(t *testing.T) {
	dir := t.TempDir()
	verified := filepath.Join(dir, "verified-prima")
	if err := os.WriteFile(verified, []byte("verified bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	bins := shard.ServeBins{
		ServerBin: filepath.Join(dir, "roles", "llama-server"),
		WorkerBin: filepath.Join(dir, "roles", "llama-cli"),
	}
	if err := installVerifiedPrima(verified, bins); err != nil {
		t.Fatalf("install of verified binary: %v", err)
	}
	for _, dst := range []string{bins.ServerBin, bins.WorkerBin} {
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read %s: %v", dst, err)
		}
		if string(data) != "verified bytes" {
			t.Fatalf("%s holds %q, want the verified bytes", dst, data)
		}
		fi, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v", dst, fi.Mode())
		}
	}
}

func TestInstallVerifiedPrimaRequiresBothRoles(t *testing.T) {
	dir := t.TempDir()
	verified := filepath.Join(dir, "verified-prima")
	if err := os.WriteFile(verified, []byte("verified bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bins := range []shard.ServeBins{
		{WorkerBin: filepath.Join(dir, "w")},
		{ServerBin: filepath.Join(dir, "s")},
		{},
	} {
		if err := installVerifiedPrima(verified, bins); err == nil {
			t.Fatalf("install with bins %+v must refuse", bins)
		}
	}
	if err := installVerifiedPrima(filepath.Join(dir, "missing"), shard.ServeBins{
		ServerBin: filepath.Join(dir, "s"), WorkerBin: filepath.Join(dir, "w"),
	}); err == nil {
		t.Fatal("install of a missing verified binary must refuse")
	}
}

// The shard gate runs before any model work: provisioning fails closed on the
// binaries with no Ollama endpoint and no model available.
func TestProvisionShardGatePrecedesModel(t *testing.T) {
	dir := t.TempDir()
	bins := shard.ServeBins{
		ServerBin: filepath.Join(dir, "llama-server"),
		WorkerBin: filepath.Join(dir, "llama-cli"),
	}
	_, err := provisionShard(context.Background(), bins, "", "llama3.3:70b", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "binaries:") {
		t.Fatalf("provisioning must fail at the binaries gate, got %v", err)
	}
}
