package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
)

// PushConfig is what main.go threads into the manager to enable
// cloudbox push. Empty CloudboxBase OR AccessToken disables push
// silently (the worker still runs locally — useful for offline
// testing or for outposts not yet paired).
type PushConfig struct {
	CloudboxBase string
	AccessToken  string
	AgentName    string

	// IdentityPath is where the persistent age identity lives. Empty
	// = use backup.DefaultIdentityPath(). The identity is generated
	// on first push if missing.
	IdentityPath string

	// HTTPClient is optional — tests can inject a custom transport.
	// Default = a fresh http.Client with a 60s timeout, generous
	// enough for a 500 MiB upload over a slow link.
	HTTPClient *http.Client
}

// Pusher encrypts a candidate's file with age and POSTs it to
// cloudbox's /api/v1/backup/artifact. Returns the cloudbox-assigned
// artifact id + the ciphertext sha256 on success.
//
// Cloudbox-side route lives in cloudbox/hub/internal/handlers/
// v1_backup.go:V1BackupCreateArtifact and stores the blob under
// <cloudbox cfg.Base>/blobstore/backup/<owner>/<artifact-id>.bin.
type Pusher struct {
	cfg    PushConfig
	client *http.Client

	// Recipient lazily resolved on first push so a daemon that never
	// pushes doesn't generate a key file.
	id  *age.X25519Identity
	rec *age.X25519Recipient
}

// NewPusher constructs a Pusher. cfg.CloudboxBase + cfg.AccessToken
// are required at push time but may be empty here — Configured()
// reports the truth.
func NewPusher(cfg PushConfig) *Pusher {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.IdentityPath == "" {
		cfg.IdentityPath = DefaultIdentityPath()
	}
	return &Pusher{cfg: cfg, client: cfg.HTTPClient}
}

// Configured reports whether a push attempt has any chance of
// succeeding (cloudbox base + access token + agent name present).
// Callers gate on this to avoid wasting work + ledger noise on
// unpaired hosts.
func (p *Pusher) Configured() bool {
	return p != nil &&
		strings.TrimSpace(p.cfg.CloudboxBase) != "" &&
		strings.TrimSpace(p.cfg.AccessToken) != "" &&
		strings.TrimSpace(p.cfg.AgentName) != ""
}

// PushResult is what Push returns on success — the cloudbox-assigned
// artifact id, the recipient fingerprint used, and the sha256 of the
// uploaded ciphertext (distinct from Candidate.SHA256 which is the
// plaintext sha).
type PushResult struct {
	ArtifactID   string
	CipherSHA256 string
	KeyID        string
}

