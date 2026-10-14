package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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

// isolatePrimaVerifier prevents these tests from consulting the host's cache or
// downloading a real release. A supported platform has real pins, but this
// unsigned cache entry must still be rejected by the production verifier.
func isolatePrimaVerifier(t *testing.T) *primaRejectTransport {
	t.Helper()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	t.Setenv("BASHY_OFFLINE", "0")
	transport := &primaRejectTransport{}
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: transport}
	t.Cleanup(func() { http.DefaultClient = old })
	if _, supported := binmgr.PrimaTool().Assets[binmgr.Platform()]; supported {
		path, err := binmgr.InstallPath(binmgr.PrimaTool())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("unsigned cached prima"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return transport
}

// Exercise the real shared verifier: its refusal must survive the Outpost
// wrapper and happen before network or role installation, even with real pins.
func TestEnsureBinariesUsesPrimaVerifier(t *testing.T) {
	transport := isolatePrimaVerifier(t)
	ctx := context.Background()
	verified, want := binmgr.EnsurePrima(ctx)
	if want == nil || verified != "" {
		t.Fatalf("unsigned Prima must refuse: path=%q err=%v", verified, want)
	}
	if _, supported := binmgr.PrimaTool().Assets[binmgr.Platform()]; supported {
		if !errors.Is(want, binmgr.ErrSigstore) {
			t.Fatalf("expected signature refusal, got %v", want)
		}
	} else if !strings.Contains(want.Error(), "no asset for") {
		t.Fatalf("expected unsupported platform refusal, got %v", want)
	}
	dir := t.TempDir()
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "server"), WorkerBin: filepath.Join(dir, "worker")}
	got := ensureBinaries(ctx, bins)
	if got == nil || errors.Unwrap(got) == nil || errors.Unwrap(got).Error() != want.Error() {
		t.Fatalf("expected wrapped EnsurePrima error %v, got %v", want, got)
	}
	if transport.calls != 0 {
		t.Fatalf("made %d network requests for an unsigned cache entry", transport.calls)
	}
	for _, path := range []string{bins.ServerBin, bins.WorkerBin} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refusal touched %s: %v", path, err)
		}
	}
}

func TestEnsureBinariesDownloadFailureDoesNotInstall(t *testing.T) {
	transport := isolatePrimaVerifier(t)
	if _, supported := binmgr.PrimaTool().Assets[binmgr.Platform()]; !supported {
		t.Skip("Prima has no release for this platform")
	}
	cached, err := binmgr.InstallPath(binmgr.PrimaTool())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cached); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "server"), WorkerBin: filepath.Join(dir, "worker")}
	if err := ensureBinaries(context.Background(), bins); err == nil || !strings.Contains(err.Error(), "unexpected network access") {
		t.Fatalf("expected download refusal, got %v", err)
	}
	if transport.calls != 1 {
		t.Fatalf("expected one intercepted download, got %d", transport.calls)
	}
	for _, path := range []string{cached, bins.ServerBin, bins.WorkerBin} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed download installed %s: %v", path, err)
		}
	}
}

