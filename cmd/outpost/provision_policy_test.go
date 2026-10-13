package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/shard"
)

func TestLegacyPrimaCacheCannotBypassSignedBuildGate(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"llama-server", "llama-cli"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old upstream binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("nng-3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bins := shard.ServeBins{ServerBin: filepath.Join(dir, "llama-server"), WorkerBin: filepath.Join(dir, "llama-cli")}
	if err := ensureBinaries(context.Background(), bins); err == nil || !strings.Contains(err.Error(), "bashy-built, signed") {
		t.Fatalf("legacy cache must be refused until a signed release is admitted, got %v", err)
	}
}
