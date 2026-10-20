package federation

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/qiangli/yoke/external/registry"
	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestExecRunnerUsesPinnedManagedKarmadactl(t *testing.T) {
	entry, ok := registry.Lookup("karmadactl")
	if !ok || entry.DefaultVersion != "v1.19.0" {
		t.Fatalf("managed karmadactl entry = %+v, present = %v", entry, ok)
	}
	if runtime.GOOS != "windows" {
		if _, pinned := binmgr.PinnedSHA256("karmadactl", entry.DefaultVersion, binmgr.Platform()); !pinned {
			t.Fatalf("karmadactl %s lacks a digest for %s", entry.DefaultVersion, binmgr.Platform())
		}
	}

	ctx := context.WithValue(context.Background(), struct{}{}, "request")
	want := errors.New("managed resolver called")
	runner := ExecRunner{Ensure: func(got context.Context) (string, error) {
		if got != ctx {
			t.Fatal("request context was not passed to managed resolver")
		}
		return "", want
	}}
	if _, err := runner.Run(ctx, "version"); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want managed resolver error", err)
	}
}