// Push encrypts c.Path with the resolved age recipient and POSTs the
// resulting blob to cloudbox. Returns a PushResult on success. The
// caller is expected to stamp the result fields onto its Candidate
// before writing to the ledger.
//
// Errors fall into three buckets:
//   - Configuration: unpaired host, missing key — surface to operator.
//   - Encrypt: filesystem / age failure — likely transient.
//   - Network/Server: cloudbox unreachable or 4xx/5xx — retry-eligible.
//
// All are returned as plain Go errors; the manager logs + records
// them on the Candidate's PushError field.
func (p *Pusher) Push(ctx context.Context, c Candidate, app string) (PushResult, error) {
	if !p.Configured() {
		return PushResult{}, errors.New("pusher: cloudbox not configured (unpaired host or missing access token)")
	}
	if c.Path == "" || c.SHA256 == "" || c.Size <= 0 {
		return PushResult{}, errors.New("pusher: candidate missing path/sha256/size")
	}
	if err := p.ensureIdentity(); err != nil {
		return PushResult{}, err
	}

	// Encrypt to a temp ciphertext file rather than buffering in RAM
	// — a 50 MiB classgo ZIP fits in memory but a future kg dump
	// won't. Tee through sha256 so we don't reread the ciphertext.
	tmp, cipherSHA, cipherSize, err := p.encryptToTemp(c.Path)
	if err != nil {
		return PushResult{}, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()

	// Build a multipart body. The body is constructed in-memory
	// (form fields are tiny + the blob is streamed). For >100 MiB
	// payloads we'd swap to a pipe writer, but at v1 sizes the
	// simpler shape is fine.
	body, contentType, err := buildMultipartBody(tmp, cipherSize, cipherSHA, p.cfg.AgentName, app, p.rec)
	if err != nil {
		return PushResult{}, fmt.Errorf("pusher: build multipart: %w", err)
	}

	url := strings.TrimRight(p.cfg.CloudboxBase, "/") + "/api/v1/backup/artifact"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return PushResult{}, fmt.Errorf("pusher: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.AccessToken)
	req.Header.Set("Content-Type", contentType)

	resp, err := p.client.Do(req)
	if err != nil {
		return PushResult{}, fmt.Errorf("pusher: POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return PushResult{}, fmt.Errorf("pusher: cloudbox %s: %s", resp.Status, strings.TrimSpace(string(buf)))
	}
	var out struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
		KeyID  string `json:"key_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PushResult{}, fmt.Errorf("pusher: decode response: %w", err)
	}
	return PushResult{
		ArtifactID:   out.ID,
		CipherSHA256: cipherSHA,
		KeyID:        out.KeyID,
	}, nil
}

// ensureIdentity lazily resolves the age identity + recipient on
// first push. Subsequent pushes reuse them.
func (p *Pusher) ensureIdentity() error {
	if p.id != nil && p.rec != nil {
		return nil
	}
	if p.cfg.IdentityPath == "" {
		return errors.New("pusher: no age identity path resolved (UserCacheDir unavailable?)")
	}
	id, rec, err := LoadOrCreateIdentity(p.cfg.IdentityPath)
	if err != nil {
		return err
	}
	p.id, p.rec = id, rec
	return nil
}

// encryptToTemp opens plainPath, age-encrypts it through a tee'd
// sha256, and writes the ciphertext to a tempfile in os.TempDir().
// Returns the open ciphertext file (caller closes + removes), the
// hex sha256 of the ciphertext, and its size.
func (p *Pusher) encryptToTemp(plainPath string) (*os.File, string, int64, error) {
	plain, err := os.Open(plainPath)
	if err != nil {
		return nil, "", 0, fmt.Errorf("pusher: open plain %s: %w", plainPath, err)
	}
	defer plain.Close()

	tmpDir := os.TempDir()
	tmp, err := os.CreateTemp(tmpDir, "outpost-backup-*.age")
	if err != nil {
		return nil, "", 0, fmt.Errorf("pusher: create temp: %w", err)
	}
	hasher := sha256.New()
	mw := io.MultiWriter(tmp, hasher)
	wc, err := age.Encrypt(mw, p.rec)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pusher: age.Encrypt: %w", err)
	}
	if _, err := io.Copy(wc, plain); err != nil {
		wc.Close()
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pusher: copy plaintext: %w", err)
	}
	if err := wc.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pusher: close encryptor: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pusher: seek temp: %w", err)
	}
	st, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, "", 0, fmt.Errorf("pusher: stat temp: %w", err)
	}
	return tmp, hex.EncodeToString(hasher.Sum(nil)), st.Size(), nil
}

// buildMultipartBody assembles the multipart/form-data payload
// cloudbox expects. Form fields go before the file part so the
// receiver can validate them without buffering the blob.
func buildMultipartBody(blobFile *os.File, size int64, cipherSHA, host, app string, rec *age.X25519Recipient) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fields := map[string]string{
		"host":    host,
		"app":     app,
		"sha256":  cipherSHA,
		"size":    strconv.FormatInt(size, 10),
		"enc_alg": "age-x25519+chacha20poly1305",
		"key_id":  RecipientFingerprint(rec),
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	// File part. We stream the blob into the buffer — for v1 sizes
	// this is fine; a future move to io.Pipe would let us stream
	// directly through the HTTP request without staging.
	fw, err := mw.CreateFormFile("blob", filepath.Base(blobFile.Name()))
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(fw, blobFile); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}
