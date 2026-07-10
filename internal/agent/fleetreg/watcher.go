// Package fleetreg pushes this host's fleet inventory — the tools, agents,
// and skills it has installed — up to cloudbox.
//
// It is the counterpart of internal/agent/ollama's model-registry watcher, and
// deliberately the same shape: the host is the source of truth about itself,
// cloudbox caches that truth behind a freshness clock, and a content hash lets
// an unchanged snapshot cost one UPDATE instead of a rewrite.
//
// The inventory is read straight out of coreutils/pkg/fleet rather than by
// shelling out to `bashy`. Outpost already depends on coreutils, so the
// registry is a library call — and a host that has no bashy binary installed
// still has a fleet, because the compiled-in baseline is part of the library.
package fleetreg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/assetring"
	"github.com/qiangli/coreutils/pkg/fleet"
)

const (
	defaultPollInterval      = 5 * time.Minute
	defaultHeartbeatInterval = 5 * time.Minute
	defaultHTTPTimeout       = 20 * time.Second
	registryPath             = "/api/v1/fleet/registry"
)

// Asset is one reported tool / agent / skill.
type Asset struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Display string `json:"display,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type payload struct {
	AgentName   string    `json:"agent_name"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	Assets      []Asset   `json:"assets"`
	ContentHash string    `json:"content_hash,omitempty"`
}

// Config configures the watcher.
type Config struct {
	// CloudboxURL is the base URL (e.g. https://ai.dhnt.io).
	CloudboxURL string
	// AccessToken is the per-outpost bearer JWT. It must carry
	// fleet:registry — the scope minted for a HOST reporting a fact about
	// itself, not for a user authoring a definition.
	AccessToken string
	// AgentName is the host name cloudbox knows this outpost by.
	AgentName string

	PollInterval      time.Duration
	HeartbeatInterval time.Duration
	HTTP              *http.Client

	// Catalog is indirected for tests. Nil uses the host's real registry.
	Catalog func() *fleet.Catalog
	// Skills lists the skills installed in the host's local store. Nil uses
	// the default reader.
	//
	// It reads directory names rather than importing coreutils/pkg/skills:
	// that package pulls the dhnt skill-CNL runtime, and outpost is the lean
	// mesh supervisor. A name is all the inventory needs — a peer asking
	// "does host-a have the conductor skill" wants a yes, not the procedure.
	Skills func() []string
}

// Watcher polls the local fleet registry and pushes changes.
type Watcher struct {
	cfg Config
}

// New validates cfg and constructs a Watcher.
func New(cfg Config) (*Watcher, error) {
	if strings.TrimSpace(cfg.CloudboxURL) == "" {
		return nil, fmt.Errorf("fleetreg: CloudboxURL is required")
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return nil, fmt.Errorf("fleetreg: AgentName is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaultHeartbeatInterval
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.Catalog == nil {
		cfg.Catalog = func() *fleet.Catalog { return fleet.New() }
	}
	if cfg.Skills == nil {
		cfg.Skills = localSkillNames
	}
	cfg.CloudboxURL = strings.TrimRight(cfg.CloudboxURL, "/")
	return &Watcher{cfg: cfg}, nil
}

// Snapshot reads the host's current inventory.
//
// Tools are the agentic-CLI kind only. A function kit is not something this
// host can be asked to launch, so reporting one would advertise a capability
// that does not exist.
func (w *Watcher) Snapshot() []Asset {
	cat := w.cfg.Catalog()
	var out []Asset

	tools, _ := cat.Tools(false)
	for _, t := range tools {
		detail := t.CLI.Binary
		if !t.TakesModel() {
			detail += " (cannot select a model)"
		}
		out = append(out, Asset{
			Kind: "tool", Name: t.Name, Display: t.Display, Detail: detail,
		})
	}

	agents, _ := cat.Agents()
	for _, a := range agents {
		// The binding IS the useful detail: a peer deciding whether to send
		// work here needs to know which model this agent runs, not merely
		// that a nickname exists.
		out = append(out, Asset{
			Kind: "agent", Name: a.Name, Display: a.Display, Detail: a.MatrixKey(),
		})
	}

	for _, name := range w.cfg.Skills() {
		out = append(out, Asset{Kind: "skill", Name: name})
	}

	sortAssets(out)
	return out
}

// skillDir is the host-local skill store bashy writes. $BASHY_SKILLS_DIR
// overrides it, exactly as it does for bashy itself.
func skillDir() string {
	if d := os.Getenv("BASHY_SKILLS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "bashy", "skills")
}

// localSkillNames lists the skills installed on this host. A missing store is
// an empty list, not an error: most hosts have installed none.
func localSkillNames() []string {
	dir := skillDir()
	if dir == "" {
		return nil
	}
	names, err := assetring.FolderDir(dir, assetring.RingLocal, "SKILL.md").Names()
	if err != nil {
		return nil
	}
	return names
}

func sortAssets(a []Asset) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].Kind != a[j].Kind {
			return a[i].Kind < a[j].Kind
		}
		return a[i].Name < a[j].Name
	})
}

