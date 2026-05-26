package upgrade

import (
	"errors"
	"strings"
	"testing"
)

func TestProbe_Valid(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"commit":"abc1234567","vcs_time":"2026-05-26T16:00:00Z","dirty":false,"go_version":"go1.26.0"}`, 0)
	got, err := Probe(bin, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got.Commit != "abc1234567" || got.GoVersion != "go1.26.0" {
		t.Fatalf("unexpected BuildInfo: %+v", got)
	}
}

func TestProbe_RejectsNonJSON(t *testing.T) {
	bin := fakeOutpostBinary(t, "not json at all", 0)
	if _, err := Probe(bin, ""); err == nil {
		t.Fatal("expected error for non-JSON output")
	}
}

func TestProbe_RejectsMissingGoVersion(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"commit":"abc1234"}`, 0)
	if _, err := Probe(bin, ""); err == nil {
		t.Fatal("expected error when go_version is empty")
	}
}

func TestProbe_RejectsNonZeroExit(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"go_version":"go1.26.0"}`, 1)
	if _, err := Probe(bin, ""); err == nil {
		t.Fatal("expected error when probe exits non-zero")
	}
}

func TestProbe_RejectsCommitMismatch(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"commit":"abc1234567","go_version":"go1.26.0"}`, 0)
	_, err := Probe(bin, "deadbee")
	if err == nil {
		t.Fatal("expected commit-mismatch error")
	}
	if !errors.Is(err, ErrShortCommit) {
		t.Fatalf("expected ErrShortCommit, got %v", err)
	}
	if !strings.Contains(err.Error(), "deadbee") || !strings.Contains(err.Error(), "abc1234") {
		t.Fatalf("error did not name both sides: %v", err)
	}
}

func TestProbe_AcceptsCommitMatch(t *testing.T) {
	bin := fakeOutpostBinary(t, `{"commit":"abc1234567","go_version":"go1.26.0"}`, 0)
	if _, err := Probe(bin, "abc1234"); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
}
