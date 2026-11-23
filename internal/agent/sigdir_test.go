package agent

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"golang.org/x/crypto/ssh"
)

// RFC 8037 appendix A.3: the Ed25519 JWK thumbprint test vector, the same
// one bashy's signer (yoke/pkg/webbotauth) and the site template check.
func TestJWKThumbprintRFC8037(t *testing.T) {
	x, _ := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	if got, want := jwkThumbprint(ed25519.PublicKey(x)), "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k"; got != want {
		t.Fatalf("thumbprint %s, want %s", got, want)
	}
}

func TestSignatureDirectoryOnRootAndSiteMount(t *testing.T) {
	signer, err := loadOrCreateHostKeyAt(filepath.Join(t.TempDir(), "ssh_host_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	prev := signatureDirectoryKey
	signatureDirectoryKey = func() (ssh.Signer, error) { return signer, nil }
	t.Cleanup(func() { signatureDirectoryKey = prev })
	pub, _ := ed25519Public(signer)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("directory request reached the site engine: %s", r.URL.Path)
	}))
	t.Cleanup(engine.Close)
	host, port := splitHostPort(t, engine.URL)
	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{Name: "site", Scheme: "http", Host: host, Port: port, Enabled: true,
		RequireLogin: true, TrustCloudIdentity: true, SSOSecret: "k", MCPPort: port}); err != nil {
		t.Fatal(err)
	}
	reg.SetHostGrants(NewHostGrants("owner@example.com"))
	router := newTestRouter(reg)
	router.GET(signatureDirectoryPath, serveSignatureDirectory)
	front := httptest.NewServer(router)
	t.Cleanup(front.Close)

	for _, path := range []string{signatureDirectoryPath, "/app/site" + signatureDirectoryPath} {
		req, _ := http.NewRequest(http.MethodGet, front.URL+path, nil)
		// Anonymous through the cloud front on a login-required site: the
		// directory is public like any key directory.
		req.Header.Set("X-Forwarded-Prefix", "/h/host-a/app/site")
		req.Header.Set("X-Forwarded-Host", "Site.Example")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var set struct {
			Keys []struct{ Kty, Crv, X, Kid string }
		}
		err = json.NewDecoder(resp.Body).Decode(&set)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != signatureDirectoryMediaType {
			t.Fatalf("%s: %d %q %v", path, resp.StatusCode, resp.Header.Get("Content-Type"), err)
		}
		if len(set.Keys) != 1 || set.Keys[0].Kty != "OKP" || set.Keys[0].Crv != "Ed25519" ||
			set.Keys[0].X != base64.RawURLEncoding.EncodeToString(pub) || set.Keys[0].Kid != jwkThumbprint(pub) {
			t.Fatalf("%s: keys %+v", path, set.Keys)
		}
		input := resp.Header.Get("Signature-Input")
		params, ok := strings.CutPrefix(input, "binding0=")
		if !ok || !strings.Contains(params, `tag="http-message-signatures-directory"`) || !strings.Contains(params, `keyid="`+jwkThumbprint(pub)+`"`) {
			t.Fatalf("%s: Signature-Input %q", path, input)
		}
		raw, _ := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(resp.Header.Get("Signature"), "binding0=:"), ":"))
		base := fmt.Sprintf("%q: %s\n%q: %s", "@authority", "site.example", "@signature-params", params)
		if !ed25519.Verify(pub, []byte(base), raw) {
			t.Fatalf("%s: directory response signature does not verify", path)
		}
	}
}

// The signed authority is the Host unless the cloud front (X-Forwarded-Prefix)
// names a forwarded host, and never anything that could break line framing.
func TestSignatureDirectoryAuthority(t *testing.T) {
	for _, tc := range []struct {
		host, fwdHost, prefix, want string
		ok                          bool
	}{
		{"127.0.0.1:8080", "", "", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "evil.example", "", "127.0.0.1:8080", true},
		{"127.0.0.1:8080", "Site.Example", "/h/host-a/app/site", "site.example", true},
		{"127.0.0.1:8080", "a.example, b.example", "/h/x", "a.example", true},
		{"[::1]:8080", "", "", "[::1]:8080", true},
		{"127.0.0.1", "evil.example\n\"@method\": GET", "/h/x", "", false},
		{"bad host", "", "", "", false},
		{"", "", "", "", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://placeholder"+signatureDirectoryPath, nil)
		r.Host = tc.host
		if tc.fwdHost != "" {
			r.Header["X-Forwarded-Host"] = []string{tc.fwdHost}
		}
		if tc.prefix != "" {
			r.Header.Set("X-Forwarded-Prefix", tc.prefix)
		}
		got, ok := directoryAuthority(r)
		if got != tc.want || ok != tc.ok {
			t.Errorf("host %q fwd %q prefix %q: got %q %v, want %q %v", tc.host, tc.fwdHost, tc.prefix, got, ok, tc.want, tc.ok)
		}
	}
}
