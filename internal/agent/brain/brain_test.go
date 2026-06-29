package brain

import (
	"context"
	"errors"
	"testing"
)

func TestDecide_BootstrapStandsWithoutRefiner(t *testing.T) {
	got, fromLLM := Decide(context.Background(), "boot", "p", nil,
		func(string) (string, bool) { return "", false })
	if got != "boot" || fromLLM {
		t.Fatalf("got %q fromLLM=%v, want boot/false", got, fromLLM)
	}
}

func TestDecide_RefinerImproves(t *testing.T) {
	refine := func(context.Context, string) (string, error) { return "better", nil }
	got, fromLLM := Decide(context.Background(), "boot", "p", refine,
		func(s string) (string, bool) { return s, true })
	if got != "better" || !fromLLM {
		t.Fatalf("got %q fromLLM=%v, want better/true", got, fromLLM)
	}
}

func TestDecide_BootstrapStandsOnRefinerError(t *testing.T) {
	refine := func(context.Context, string) (string, error) { return "", errors.New("llm down") }
	got, fromLLM := Decide(context.Background(), "boot", "p", refine,
		func(s string) (string, bool) { return s, true })
	if got != "boot" || fromLLM {
		t.Fatalf("got %q, want boot (bootstrap stands when the LLM errs)", got)
	}
}

func TestDecide_BootstrapStandsOnUnparseable(t *testing.T) {
	refine := func(context.Context, string) (string, error) { return "garbage", nil }
	got, _ := Decide(context.Background(), "boot", "p", refine,
		func(string) (string, bool) { return "", false })
	if got != "boot" {
		t.Fatalf("got %q, want boot (bootstrap stands on an unparseable reply)", got)
	}
}
