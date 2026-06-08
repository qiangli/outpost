# cooperative-app fixture

Tiny Python stub that emulates a cooperative web app for end-to-end
testing of the matrix-tunnel proxy + HMAC identity contract.

Three routes:

| Path              | Purpose |
|-------------------|---------|
| `GET /`           | Landing page (HTML, 200) |
| `GET /lan-only/*` | Kiosk endpoint — outpost's `lan_only_paths` gate should 404 this when the request arrives through the cloud tunnel |
| `GET /echo-headers` | Returns a JSON dump of the request headers — used by E2E tests to assert cloudbox-stamped identity headers reach the upstream verbatim |

## Bare-process run (E2E-3)

```bash
python3 server.py            # listens on :58080
PORT=58081 python3 server.py # override port
```

## Podman container (E2E-4)

```bash
podman build -t cooperative-app .
podman run --rm -p 58080:58080 cooperative-app
```

The container exposes the same routes; the only difference is the
upstream's runtime is podman-managed instead of a bare process. Used
by `e2e-share-podman` to validate that the cooperative-app contract
(URL prefix, identity headers, lan_only_paths gate) round-trips
identically across both runtimes.

## Why this lives in `outpost/test/fixtures/`

The fixture is referenced by E2E scripts in the cloudbox repo
(proprietary) but is itself language-neutral and OSS-safe — no
proprietary app names, no cloudbox source-path references. Keeping it
here lets the OSS test surface stay consistent across repos without
the cloudbox driver needing to reach into a separate fixture repo.
