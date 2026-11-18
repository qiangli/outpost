package conf

import (
	"encoding/base64"

	"github.com/qiangli/yoke/pkg/secrets"
)

// Register credentials on load, persistence, and minting. Retired values stay
// masked for delayed log output after a rotation.
func (fc *FileConfig) registerLogSecrets() {
	if fc == nil {
		return
	}
	values := []struct{ name, value string }{
		{"OUTPOST_TUNNEL_TOKEN", fc.Token},
		{"OUTPOST_ACCESS_TOKEN", fc.AccessToken},
		{"OUTPOST_MCP_TOKEN", fc.MCPBearerToken},
		{"OUTPOST_CLOUD_TOKEN", fc.CloudDOToken},
		{"OUTPOST_LLM_KEY", fc.ClusterLLMAPIKey},
		{"OUTPOST_ADMIN_KEY", base64.StdEncoding.EncodeToString(fc.AdminSessionKey)},
		{"OUTPOST_FILES_KEY", base64.StdEncoding.EncodeToString(fc.FilesSigningKey)},
	}
	for _, value := range values {
		_ = secrets.RegisterHostSecret(value.name, value.value)
	}
	if fc.Cloudbox != nil {
		_ = secrets.RegisterHostSecret("OUTPOST_HOST_TOKEN", fc.Cloudbox.Token)
	}
	for _, attachment := range fc.Attachments {
		_ = secrets.RegisterHostSecret("OUTPOST_ATTACHMENT_TOKEN", attachment.Token)
		if tunnel := attachment.Tunnel; tunnel != nil {
			_ = secrets.RegisterHostSecret("OUTPOST_ATTACHMENT_TUNNEL", tunnel.Token)
			_ = secrets.RegisterHostSecret("OUTPOST_ATTACHMENT_ACCESS", tunnel.AccessToken)
		}
	}
	for _, app := range fc.Apps {
		_ = secrets.RegisterHostSecret("OUTPOST_APP_SSO", app.SSOSecret)
		_ = secrets.RegisterHostSecret("OUTPOST_APP_PROVISIONING", app.ProvisioningToken)
	}
	for _, value := range fc.ServiceSSO {
		_ = secrets.RegisterHostSecret("OUTPOST_SERVICE_SSO", value)
	}
	if c := fc.Cluster; c != nil {
		for _, value := range []string{c.Token, c.NodeToken, c.STCPSecret, c.CloudSTCPSecret, c.TunnelToken, c.JoinToken, c.OverlayAuthKey} {
			_ = secrets.RegisterHostSecret("OUTPOST_CLUSTER_CREDENTIAL", value)
		}
		_ = secrets.RegisterHostSecret("OUTPOST_CLUSTER_CLIENT_KEY", string(c.ClientKey))
		_ = secrets.RegisterHostSecret("OUTPOST_CLUSTER_ENCODED_KEY", base64.StdEncoding.EncodeToString(c.ClientKey))
		if f := c.Federation; f != nil {
			_ = secrets.RegisterHostSecret("OUTPOST_FEDERATION_TOKEN", f.Token)
			_ = secrets.RegisterHostSecret("OUTPOST_FEDERATION_SECRET", f.STCPSecret)
		}
	}
}
