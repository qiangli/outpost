package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestTokenPrintCmd(t *testing.T) {
	tests := []struct {
		name       string
		fc         *conf.FileConfig
		wantStdout string
		wantErr    bool
	}{
		{
			name:       "prints access token only",
			fc:         &conf.FileConfig{AccessToken: "cloudbox-token-123"},
			wantStdout: "cloudbox-token-123",
		},
		{
			name:    "errors when no access token",
			fc:      &conf.FileConfig{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			cfgPath := filepath.Join(xdg, "matrix", "agent.json")
			if err := conf.SaveFile(cfgPath, tt.fc); err != nil {
				t.Fatalf("SaveFile: %v", err)
			}

			cmd := tokenCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"print"})

			err := cmd.Execute()
			if tt.wantErr {
				if err == nil {
					t.Fatal("Execute succeeded, want error")
				}
				if stdout.String() != "" {
					t.Fatalf("stdout = %q, want empty", stdout.String())
				}
				if strings.TrimSpace(stderr.String()) == "" {
					t.Fatal("stderr is empty, want diagnostic")
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if stdout.String() != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}
