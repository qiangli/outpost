package ollama

import (
	"net/url"
	"strconv"
)

// DefaultURL is the host Ollama daemon's own default endpoint — the
// upstream `ollama serve` bind. This is the single place in outpost the
// port number is written: the detector, the ollama unit's fallback, the
// shard/warm plane's default and the suggestion surfaces all resolve
// through here (or through the unit's configured URL) instead of copying
// the literal. internal/agent/ports_lint_test.go fails on literal copies
// anywhere else.
const DefaultURL = "http://127.0.0.1:11434"

// DefaultPort is the port of DefaultURL. Parsed rather than re-declared
// so the number cannot drift from the URL.
func DefaultPort() int {
	u, err := url.Parse(DefaultURL)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return p
}