// ensureBinaries must refuse a legacy on-disk prima (even with a matching
// VERSION marker) and must never fall back to a $PATH executable: no legacy
// cache, PATH, upstream release, or ad-hoc codesign consults the gate.
func TestEnsureBinariesRefusesLegacyCacheAndPATH(t *testing.T) {
	isolatePrimaVerifier(t)
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
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "llama-server"), WorkerBin: filepath.Join(dir, "llama-cli")}
	if err := ensureBinaries(context.Background(), bins); err == nil || !strings.Contains(err.Error(), "bashy-built, signed") {
		t.Fatalf("legacy cache and PATH must not bypass signature refusal, got %v", err)
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

// Each verified executable wires its own shard role: the server (leader rank
// 0) and the worker run distinct verified bytes from the signed release.
// Each role path is required and receives its own bytes.
func TestInstallVerifiedPrimaWiresBothRoles(t *testing.T) {
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
	if err := installVerifiedPrima(server, worker, bins); err != nil {
		t.Fatalf("install of verified binaries: %v", err)
	}
	for dst, want := range map[string]string{
		bins.ServerBin: "verified server bytes",
		bins.WorkerBin: "verified worker bytes",
	} {
		data, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read %s: %v", dst, err)
		}
		if string(data) != want {
			t.Fatalf("%s holds %q, want %q", dst, data, want)
		}
		if runtime.GOOS == "windows" {
			continue // Windows has no Unix execute bit; any file runs
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
	server := filepath.Join(dir, "verified-prima-server")
	worker := filepath.Join(dir, "verified-prima")
	if err := os.WriteFile(server, []byte("verified server bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worker, []byte("verified worker bytes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		server string
		worker string
		bins   shard.ServeBins
	}{
		{"no server binary", "", worker, shard.ServeBins{ServerBin: filepath.Join(dir, "s"), WorkerBin: filepath.Join(dir, "w")}},
		{"no worker binary", server, "", shard.ServeBins{ServerBin: filepath.Join(dir, "s"), WorkerBin: filepath.Join(dir, "w")}},
		{"no server role", server, worker, shard.ServeBins{WorkerBin: filepath.Join(dir, "w")}},
		{"no worker role", server, worker, shard.ServeBins{ServerBin: filepath.Join(dir, "s")}},
		{"no roles", server, worker, shard.ServeBins{}},
		{"missing server binary", filepath.Join(dir, "missing"), worker, shard.ServeBins{ServerBin: filepath.Join(dir, "s"), WorkerBin: filepath.Join(dir, "w")}},
		{"missing worker binary", server, filepath.Join(dir, "missing"), shard.ServeBins{ServerBin: filepath.Join(dir, "s"), WorkerBin: filepath.Join(dir, "w")}},
	} {
		if err := installVerifiedPrima(tc.server, tc.worker, tc.bins); err == nil {
			t.Fatalf("install %s must refuse", tc.name)
		}
	}
}

// The Windows OpenMP runtime lands beside the executables: the .exe files
// load it from their own directory, so each role directory gets a copy (one
// copy when both roles share a directory).
func TestInstallVerifiedLibOMPSitsNextToExe(t *testing.T) {
	dir := t.TempDir()
	dll := filepath.Join(dir, "verified-"+binmgr.PrimaLibOMPFile)
	if err := os.WriteFile(dll, []byte("verified dll bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	split := shard.ServeBins{
		ServerBin: filepath.Join(dir, "leader", "prima-server-windows-amd64.exe"),
		WorkerBin: filepath.Join(dir, "worker", "prima-windows-amd64.exe"),
	}
	if err := installVerifiedLibOMP(dll, split); err != nil {
		t.Fatalf("install of verified dll: %v", err)
	}
	for _, exe := range []string{split.ServerBin, split.WorkerBin} {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(exe), binmgr.PrimaLibOMPFile))
		if err != nil || string(got) != "verified dll bytes" {
			t.Fatalf("dll beside %s holds %q, %v", exe, got, err)
		}
	}
	shared := shard.ServeBins{
		ServerBin: filepath.Join(dir, "shared", "prima-server-windows-amd64.exe"),
		WorkerBin: filepath.Join(dir, "shared", "prima-windows-amd64.exe"),
	}
	if err := installVerifiedLibOMP(dll, shared); err != nil {
		t.Fatalf("install of verified dll into a shared dir: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != binmgr.PrimaLibOMPFile {
		t.Fatalf("shared dir holds %v, want one %s", entries, binmgr.PrimaLibOMPFile)
	}
	if err := installVerifiedLibOMP("", shared); err == nil {
		t.Fatal("install of an empty dll path must refuse")
	}
	if err := installVerifiedLibOMP(filepath.Join(dir, "missing.dll"), shared); err == nil {
		t.Fatal("install of a missing dll must refuse")
	}
}

// The shard gate runs before any model work: provisioning fails closed on the
// binaries with no Ollama endpoint and no model available.
func TestProvisionShardGatePrecedesModel(t *testing.T) {
	transport := isolatePrimaVerifier(t)
	dir := t.TempDir()
	bins := shard.ServeBins{
		ServerBin: filepath.Join(dir, "llama-server"),
		WorkerBin: filepath.Join(dir, "llama-cli"),
	}
	_, err := provisionShard(context.Background(), bins, "", "llama3.3:70b", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "binaries:") || !strings.Contains(err.Error(), "prima engine unavailable") {
		t.Fatalf("provisioning must fail at the binaries gate, got %v", err)
	}
	if transport.calls != 0 {
		t.Fatalf("provisioning made %d network requests after cache refusal", transport.calls)
	}
}
