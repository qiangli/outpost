package agent

import "testing"

func TestOllamaBaseURL_DefaultWhenEnvUnset(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:11434" {
		t.Errorf("ollamaBaseURL()=%q, want default", got)
	}
}

func TestOllamaBaseURL_HostPortForm(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:8000")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:8000" {
		t.Errorf("ollamaBaseURL()=%q, want http://127.0.0.1:8000", got)
	}
}

func TestOllamaBaseURL_FullURLForm(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "https://ollama.example.com:9999/")
	if got := ollamaBaseURL(); got != "https://ollama.example.com:9999" {
		t.Errorf("ollamaBaseURL()=%q, want trimmed URL", got)
	}
}

func TestOllamaBaseURL_WhitespaceTolerant(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "  ")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:11434" {
		t.Errorf("whitespace-only OLLAMA_HOST should fall back to default, got %q", got)
	}
}
