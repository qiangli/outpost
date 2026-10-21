package admincore

import "github.com/qiangli/outpost/internal/agent/conf"

// SecretPresence reports which credentials a redacted app or bashy service
// holds, so a redacted view stays useful without carrying the values.
type SecretPresence struct {
	Name                 string `json:"name"`
	HasProvisioningToken bool   `json:"has_provisioning_token,omitempty"`
	HasSSOSecret         bool   `json:"has_sso_secret"`
}

// RedactApp blanks the per-app credentials (provisioning bearer and SSO HMAC
// key). Only the interactive admin session and the dedicated secret/rotate
// tools may hand those out.
func RedactApp(ac conf.AppConfig) conf.AppConfig {
	ac.ProvisioningToken = ""
	ac.SSOSecret = ""
	// The expose digest is not a credential, but it identifies one, so
	// no surface prints it in full: keep the first 8 hex characters —
	// enough to tell two exposes apart in a listing, not enough to
	// reconstruct the hash.
	if len(ac.ExposeTokenSHA256) > exposeDigestPreview {
		ac.ExposeTokenSHA256 = ac.ExposeTokenSHA256[:exposeDigestPreview]
	}
	return ac
}

// exposeDigestPreview is how many leading hex characters of an expose
// digest a redacted view may carry.
const exposeDigestPreview = 8

// RedactApps returns a redacted copy; the input slice is left untouched.
func RedactApps(apps []conf.AppConfig) []conf.AppConfig {
	out := make([]conf.AppConfig, len(apps))
	for i, a := range apps {
		out[i] = RedactApp(a)
	}
	return out
}

// Redacted returns a copy of the view with nested app and bashy-service
// credentials removed and their presence reported in AppSecrets /
// BashyServiceSecrets. Top-level secrets are never in SafeView at all.
func (v SafeView) Redacted() SafeView {
	v.AppSecrets = nil
	for _, a := range v.Apps {
		if a.ProvisioningToken != "" || a.SSOSecret != "" {
			v.AppSecrets = append(v.AppSecrets, SecretPresence{
				Name:                 a.Name,
				HasProvisioningToken: a.ProvisioningToken != "",
				HasSSOSecret:         a.SSOSecret != "",
			})
		}
	}
	v.Apps = RedactApps(v.Apps)
	// A service unit's view never carries a secret in the first place — the
	// declaration holds vault REFERENCES, and the trusted-identity HMAC keys
	// live in outpost's own config. Report which app routes have one.
	v.ServiceSecrets = nil
	for _, app := range sortedKeys(v.serviceSSO) {
		if v.serviceSSO[app] != "" {
			v.ServiceSecrets = append(v.ServiceSecrets, SecretPresence{Name: app, HasSSOSecret: true})
		}
	}
	v.serviceSSO = nil
	return v
}
