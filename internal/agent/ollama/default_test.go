package ollama

import "testing"

// Pins the upstream default next to the constant, so an accidental edit
// of DefaultURL fails here instead of surfacing as a far-away
// integration miss (and DefaultPort can never silently disagree with the
// URL it is derived from).
func TestDefaultURLPinsOllamaUpstreamDefault(t *testing.T) {
	if DefaultURL != "http://127.0.0.1:11434" {
		t.Fatalf("DefaultURL = %q; the host daemon default is Ollama's own upstream bind http://127.0.0.1:11434", DefaultURL)
	}
	if DefaultPort() != 11434 {
		t.Fatalf("DefaultPort() = %d, want 11434 (the port of DefaultURL)", DefaultPort())
	}
}