// ContentHash is a deterministic sha256 over the reported inventory.
//
// The push order cannot change it (assets are sorted), and no timestamp enters
// it — the whole point is that an unchanged host hashes the same across polls,
// so cloudbox can skip the rewrite and just move the freshness clock.
func ContentHash(assets []Asset) string {
	sorted := append([]Asset(nil), assets...)
	sortAssets(sorted)
	h := sha256.New()
	for _, a := range sorted {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x1e", a.Kind, a.Name, a.Display, a.Detail)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Push sends one snapshot. It returns the server's `applied` verdict
// ("replaced" or "touched") so a caller can log what actually happened.
func (w *Watcher) Push(ctx context.Context, assets []Asset, contentHash string) (string, error) {
	body, err := json.Marshal(payload{
		AgentName:   w.cfg.AgentName,
		HeartbeatAt: time.Now().UTC(),
		Assets:      assets,
		ContentHash: contentHash,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.CloudboxURL+registryPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.cfg.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.cfg.AccessToken)
	}
	resp, err := w.cfg.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", &PushError{Status: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var ack struct {
		Applied string `json:"applied"`
	}
	_ = json.Unmarshal(raw, &ack)
	return ack.Applied, nil
}

// PushError carries the HTTP status so the caller can honor the endpoint's
// contract: 401 means stop (revoked or unknown token), 403 means the token
// lacks fleet:registry, 404 means this host is not paired under that owner.
// Only 5xx is worth retrying.
type PushError struct {
	Status int
	Body   string
}

func (e *PushError) Error() string {
	return fmt.Sprintf("fleetreg: push failed: %d: %s", e.Status, e.Body)
}

// Fatal reports whether retrying can never help.
func (e *PushError) Fatal() bool { return e.Status >= 400 && e.Status < 500 }

// Run polls until ctx is cancelled, pushing when the inventory changes and on
// a heartbeat so the freshness clock keeps ticking.
//
// A failed push is logged and retried on the next tick; it never takes the
// daemon down. The inventory is a convenience for peers, not a dependency of
// anything running on this host.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	var lastHash string
	var lastPush time.Time

	tick := func() {
		assets := w.Snapshot()
		hash := ContentHash(assets)
		due := time.Since(lastPush) >= w.cfg.HeartbeatInterval
		if hash == lastHash && !due {
			return
		}
		applied, err := w.Push(ctx, assets, hash)
		if err != nil {
			var pe *PushError
			if ok := asPushError(err, &pe); ok && pe.Fatal() {
				slog.Warn("fleetreg: push rejected; not retrying until config changes",
					"status", pe.Status, "body", pe.Body)
				// Keep the hash so an unchanged inventory stops re-asking.
				lastHash, lastPush = hash, time.Now()
				return
			}
			slog.Debug("fleetreg: push failed; will retry", "err", err)
			return
		}
		slog.Debug("fleetreg: pushed", "assets", len(assets), "applied", applied)
		lastHash, lastPush = hash, time.Now()
	}

	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

func asPushError(err error, out **PushError) bool {
	pe, ok := err.(*PushError)
	if ok {
		*out = pe
	}
	return ok
}
