package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSupervisorCredentialOutputHelper(t *testing.T) {
	if os.Getenv("OUTPOST_REDACTION_HELPER") != "1" {
		return
	}
	token := "9f42e8a1b0c7d6358a4f1c962e0b3d75f8a9c6102e4d7b953a6f8c012d35b7e4"
	fmt.Fprint(os.Stdout, "ready token="+token[:20])
	fmt.Fprintln(os.Stdout, token[20:])
	fmt.Fprintln(os.Stderr, "Authorization: Bearer "+token)
	os.Exit(0)
}

func TestSupervisorRedactsChildCredentialLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	p := &Program{Name: "redaction-child", Path: os.Args[0], Args: []string{"-test.run=^TestSupervisorCredentialOutputHelper$"}, Env: []string{"OUTPOST_REDACTION_HELPER=1"}, LogPath: logPath, MinBackoff: time.Hour, MaxBackoff: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := New(p)
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := m.Snapshot()[0]
		if status.Starts > 0 && !status.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	token := "9f42e8a1b0c7d6358a4f1c962e0b3d75f8a9c6102e4d7b953a6f8c012d35b7e4"
	if strings.Contains(string(data), token) {
		t.Fatal("child credential leaked to daemon log")
	}
	if !strings.Contains(string(data), "ready") || !strings.Contains(string(data), "[redacted:") {
		t.Fatal("child log redaction did not preserve ordinary output")
	}
}

type unavailableLogWriter struct{}

func (unavailableLogWriter) Write([]byte) (int, error) { return 0, errors.New("log unavailable") }

func TestSupervisorLogWriteFailureUnblocksChild(t *testing.T) {
	output, err := newChildLogOutput(unavailableLogWriter{})
	if err != nil {
		t.Fatal(err)
	}
	defer output.close()
	done := make(chan error, 1)
	go func() { _, err := output.write.Write([]byte(strings.Repeat("ordinary-output ", 100000))); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("child output did not receive a failed-log write error")
		}
	case <-time.After(3 * time.Second):
		_ = output.read.Close()
		t.Fatal("failed log left child output blocked")
	}
}
