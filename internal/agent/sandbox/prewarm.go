package sandbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

// libpodPrefix is the versioned libpod REST base. podman 5.x serves both
// this and back-compat aliases; the image endpoints below are stable
// across the 4.x/5.x line.
const libpodPrefix = "/v5.0.0/libpod"

// Prewarmer keeps a configured set of container images pulled on the local
// podman daemon so a remote sandbox create+start doesn't pay the
// image-pull cost — the dominant cold-start latency for a thin client
// running code on this node. It re-checks on a timer so an image that
// podman garbage-collects gets re-pulled.
//
// This is the Phase-A "warm" optimization: a transparent pre-warmed
// CONTAINER pool can't work at the raw-docker layer (the caller picks the
// image / cmd / env at create time), but pre-pulling the images the
// caller is actually allowed to run is both correct and the bulk of the
// speedup. The right image set is the operator's allowlist (or an
// explicit prewarm list).
type Prewarmer struct {
	client   *http.Client
	base     string // scheme://host the client dials ("http://podman" for unix)
	images   []string
	interval time.Duration
	pullTO   time.Duration
	ready    atomic.Int64
}

// NewPrewarmer builds a Prewarmer that talks to the libpod socket at
// `socket` and keeps `images` warm. The HTTP client dials the unix socket
// regardless of request host (the synthetic "podman" host is ignored by
// the daemon).
func NewPrewarmer(socket string, images []string) *Prewarmer {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		IdleConnTimeout: 90 * time.Second,
	}
	return newPrewarmer(&http.Client{Transport: tr}, "http://podman", images)
}

// newPrewarmer is the shared constructor; NewPrewarmer wires the unix
// transport, tests inject an httptest client + base URL.
func newPrewarmer(client *http.Client, base string, images []string) *Prewarmer {
	return &Prewarmer{
		client:   client,
		base:     base,
		images:   dedupeNonEmpty(images),
		interval: 30 * time.Minute,
		pullTO:   10 * time.Minute,
	}
}

// Ready returns how many configured images are confirmed present locally.
func (p *Prewarmer) Ready() int { return int(p.ready.Load()) }

// Total returns how many images the prewarmer is responsible for.
func (p *Prewarmer) Total() int { return len(p.images) }

// Run pulls missing images once on start, then reconciles on each tick
// until ctx is cancelled. Returns ctx.Err() on shutdown. A nil/empty
// image list makes Run block on ctx (nothing to do) so callers can wire
// it under the errgroup unconditionally.
func (p *Prewarmer) Run(ctx context.Context) error {
	if len(p.images) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	p.reconcile(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			p.reconcile(ctx)
		}
	}
}

// reconcile pulls each missing image, then recounts how many are present.
// A pull that fails (image typo, registry down) just leaves that image
// uncounted — the next tick retries. Readiness is published only after
// the whole pass so the capacity report never flaps mid-reconcile.
func (p *Prewarmer) reconcile(ctx context.Context) {
	for _, img := range p.images {
		if ctx.Err() != nil {
			return
		}
		if !p.exists(ctx, img) {
			_ = p.pull(ctx, img)
		}
	}
	var ready int64
	for _, img := range p.images {
		if p.exists(ctx, img) {
			ready++
		}
	}
	p.ready.Store(ready)
}

// exists reports whether the image is present locally. libpod answers
// GET .../images/<name>/exists with 204 (present) or 404 (absent). The
// image ref carries unescaped slashes (docker.io/library/python:3.12) —
// podman's route matches the remainder of the path, so we insert it raw.
func (p *Prewarmer) exists(ctx context.Context, image string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		p.base+libpodPrefix+"/images/"+image+"/exists", nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer drainClose(resp.Body)
	return resp.StatusCode == http.StatusNoContent
}

// pull pulls the image. libpod streams pull progress as the response
// body; draining it to EOF is what blocks until the pull finishes. A
// non-200 status (or a body-level error) leaves the image absent — the
// caller re-checks exists() after, so a failed pull simply doesn't count
// toward readiness.
func (p *Prewarmer) pull(ctx context.Context, image string) error {
	pctx, cancel := context.WithTimeout(ctx, p.pullTO)
	defer cancel()
	q := url.Values{"reference": {image}}
	req, err := http.NewRequestWithContext(pctx, http.MethodPost,
		p.base+libpodPrefix+"/images/pull?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer drainClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandbox prewarm: pull %s: status %d", image, resp.StatusCode)
	}
	return nil
}

func drainClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}

func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
