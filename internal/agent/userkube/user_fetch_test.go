package userkube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validUserKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: cloud
  cluster:
    server: https://cluster.example.test
users:
- name: owner
  user:
    token: redacted-test-token
contexts:
- name: owner@cloud
  context:
    cluster: cloud
    user: owner
current-context: owner@cloud
`

func TestFetchUserKubeconfigYAMLValidatesResponse(t *testing.T) {
	const accessToken = "sensitive-access-token"
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		want        string
		wantSuccess bool
	}{
		{
			name: "valid YAML",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != UserKubeconfigEndpoint {
					t.Errorf("path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
				_, _ = w.Write([]byte(validUserKubeconfig))
			},
			wantSuccess: true,
		},
		{
			name: "200 SPA HTML",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<!doctype html><html>tessaro login</html>"))
			},
			want: "unexpected content-type",
		},
		{
			name: "redirect to login",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == UserKubeconfigEndpoint {
					http.Redirect(w, r, "/login?access_token="+accessToken, http.StatusFound)
					return
				}
				_, _ = w.Write([]byte("<html>login</html>"))
			},
			want: "HTTP 302 redirect",
		},
		{
			name: "truncated YAML",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/yaml")
				_, _ = w.Write([]byte("apiVersion: v1\nkind: Config\nclusters:\n- name: cut\n"))
			},
			want: "invalid kubeconfig",
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("<html>internal details</html>"))
			},
			want: "HTTP 500",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()
			data, err := FetchUserKubeconfigYAML(context.Background(), server.URL, accessToken)
			if tt.wantSuccess {
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != validUserKubeconfig {
					t.Fatal("valid response bytes changed")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), UserKubeconfigEndpoint) {
				t.Fatalf("error lacks actionable endpoint: %v", err)
			}
			if strings.Contains(err.Error(), accessToken) {
				t.Fatalf("error leaked access token: %v", err)
			}
		})
	}
}

func TestValidateKubeconfigRequiresCompleteHTTPSCurrentContext(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"HTML with YAML content type", "<html>login</html>", "HTML/login SPA"},
		{"HTTP server", strings.Replace(validUserKubeconfig, "https://cluster", "http://cluster", 1), "absolute https URL"},
		{"missing users", strings.Replace(validUserKubeconfig, "users:", "missing-users:", 1), "users: must not be empty"},
		{"missing contexts", strings.Replace(validUserKubeconfig, "contexts:", "missing-contexts:", 1), "contexts: must not be empty"},
		{"missing current context", strings.Replace(validUserKubeconfig, "current-context: owner@cloud", "current-context:", 1), "current-context"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ValidateKubeconfig([]byte(tt.data)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWriteStandaloneNeverClobbersValidCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster-kubeconfig.yaml")
	if err := WriteStandalone([]byte(validUserKubeconfig), path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteStandalone([]byte("<html>login</html>"), path); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("invalid response clobbered the prior valid cache")
	}
}

func TestFetchUserAndWritePreservesPriorCacheOnFetchFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster-kubeconfig.yaml")
	if err := WriteStandalone([]byte(validUserKubeconfig), path); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html>login</html>"))
	}))
	defer server.Close()
	if _, err := FetchUserAndWrite(context.Background(), server.URL, "access-token", path); err == nil {
		t.Fatal("HTML fetch unexpectedly succeeded")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != validUserKubeconfig {
		t.Fatal("failed fetch clobbered prior valid cache")
	}
}
