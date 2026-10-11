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
	return ac
}

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
	v.BashyServiceSecrets = nil
	if v.BashyServices != nil {
		svcs := make([]conf.BashyService, len(v.BashyServices))
		for i, b := range v.BashyServices {
			if b.SSOSecret != "" {
				v.BashyServiceSecrets = append(v.BashyServiceSecrets, SecretPresence{Name: b.Name, HasSSOSecret: true})
			}
			b.SSOSecret = ""
			svcs[i] = b
		}
		v.BashyServices = svcs
	}
	return v
}
