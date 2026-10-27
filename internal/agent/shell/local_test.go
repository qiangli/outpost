package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/bashypath"
)

func TestRunCommandSessionEnvironmentExported(t *testing.T) {
	t.Setenv("OUTPOST_SESSION_TEST", "inherited")
	var out bytes.Buffer
	code, err := RunCommand(context.Background(), `bashy -c 'printf "%s:%s:%s" "$OUTPOST_SESSION_TEST" "$SSH_AUTH_SOCK" "${PATH:+set}"' | cat`, nil, &out, &out, map[string]string{"SSH_AUTH_SOCK": "/session/socket"})
	if err != nil || code != 0 || out.String() != "inherited:/session/socket:set" {
		t.Fatalf("session env: code=%d err=%v output=%q", code, err, out.String())
	}
}

func TestRunCommandResolvedBashyOnSessionPath(t *testing.T) {
	path, _, err := bashypath.Find(nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	installed := filepath.Join(dir, bashypath.Name())
	if err := os.WriteFile(installed, data, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BASHY_BIN", installed)
	t.Setenv("PATH", dir)
	var out bytes.Buffer
	// An explicit session PATH must still find the Bashy selected for this
	// session, including when it lives outside the usual install directories.
	code, err := RunCommand(context.Background(), `bashy -c 'printf child' | cat`, nil, &out, &out, map[string]string{"PATH": t.TempDir()})
	if err != nil || code != 0 || out.String() != "child" {
		t.Fatalf("session PATH: code=%d err=%v output=%q", code, err, out.String())
	}
}

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
	// Parse diagnostics and status now come from the Bashy child.
	code, err := RunLocalCommand(context.Background(), `echo "unclosed`, nil, &out, &out)
	if err != nil {
		t.Fatalf("child launch failed: %v", err)
	}
	if code != 2 {
		t.Errorf("expected Bashy syntax-error exit 2, got %d", code)
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
