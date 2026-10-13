package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/shard"
	"github.com/qiangli/yoke/pkg/binmgr"
)

type primaRejectTransport struct{ calls int }

func (r *primaRejectTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, errors.New("unexpected network access")
}

// Exercise the real shared verifier: its refusal must survive the Outpost
// wrapper and happen before network, cache creation, or role installation.
func TestEnsureBinariesUsesPrimaVerifier(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "cache")
	t.Setenv("BASHY_BIN_CACHE", cache)
	transport := &primaRejectTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = old })
	ctx := context.Background()
	verified, want := binmgr.EnsurePrima(ctx)
	if want == nil || verified != "" {
		t.Fatalf("unadmitted Prima must refuse: path=%q err=%v", verified, want)
	}
	if !strings.Contains(want.Error(), "no committed sha256") && !strings.Contains(want.Error(), "no asset for") {
		t.Fatalf("expected admission refusal, got %v", want)
	}
	dir := t.TempDir()
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "server"), WorkerBin: filepath.Join(dir, "worker")}
	got := ensureBinaries(ctx, bins)
	if got == nil || errors.Unwrap(got) == nil || errors.Unwrap(got).Error() != want.Error() {
		t.Fatalf("expected wrapped EnsurePrima error %v, got %v", want, got)
	}
	if transport.calls != 0 {
		t.Fatalf("made %d network requests before admission", transport.calls)
	}
	for _, path := range []string{cache, bins.ServerBin, bins.WorkerBin} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refusal touched %s: %v", path, err)
		}
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
	for _, name := range []string{"prima", "llama-server", "llama-cli", "codesign", "curl"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\necho invoked > \"$PRIMA_DECOY_MARKER\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(t.TempDir(), "invoked")
	t.Setenv("PRIMA_DECOY_MARKER", marker)
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "llama-server"), WorkerBin: filepath.Join(dir, "llama-cli")}
	if err := ensureBinaries(context.Background(), bins); err == nil || !strings.Contains(err.Error(), "bashy-built, signed") {
		t.Fatalf("legacy cache and PATH must be refused until a signed release is admitted, got %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fallback executable invoked: %v", err)
	}
	for _, name := range []string{"llama-server", "llama-cli"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != "old upstream binary" {
			t.Fatalf("refusal changed %s: %q, %v", name, data, err)
		}
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
