// Command hello-tessaro is the reference "custom app on tessaro": a minimal
// cooperative web app that demonstrates every piece of the contract an app of
// this shape needs — local-only routes, web/cloud routes, admin vs regular
// users, per-env config, and the CI/CD verify endpoints (/healthz, /version,
// --version --json). Copy it, delete the demo handlers, keep the wiring.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/qiangli/outpost/examples/hello-tessaro/internal/config"
	"github.com/qiangli/outpost/examples/hello-tessaro/internal/tessaro"
)

// Build metadata, injected via -ldflags "-X main.Version=... -X main.Commit=...".
// /version and `--version --json` report Commit — the CI/CD pipeline polls it to
// confirm a deploy actually landed the expected git SHA.
var (
	Version = "dev"
	Commit  = "unknown"
)

func main() {
	var showVersion bool
	var asJSON bool
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.BoolVar(&asJSON, "json", false, "with --version, print JSON")
	flag.Parse()

	if showVersion {
		printVersion(asJSON)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(".")
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	if cfg.SSOSecret == "" && cfg.RequireHMAC {
		// Fail loud: require_hmac with no secret would reject every cloud caller.
		log.Warn("require_hmac is on but SSO secret is empty; cloud requests will be rejected until HELLO_TESSARO_SSO_SECRET is set")
	}

	guard := &tessaro.Guard{
		Secret:      []byte(cfg.SSOSecret),
		RequireHMAC: cfg.RequireHMAC,
		AdminEmails: cfg.AdminSet(),
		Log:         log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthz)
	mux.HandleFunc("/version", version)
	mux.HandleFunc("/", index(cfg))                               // public
	mux.Handle("/app", guard.RequireAuth(userPage()))             // regular users
	mux.Handle("/admin", guard.RequireAdmin(adminPage()))         // admins
	mux.Handle("/admin/danger", guard.RequireLocal(dangerPage())) // LAN-only (fenced by outpost lan_only_paths + RequireLocal)
	mux.HandleFunc("/api/link", apiLink)                          // JSON-body prefixing demo

	log.Info("hello-tessaro listening", "env", cfg.Env, "addr", cfg.Addr, "commit", Commit)
	srv := &http.Server{Addr: cfg.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func printVersion(asJSON bool) {
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{
			"version": Version, "commit": Commit,
		})
		return
	}
	fmt.Printf("hello-tessaro %s (%s)\n", Version, Commit)
}

// healthz is the CI/CD liveness probe: 200 when serving, small JSON body.
func healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// version reports the deployed commit. The deploy pipeline polls this until the
// new SHA appears (else it rolls back).
func version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": Version, "commit": Commit})
}

// index is public (require_login:false). It shows the resolved <base href> and
// the caller's cloud identity if any, and links to the gated surfaces.
func index(cfg config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// The public route has no auth middleware, so read the raw (unverified)
		// stamp just to show what outpost saw — never trust it for a decision.
		who := "anonymous"
		if raw := tessaro.IdentityFrom(r); raw.User != "" {
			who = raw.User + " (" + raw.Groups + ")"
		}
		page(w, r, "hello-tessaro", fmt.Sprintf(`
<p>Environment: <b>%s</b> · commit <code>%s</code></p>
<p>Seen by outpost as: <b>%s</b> · arrived via cloud: <b>%v</b></p>
<ul>
  <li><a href="app">/app</a> — any signed-in user</li>
  <li><a href="admin">/admin</a> — admins only</li>
  <li><a href="admin/danger">/admin/danger</a> — LAN-only (404 over the web)</li>
  <li><a href="api/link">/api/link</a> — JSON body with a proxy-safe path</li>
</ul>`, html.EscapeString(cfg.Env), html.EscapeString(Commit), html.EscapeString(who), tessaro.ArrivedViaCloud(r)))
	}
}

func userPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := tessaro.IdentityOf(r)
		page(w, r, "your area", fmt.Sprintf(`<p>Signed in as <b>%s</b>. This route requires a verified cloud identity.</p>`,
			html.EscapeString(id.User)))
	})
}

func adminPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := tessaro.IdentityOf(r)
		page(w, r, "admin", fmt.Sprintf(`<p>Admin surface. You are <b>%s</b>.</p>
<p>Enforced by the app's admin allowlist (RBAC), not by cloudbox.</p>`, html.EscapeString(id.User)))
	})
}

// dangerPage is a destructive operation, reached only via RequireLocal (LAN).
// A real app would additionally require OS auth here (superadmin) before acting.
func dangerPage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page(w, r, "danger zone", `<p>LAN-only destructive action. Reachable only on the local network.</p>`)
	})
}

// apiLink demonstrates the most-missed rule: outpost rewrites Location headers
// but NOT JSON bodies, so any absolute path in a JSON response must be prefixed
// server-side with PrefixPath.
func apiLink(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"self":   tessaro.PrefixPath(r, "/api/link"),
		"app":    tessaro.PrefixPath(r, "/app"),
		"origin": tessaro.ExternalBase(r),
	})
}

// page renders a minimal HTML document with the load-bearing <base href> so all
// relative links resolve under the app's mount prefix.
func page(w http.ResponseWriter, r *http.Request, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8">
<base href="%s"><title>%s</title></head><body>
<h1>%s</h1>%s
<hr><p><a href="">home</a></p></body></html>`,
		html.EscapeString(tessaro.BaseHref(r)), html.EscapeString(title), html.EscapeString(title), body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
