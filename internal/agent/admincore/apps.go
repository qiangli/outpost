package admincore

import (
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// ListApps returns the apps slice from the on-disk FileConfig. Returns
// an empty slice (never nil) when no apps are registered, so JSON
// serialization stays a list.
func (s *Server) ListApps() ([]conf.AppConfig, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	if fc.Apps == nil {
		return []conf.AppConfig{}, nil
	}
	return fc.Apps, nil
}

// AppUpsertParams is the wire shape for adding or updating an app. The
// URL field is an alternative to the {Scheme, Host, Port, Socket}
// quartet — when non-empty, it is parsed via conf.AppTargetFromURL and
// wins over the split fields.
type AppUpsertParams struct {
	conf.AppConfig
	URL string `json:"url,omitempty"`
}

// UpsertApp validates the params, persists the merged FileConfig, and
// mutates the live AppRegistry. No restart required — AppRegistry is
// concurrent-safe.
func (s *Server) UpsertApp(p AppUpsertParams) (conf.AppConfig, error) {
	ac := p.AppConfig
	if strings.TrimSpace(p.URL) != "" {
		scheme, host, port, socket, err := conf.AppTargetFromURL(p.URL)
		if err != nil {
			return conf.AppConfig{}, badRequest("%s", err.Error())
		}
		ac.Scheme, ac.Host, ac.Port, ac.Socket = scheme, host, port, socket
	}
	if err := ValidateApp(&ac); err != nil {
		return conf.AppConfig{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return conf.AppConfig{}, err
	}

	// Preserve an existing ProvisioningToken across edits — the upsert
	// payload omits the token (callers never get it back in safeView)
	// and re-saving the AppConfig would otherwise blank it. Only auto-
	// generate when the toggle is on and we don't already have one;
	// rotation is a separate explicit operation so an accidental edit
	// can't quietly mint a new token and lock out the cooperating app.
	if ac.TrustCloudIdentity {
		if strings.TrimSpace(ac.ProvisioningToken) == "" {
			for _, existing := range fc.Apps {
				if existing.Name == ac.Name && existing.ProvisioningToken != "" {
					ac.ProvisioningToken = existing.ProvisioningToken
					break
				}
			}
		}
		if strings.TrimSpace(ac.ProvisioningToken) == "" {
			tok, terr := generateProvisioningToken()
			if terr != nil {
				return conf.AppConfig{}, internalErr("%s", terr.Error())
			}
			ac.ProvisioningToken = tok
		}
	} else {
		// Toggle off → drop the token. Otherwise a stale token would
		// linger in agent.json and still authenticate against the
		// relay endpoint (which would 401 since the registry no
		// longer carries it, but operator visibility is better when
		// off truly means off).
		ac.ProvisioningToken = ""
	}

	if fc.Apps == nil {
		fc.Apps = []conf.AppConfig{}
	}
	replaced := false
	for i, existing := range fc.Apps {
		if existing.Name == ac.Name {
			fc.Apps[i] = ac
			replaced = true
			break
		}
	}
	if !replaced {
		fc.Apps = append(fc.Apps, ac)
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return conf.AppConfig{}, internalErr("%s", err.Error())
	}
	// Reflect into the live registry. Unregister first to handle the
	// edit case (target URL changed) and the disable case.
	s.deps.Apps.Unregister(ac.Name)
	if ac.Enabled {
		if err := s.deps.Apps.RegisterFromConfig(ac); err != nil {
			return conf.AppConfig{}, internalErr("%s", err.Error())
		}
	}
	return ac, nil
}

// DeleteApp removes an app by name from FileConfig and from the live
// AppRegistry. No-op when the name isn't registered (idempotent — the
// SPA's "remove" button doesn't care about prior state).
func (s *Server) DeleteApp(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	filtered := fc.Apps[:0]
	for _, app := range fc.Apps {
		if app.Name != name {
			filtered = append(filtered, app)
		}
	}
	fc.Apps = filtered
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	s.deps.Apps.Unregister(name)
	return nil
}

// RotateProvisioningToken mints a new 32-byte hex bearer for the named
// app and updates both the persisted FileConfig and the live registry.
// Errors with 404 when the app doesn't exist and 400 when
// TrustCloudIdentity is off (rotation is only meaningful when the relay
// is in use).
func (s *Server) RotateProvisioningToken(name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	idx := -1
	for i, a := range fc.Apps {
		if a.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", notFound("unknown app")
	}
	if !fc.Apps[idx].TrustCloudIdentity {
		return "", badRequest("enable Trust cloud identity first; rotation only meaningful when the relay is in use")
	}
	tok, err := generateProvisioningToken()
	if err != nil {
		return "", internalErr("%s", err.Error())
	}
	fc.Apps[idx].ProvisioningToken = tok
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return "", internalErr("%s", err.Error())
	}
	// Reflect into the live registry without re-registering the proxy —
	// just update the bearer map so the relay accepts the new value
	// immediately. SetProvisioningToken is safe to call on a name that
	// isn't in the proxy/tcp maps (disabled apps).
	s.deps.Apps.SetProvisioningToken(name, tok)
	return tok, nil
}

// SetAppEnabled flips an app's Enabled flag without re-supplying the
// rest of its config — what `outpost apps stop`/`start` and the
// outpost_set_app_enabled MCP tool delegate to. Persists the change
// and updates the live AppRegistry: enabling re-mounts the proxy,
// disabling unregisters it. Idempotent — setting to the current value
// is a no-op (still returns the row so callers can confirm the state).
//
// This only flips the proxy gate. The upstream container/process is
// untouched — operators stop those out-of-band (e.g. `podman stop`).
// 404s when the app name isn't registered.
func (s *Server) SetAppEnabled(name string, enabled bool) (conf.AppConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return conf.AppConfig{}, err
	}
	idx := -1
	for i, a := range fc.Apps {
		if a.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return conf.AppConfig{}, notFound("unknown app")
	}
	fc.Apps[idx].Enabled = enabled
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return conf.AppConfig{}, internalErr("%s", err.Error())
	}
	// Reflect into the live registry. Unregister-then-conditionally-
	// reregister mirrors UpsertApp so a stale entry can't linger when
	// the toggle is flipped off.
	s.deps.Apps.Unregister(name)
	if enabled {
		if err := s.deps.Apps.RegisterFromConfig(fc.Apps[idx]); err != nil {
			return conf.AppConfig{}, internalErr("%s", err.Error())
		}
	}
	return fc.Apps[idx], nil
}

// generateProvisioningToken returns a 32-byte random hex string. Used
// for the per-app bearer the cooperating app sends when pushing user
// grants up to cloudbox via outpost's relay. crypto/rand failure is a
// hard error — a weak token defeats the whole point.
func generateProvisioningToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
