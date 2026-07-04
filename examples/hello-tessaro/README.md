# hello-tessaro — reference "custom app on tessaro"

A minimal, dependency-free (stdlib-only) cooperative web app that demonstrates
**everything an app of this shape needs** to run on tessaro (cloudbox + outpost):

- **(a) local-only access** — a LAN-only destructive route, fenced from the web.
- **(b) web/cloud access** — the cooperative-app contract (prefix-aware URLs,
  identity headers, HMAC verification).
- **(e) admin + regular users** — four-tier model (public / user / admin / superadmin).
- **(d) dev/qa/prod** — one binary, per-env config, one tile per env.
- **CI/CD** — `/healthz` + `/version`, build/test, atomic deploy + rollback, and an
  `sdlc.yaml` so the whole loop can run **fully automatically** under `bashy sdlc`.

Copy this directory, delete the demo handlers in `main.go`, and keep the wiring.
The reusable core is `internal/tessaro/` (the contract) + `internal/config/` (env
loading) — those you keep verbatim.

## Two usages, one pipeline

| | Who drives it | How |
|---|---|---|
| **Typical custom app** | humans | file an issue, review the PR, approve the promotion |
| **Fully-automated SDLC** | `bashy sdlc` | trigger → conductor sprint → auto-gate → deploy |

Same code, same pipeline. The difference is a policy (`sdlc.yaml` → `prod_approval`).

## Quick start (local)

```bash
go test ./...                      # hermetic — no network
go run . --version --json
APP_ENV=dev HELLO_TESSARO_SSO_SECRET=dev-secret go run .   # serves 127.0.0.1:8080
```

Open http://127.0.0.1:8080/ — the public page. `/app`, `/admin` require a
cloud-vouched identity (they 403 on direct hits); `/admin/danger` is LAN-only.

## The four-tier surface

| Route | Tier | Gate |
|---|---|---|
| `/` | public | none (`require_login:false`) |
| `/app` | user | `RequireAuth` — any HMAC-verified cloud identity |
| `/admin` | admin | `RequireAdmin` — verified identity ∈ app admin allowlist |
| `/admin/danger` | superadmin | `RequireLocal` — LAN-only; add OS auth for real superadmin |
| `/healthz`, `/version` | infra | CI/CD probes |

**How enforcement is split** (the mental model): cloudbox = transport, outpost =
gate, app = authz.
- outpost fences `/admin/danger` for web callers via `lan_only_paths` **before**
  the app sees it; `RequireLocal` is defense in depth.
- outpost stamps `Remote-User`/`Remote-Groups` + an HMAC signature; the app
  **verifies the signature** (`tessaro.VerifyOutpost`) before trusting identity.
- `Remote-Groups: admin` means "cloud-vouched admin tier", NOT app admin — the
  app maps it onto its own RBAC (`AdminEmails`). Superadmin is never cloud-granted.

## Registering as an outpost app (per environment)

dev/qa/prod are three tiles, each its own `AppConfig` row in `agent.json` (or via
`outpost apps add`). Give each its **own `sso_secret`** so a dev secret can't sign
a prod request:

```jsonc
// agent.json → apps[]  (one row per env; qa/prod differ only in name/port/secret)
{
  "name": "hello-tessaro-dev",
  "scheme": "http",
  "host": "127.0.0.1",
  "port": 8080,
  "enabled": true,
  "require_login": true,            // /app and /admin need a cloud identity
  "trust_cloud_identity": true,     // stamp Remote-* + HMAC
  "sso_secret": "<per-env 32-byte hex>",  // must equal HELLO_TESSARO_SSO_SECRET
  "lan_only_paths": ["/admin/danger"]     // fenced from the web
}
```

Set the same secret in the app's environment: `HELLO_TESSARO_SSO_SECRET=<hex>`.
Then the tile is reachable at `https://ai.dhnt.io/matrix/h/<host>/app/hello-tessaro-dev/`.

## CI/CD

```bash
# build / test gate (same gate a human PR check and the conductor use)
go vet ./... && go test ./...

# deploy one env (atomic swap + health verify; keeps <bin>.previous)
deploy/wrapper.sh --env qa --url https://.../hello-tessaro-linux-amd64 --sha256 <hex>
deploy/wrapper.sh --env dev              # no --url → build from source (local loop)

# rollback (swap .previous back, ~2s)
deploy/wrapper.sh --env prod --rollback
```

The verify contract: the pipeline polls `/version` until the deployed commit
appears (10-min ceiling) and `/healthz` returns 200, else it rolls back. This
mirrors outpost's own self-upgrade worker.

**Container / DKS**: `docker build --build-arg COMMIT=$(git rev-parse --short HEAD)
-t hello-tessaro .` — the *same* image runs bare-metal, in podman, or as a DKS pod
(the deployment-mode contract; only networking/persistence differ).

## Fully-automated SDLC (`bashy sdlc`)

`sdlc.yaml` makes this app operable by the shipped SDLC control plane:

```bash
bashy sdlc doctor                       # check config + agents
bashy sdlc tick --issue "add /status"   # one round: plan → implement → test
bashy sdlc pages once                    # fully-auto single shot (issue → deploy)
```

The conductor (an agent CLI) plans a sprint, delegates coding to the fleet via
`bashy weave`, gates on tests+review, then hands the deploy to the wrapper above.
`policies.prod_approval` = `required` (human approves) or `auto` (deploy on green).
Real GitHub-issue intake + executable deploy adapters land with the umbrella P2
work — see `docs/custom-app-on-tessaro.md`.

## Layout

```
hello-tessaro/
├── main.go                     # the demo app (routes) — replace with yours
├── internal/tessaro/           # KEEP: the cooperative-app contract (prefix, HMAC, gates)
├── internal/config/            # KEEP: 12-factor per-env loader
├── config.{dev,qa,prod}.json   # non-secret per-env defaults (secrets come from env)
├── sdlc.yaml                   # bashy sdlc operating config
├── Dockerfile                  # mode-independent image
├── deploy/wrapper.sh           # bare-metal atomic deploy + rollback
├── README.md
└── ONBOARDING.md               # add/remove admin & regular users
```

## Further reading (umbrella docs)

- `outpost/docs/cooperative-web-apps.md` — the wire contract (source of truth).
- `docs/cooperative-web-apps-best-practices.md` — patterns, the six-item preflight.
- `docs/custom-app-on-tessaro.md` — the end-to-end guide (both usages + env model).
- `bashy/docs/local-loom-sdlc-control-plane.md` — the automated SDLC loop.
