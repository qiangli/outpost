package conf

import (
	"bytes"
	"encoding/json"
	"github.com/qiangli/yoke/pkg/secrets"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMintAndRotateRegisterCredentialsForLogs(t *testing.T) {
	var out bytes.Buffer
	writer := secrets.NewHostRedactingWriter(&out)
	fc := &FileConfig{}
	first, err := EnsureMCPBearerToken("", fc)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte("before " + first + "\n"))
	second, err := RotateMCPBearerToken("", fc)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = writer.Write([]byte(first + " " + second))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), first) || strings.Contains(out.String(), second) {
		t.Fatal("minted or rotated MCP credential survived log sink")
	}
}

func TestLoadedCredentialsRegisterForLogs(t *testing.T) {
	fc := &FileConfig{MCPBearerToken: "synthetic-load-token-private", Apps: []AppConfig{{Name: "demo", SSOSecret: "synthetic-load-sso-private"}}, Cloudbox: &CloudboxPairing{Token: "synthetic-cloudbox-private-token"}, Attachments: []SatelliteAttachment{{Token: "synthetic-attachment-private-token"}}, Cluster: &ClusterConfig{Token: "synthetic-cluster-api-private-token", JoinToken: "synthetic-load-join-private", Federation: &FederationConfig{Token: "synthetic-load-fed-private"}}}
	path := filepath.Join(t.TempDir(), "agent.json")
	data, err := json.Marshal(fc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fc.MCPBearerToken, fc.Apps[0].SSOSecret, fc.Cluster.JoinToken, fc.Cluster.Federation.Token, fc.Cloudbox.Token, fc.Attachments[0].Token, fc.Cluster.Token} {
		if bytes.Contains(secrets.HostRedactor().Redact([]byte(value)), []byte(value)) {
			t.Fatal("loaded credential was not registered")
		}
	}
}
