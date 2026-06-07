package shell

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunLocalCommand_Echo(t *testing.T) {
	var out bytes.Buffer
	code, err := RunLocalCommand(context.Background(), `echo hello`, nil, &out, &out)
	if err != nil {
		t.Fatalf("RunLocalCommand: %v", err)
	}
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if got := strings.TrimSpace(out.String()); got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}
}

func TestRunLocalCommand_ExitStatusPropagates(t *testing.T) {
	var out bytes.Buffer
	code, err := RunLocalCommand(context.Background(), `exit 42`, nil, &out, &out)
	if err != nil {
		t.Fatalf("RunLocalCommand: %v", err)
	}
	if code != 42 {
		t.Errorf("expected exit 42, got %d", code)
	}
}

func TestRunLocalCommand_NonZeroFromFailedCmd(t *testing.T) {
	var out bytes.Buffer
	code, err := RunLocalCommand(context.Background(), `false`, nil, &out, &out)
	if err != nil {
		t.Fatalf("RunLocalCommand: %v", err)
	}
	if code != 1 {
		t.Errorf("expected exit 1 from `false`, got %d", code)
	}
}

func TestRunLocalCommand_ParseError(t *testing.T) {
	var out bytes.Buffer
	// Unmatched quote → parse error → code 127.
	code, err := RunLocalCommand(context.Background(), `echo "unclosed`, nil, &out, &out)
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if code != 127 {
		t.Errorf("expected exit 127 on parse error, got %d", code)
	}
}

func TestRunLocalCommand_BashismsHonored(t *testing.T) {
	// Spot-check that bash $((arith)), [[ test ]], and brace expansion
	// work through the runner. If this fails, the parser is in POSIX
	// rather than Bash mode.
	var out bytes.Buffer
	cmd := `if [[ $((1+2)) -eq 3 ]]; then echo ok; fi`
	code, err := RunLocalCommand(context.Background(), cmd, nil, &out, &out)
	if err != nil {
		t.Fatalf("RunLocalCommand: %v", err)
	}
	if code != 0 {
		t.Errorf("expected exit 0, got %d (output=%q)", code, out.String())
	}
	if got := strings.TrimSpace(out.String()); got != "ok" {
		t.Errorf("expected 'ok', got %q", got)
	}
}
