package ollama

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// BashyManagedPort is bashy's own Ollama engine port — deliberately NOT
// 11434, so a bashy-managed daemon never fights a host Ollama. Mirrors
// yoke's DefaultManagedPort (yoke/external/ollama/managed.go) and the
// door raw engine's OLLAMA_HOST (bashy internal/agentos/engines_stub.go).
// Kept as a literal here so outpost does not import yoke: the two must
// stay equal, and TestBashyOllamaURL pins this side.
const BashyManagedPort = 11435

// BashyOllamaURL resolves bashy's own Ollama engine base URL, mirroring
// yoke's managedPort contract: $BASHY_OLLAMA_PORT wins when it parses
// (garbage falls back to BashyManagedPort, exactly like yoke); a
// non-positive port means OS-ephemeral — no stable URL exists — so it
// returns "" and callers skip the extra daemon.
func BashyOllamaURL() string {
	port := BashyManagedPort
	if v := strings.TrimSpace(os.Getenv("BASHY_OLLAMA_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}
