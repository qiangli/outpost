// Package config loads per-environment settings. It is 12-factor: environment
// variables always win; an optional config.<env>.json supplies defaults so an
// operator can commit non-secret per-env values (never secrets) to the repo.
package config

import (
	"encoding/json"
	"os"
	"strings"
)

// Config is the app's resolved settings for one environment.
type Config struct {
	// Env is dev|qa|prod. Selects config.<env>.json and labels the deployment.
	Env string `json:"env"`
	// Addr is the listen address. Keep it loopback (127.0.0.1) — outpost is the
	// only ingress; the tunnel reaches this port over loopback.
	Addr string `json:"addr"`
	// SSOSecret is the per-app secret shared with outpost. Load from the
	// environment / a mounted secret, never from a committed config file.
	SSOSecret string `json:"-"`
	// RequireHMAC rejects cloud requests without a valid identity signature.
	RequireHMAC bool `json:"require_hmac"`
	// AdminEmails is the app-internal admin allowlist (RBAC).
	AdminEmails []string `json:"admin_emails"`
}

// Load resolves config for APP_ENV (default "dev"): defaults ← config.<env>.json
// ← environment variables. dir is where the JSON files live (typically ".").
func Load(dir string) (Config, error) {
	env := getenv("APP_ENV", "dev")
	c := Config{Env: env, Addr: "127.0.0.1:8080", RequireHMAC: true}

	// Optional per-env JSON defaults (non-secret).
	if b, err := os.ReadFile(filePath(dir, env)); err == nil {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, err
		}
		c.Env = env // never let the file override the selected env
	} else if !os.IsNotExist(err) {
		return c, err
	}

	// Environment overrides (12-factor). Secrets only come from here.
	if v := os.Getenv("HELLO_TESSARO_ADDR"); v != "" {
		c.Addr = v
	}
	c.SSOSecret = os.Getenv("HELLO_TESSARO_SSO_SECRET")
	if v := os.Getenv("HELLO_TESSARO_REQUIRE_HMAC"); v != "" {
		c.RequireHMAC = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("HELLO_TESSARO_ADMIN_EMAILS"); v != "" {
		c.AdminEmails = splitList(v)
	}
	return c, nil
}

// AdminSet returns AdminEmails as a lookup set.
func (c Config) AdminSet() map[string]bool {
	m := make(map[string]bool, len(c.AdminEmails))
	for _, e := range c.AdminEmails {
		if e = strings.TrimSpace(e); e != "" {
			m[e] = true
		}
	}
	return m
}

func filePath(dir, env string) string {
	if dir == "" {
		dir = "."
	}
	return dir + "/config." + env + ".json"
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
