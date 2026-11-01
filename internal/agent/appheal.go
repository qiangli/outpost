package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// ReadHealRecords reads the append-only ledger in write order.
func ReadHealRecords(path string) ([]HealRecord, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return []HealRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows := []HealRecord{}
	dec := json.NewDecoder(f)
	for {
		var row HealRecord
		if err := dec.Decode(&row); err != nil {
			if errors.Is(err, io.EOF) {
				return rows, nil
			}
			return nil, err
		}
		rows = append(rows, row)
	}
}

// HealRecord is one durable, reversible runtime or configuration action.
type HealRecord struct {
	ID       string    `json:"id"`
	App      string    `json:"app"`
	Incident string    `json:"incident"`
	Rung     string    `json:"rung"`
	Before   any       `json:"before,omitempty"`
	After    any       `json:"after,omitempty"`
	Reason   string    `json:"reason"`
	Result   string    `json:"result"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind,omitempty"`
	Evidence any       `json:"evidence,omitempty"`
}

type HealStatus struct {
	Incident string `json:"incident"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Reason   string `json:"reason,omitempty"`
}

// DeclaredService returns the already declared unit for an app, its state,
// and a start operation owned by the service monitor.
type DeclaredService func(context.Context, string) (string, string, func(context.Context) error)

type AppHealer struct {
	mu      sync.Mutex
	status  map[string]HealStatus
	path    string
	load    func(string) (conf.AppConfig, error)
	upsert  func(conf.AppConfig) error
	service DeclaredService
}

func (r *AppRegistry) ConfigureHealing(path string, load func(string) (conf.AppConfig, error), upsert func(conf.AppConfig) error, service DeclaredService) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healer = &AppHealer{status: make(map[string]HealStatus), path: path, load: load, upsert: upsert, service: service}
}

func (h *AppHealer) Status(app string) HealStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.status[app]
	if !ok {
		return HealStatus{State: "failed", Reason: "no incident recorded"}
	}
	return st
}

var healDigits = regexp.MustCompile(`[0-9]+`)
var healSecret = regexp.MustCompile(`(?i)(token|password|secret|authorization)[=: ]+[^\s,;]+`)
var healEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
var healURL = regexp.MustCompile(`https?://[^\s]+`)
var healIP = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
var healDialAddress = regexp.MustCompile(`(?i)(tcp|udp)\s+[^\s:]+(?::[^\s:]+)?:[0-9]+`)
var healSocketAddress = regexp.MustCompile(`(?i)(unix|npipe)\s+[^\s:]+`)

func incident(app string, err error) string {
	kind := redactHealError(err.Error())
	var op *net.OpError
	if errors.As(err, &op) {
		if op.Timeout() {
			kind = "timeout"
		}
	}
	return app + ":" + strings.ReplaceAll(healDigits.ReplaceAllString(kind, "#"), " ", "-")
}

func newHealID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (h *AppHealer) ProxyError(w http.ResponseWriter, req *http.Request, app string, target *url.URL, dialErr error) {
	sig := incident(app, dialErr)
	h.mu.Lock()
	st := h.status[app]
	if st.State == "repairing" && st.Attempts > 0 {
		sig = st.Incident
	} else if st.Incident != sig || st.State == "healed" {
		st = HealStatus{Incident: sig, State: "repairing"}
	}
	if st.State == "repairing" && st.Attempts == 0 {
		st.Attempts = 1
		h.status[app] = st
		go h.diagnose(app, sig, target, dialErr)
	} else {
		h.status[app] = st
	}
	h.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	if req.Method == http.MethodGet && strings.Contains(req.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		// The status URL is relative to the current app mount, including cloud prefixes.
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Fixing app</title><p id="state">Oops, I saw that error and am fixing it, please wait…</p><script>
const base=location.pathname.split("/.outpost/")[0].replace(/\/$/,"");
const mount=location.pathname.match(/^(.*\/app\/[^/]+)\//);
const statusURL=(mount?mount[1]:base)+"/.outpost/heal";
setInterval(async()=>{try{const r=await fetch(statusURL,{cache:"no-store"});const s=await r.json();if(s.state==="healed")location.reload();if(s.state==="failed"||s.state==="reported")document.getElementById("state").textContent=s.reason||"Repair could not complete."; }catch(e){}},2000);
</script>`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "upstream unavailable", "incident": sig, "state": st.State})
}

func dial(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 350*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func (h *AppHealer) diagnose(app, sig string, target *url.URL, dialErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	evidence := map[string]string{"error": redactHealErrorString(dialErr.Error()), "target": redactHealTarget(target)}
	host, port, splitErr := net.SplitHostPort(target.Host)
	ip := net.ParseIP(host)
	if splitErr == nil && host != "localhost" && (ip == nil || !ip.IsLoopback()) &&
		!dial(target.Host) && dial(net.JoinHostPort("127.0.0.1", port)) && h.load != nil && h.upsert != nil {
		before, err := h.load(app)
		if err == nil && before.Host == host {
			after := before
			after.Host = "127.0.0.1"
			err = h.upsert(after)
			result := "healed"
			if err != nil {
				result = "failed"
			}
			h.record(HealRecord{ID: newHealID(), App: app, Incident: sig, Rung: "address", Before: before, After: after, Reason: "loopback listener answered while the configured address did not", Result: result, Time: time.Now().UTC(), Evidence: evidence})
			reason := ""
			if err != nil {
				reason = "app configuration update failed"
			}
			h.finish(app, sig, result, reason, redactHealTarget(target))
			return
		}
	}
	if h.service != nil {
		unit, state, start := h.service(ctx, app)
		if unit != "" && (state == "stopped" || state == "failed" || state == "inactive") && start != nil {
			err := start(ctx)
			if err == nil {
				for !dial(target.Host) && ctx.Err() == nil {
					select {
					case <-ctx.Done():
					case <-time.After(200 * time.Millisecond):
					}
				}
				if ctx.Err() != nil {
					err = fmt.Errorf("declared service did not listen before timeout")
				}
			}
			result := "healed"
			if err != nil {
				result = "failed"
			}
			h.record(HealRecord{ID: newHealID(), App: app, Incident: sig, Rung: "service", Before: state, After: result, Reason: "started declared service " + unit, Result: result, Time: time.Now().UTC(), Evidence: evidence})
			reason := ""
			if err != nil {
				reason = "declared service did not start or listen"
			}
			h.finish(app, sig, result, reason, redactHealTarget(target))
			return
		}
	}
	reason := "no service for this app is declared or listening"
	h.record(HealRecord{ID: newHealID(), App: app, Incident: sig, Rung: "diagnosis", Reason: reason, Result: "failed", Time: time.Now().UTC(), Evidence: evidence})
	h.finish(app, sig, "failed", reason, redactHealTarget(target))
}

func redactHealError(s string) string {
	// Upstream errors can include URL userinfo or query values; retain only the class.
	if strings.Contains(strings.ToLower(s), "refused") {
		return "connection refused"
	}
	if strings.Contains(strings.ToLower(s), "timeout") || strings.Contains(strings.ToLower(s), "deadline") {
		return "timeout"
	}
	if strings.Contains(strings.ToLower(s), "unreachable") {
		return "unreachable"
	}
	return "dial failed"
}

func redactHealErrorString(s string) string {
	s = healSecret.ReplaceAllString(s, "[redacted]")
	s = healURL.ReplaceAllString(s, "[redacted URL]")
	s = healEmail.ReplaceAllString(s, "[redacted email]")
	s = healDialAddress.ReplaceAllString(s, "$1 [redacted address]")
	s = healSocketAddress.ReplaceAllString(s, "$1 [redacted address]")
	s = healIP.ReplaceAllString(s, "[redacted IP]")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}
func redactHealTarget(u *url.URL) string {
	if u == nil {
		return ""
	}
	if host, port, err := net.SplitHostPort(u.Host); err == nil {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return u.Scheme + "://" + net.JoinHostPort(host, port)
		}
		return u.Scheme + "://<redacted>:" + port
	}
	return u.Scheme + "://<redacted>"
}
func (h *AppHealer) finish(app, sig, state, reason, where string) {
	h.mu.Lock()
	st := h.status[app]
	if st.Incident == sig {
		st.State = state
		st.Reason = reason
		h.status[app] = st
	}
	h.mu.Unlock()
	if state == "failed" || state == "reported" {
		slog.Warn("app heal: owner notice", "app", app, "state", state, "reason", reason, "upstream", where)
	}
}

func (h *AppHealer) record(row HealRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(h.path), 0700)
	f, err := os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		slog.Warn("app heal ledger append failed", "err", err)
		return
	}
	defer f.Close()
	if err := restrictSecretFile(h.path); err != nil {
		slog.Warn("app heal ledger permissions failed", "err", err)
		return
	}
	_ = json.NewEncoder(f).Encode(row)
}

func (h *AppHealer) ReportCode(app string, target *url.URL, resp *http.Response) {
	// Read a bounded prefix and put it back so the caller receives the original body.
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), resp.Body), resp.Body}
	tail := append([]byte(nil), b...)
	if len(tail) > 256 {
		tail = tail[len(tail)-256:]
	}
	// Only record printable bytes; avoid disclosing application content.
	for i, c := range tail {
		if c < 32 || c > 126 {
			tail[i] = ' '
		}
	}
	bodyTail := healIP.ReplaceAllString(healURL.ReplaceAllString(healEmail.ReplaceAllString(healSecret.ReplaceAllString(string(tail), "[redacted]"), "[redacted]"), "[redacted]"), "[redacted]")
	sig := app + ":http-5xx"
	h.mu.Lock()
	st := h.status[app]
	if st.Incident == sig && st.State == "reported" {
		h.mu.Unlock()
		return
	}
	h.status[app] = HealStatus{Incident: sig, State: "reported", Reason: "upstream answered with a server error"}
	h.mu.Unlock()
	h.record(HealRecord{ID: newHealID(), App: app, Incident: sig, Rung: "code", Kind: "code", Reason: "upstream answered with a server error", Result: "reported", Time: time.Now().UTC(), Evidence: map[string]any{"status": resp.StatusCode, "body_tail": bodyTail, "target": redactHealTarget(target)}})
	h.finish(app, sig, "reported", "upstream answered with a server error", redactHealTarget(target))
}
