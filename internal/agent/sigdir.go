package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

// The host's HTTP Message Signatures directory (Web Bot Auth): the public
// half of the persistent host key as a JWK Set, so a site can verify that a
// request signed by this host's bashy agent (`bashy fetch --sign`, `bashy
// site ... --sign`) came from it. Public by design, like any key directory;
// served at the outpost root and on every site app mount, where it gives the
// owner's agent a same-origin Signature-Agent URL.
const (
	signatureDirectoryPath      = "/.well-known/http-message-signatures-directory"
	signatureDirectoryMediaType = "application/http-message-signatures-directory+json"
	signatureDirectoryTag       = "http-message-signatures-directory"
	signatureDirectoryTTL       = 5 * time.Minute
)

// signatureDirectoryKey loads the host key the directory publishes; a var so
// tests stay off the real config dir.
var signatureDirectoryKey = LoadOrCreateHostKey

// jwkThumbprint is the RFC 7638 SHA-256 thumbprint of an Ed25519 key: the
// keyid signers put in Signature-Input.
func jwkThumbprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + base64.RawURLEncoding.EncodeToString(pub) + `"}`))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func ed25519Public(signer ssh.Signer) (ed25519.PublicKey, bool) {
	ck, ok := signer.PublicKey().(ssh.CryptoPublicKey)
	if !ok {
		return nil, false
	}
	pub, ok := ck.CryptoPublicKey().(ed25519.PublicKey)
	return pub, ok
}

// serveSignatureDirectory answers the directory, signing the response over
// @authority with the published key so a fetcher can check possession.
func serveSignatureDirectory(c *gin.Context) {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		c.AbortWithStatus(http.StatusMethodNotAllowed)
		return
	}
	signer, err := signatureDirectoryKey()
	if err != nil || signer == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	pub, ok := ed25519Public(signer)
	if !ok {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	kid := jwkThumbprint(pub)
	body, _ := json.Marshal(gin.H{"keys": []gin.H{{
		"kty": "OKP", "crv": "Ed25519", "x": base64.RawURLEncoding.EncodeToString(pub), "kid": kid,
	}}})
	c.Header("Cache-Control", "public, max-age=300")
	authority, ok := directoryAuthority(c.Request)
	if !ok {
		// An authority that cannot be framed safely is served unsigned.
		c.Data(http.StatusOK, signatureDirectoryMediaType, body)
		return
	}
	created := time.Now().Unix()
	params := `("@authority");created=` + strconv.FormatInt(created, 10) +
		";expires=" + strconv.FormatInt(created+int64(signatureDirectoryTTL/time.Second), 10) +
		`;keyid="` + kid + `";alg="ed25519";tag="` + signatureDirectoryTag + `"`
	base := fmt.Sprintf("%q: %s\n%q: %s", "@authority", authority, "@signature-params", params)
	sig, err := signer.Sign(rand.Reader, []byte(base))
	if err == nil && sig.Format == ssh.KeyAlgoED25519 {
		c.Header("Signature-Input", "binding0="+params)
		c.Header("Signature", "binding0=:"+base64.StdEncoding.EncodeToString(sig.Blob)+":")
	}
	c.Data(http.StatusOK, signatureDirectoryMediaType, body)
}

// directoryAuthority is the @authority the directory response is signed
// over: the request Host, or X-Forwarded-Host only behind the cloud front
// (which sets X-Forwarded-Prefix). It must be a plain host[:port] so no
// client-supplied byte can break the signature base's line framing.
func directoryAuthority(r *http.Request) (string, bool) {
	authority := r.Host
	if r.Header.Get("X-Forwarded-Prefix") != "" {
		if fwd := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); fwd != "" {
			authority = fwd
		}
	}
	authority = strings.ToLower(strings.TrimSpace(authority))
	if authority == "" || len(authority) > 255 {
		return "", false
	}
	for _, ch := range authority {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || strings.ContainsRune(".-:[]", ch)) {
			return "", false
		}
	}
	return authority, true
}
