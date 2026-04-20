# Gridlane

Router and load-balancer for the [Selenwright](https://github.com/aqa-alex/selenwright) browser-automation grid. Sits in front of N Selenwright backends and multiplexes Selenium WebDriver (HTTP), Playwright (WebSocket), and the side surfaces (VNC, logs, video, downloads, devtools, clipboard, artifact history) behind a single authenticated endpoint.

- **Dual protocol** — WebDriver and Playwright on the same listener, routed by session ID
- **Stateless across replicas** — public session IDs are `r1_<route-token>_<upstream-id>`, so any Gridlane instance can follow up a session created through any other
- **Weighted, region-aware pool** — per-backend `weight` and `region`, protocol-aware selection (a Playwright request only lands on a Playwright-capable pool)
- **Passive health with cooldown** — a backend goes out of rotation after `failure_threshold` consecutive failures, comes back after `cooldown`
- **Per-user quotas** — BasicAuth users + optional guest scope, each with `max_sessions`
- **Fail-closed reload** — SIGHUP reloads `router.json`; invalid config keeps the prior runtime serving
- **Trusted-proxy identity** — propagates the resolved client identity to Selenwright via `X-Forwarded-User` / `X-Admin`, with a shared router secret so upstreams reject anyone bypassing Gridlane
- **Stdlib-only, ~10 MB static binary** — `net/http`, `net/http/httputil`, `log/slog`, `crypto/hmac`; no external Go dependencies

## Quick start

1. Create a `router.json` with one user, one backend, and a guest quota (see [`examples/router.compose.json`](examples/router.compose.json) for the full shape):

```json
{
  "version": 1,
  "users": [
    {
      "name": "alice",
      "password_ref": "env:GRIDLANE_ALICE_PASSWORD",
      "quota": { "max_sessions": 20 }
    }
  ],
  "guest": { "quota": { "max_sessions": 2 } },
  "catalog": {
    "browsers": [
      {
        "name": "chrome",
        "versions": ["stable"],
        "protocols": ["webdriver", "playwright"]
      }
    ]
  },
  "backend_pools": [
    {
      "id": "selenwright-a",
      "endpoint": "http://selenwright-a:4444",
      "region": "local-a",
      "weight": 1,
      "protocols": ["webdriver", "playwright"],
      "health": { "enabled": true, "failure_threshold": 2, "cooldown": "10s" }
    }
  ],
  "admin": { "token_ref": "env:GRIDLANE_ADMIN_TOKEN" }
}
```

2. Run Gridlane:

```bash
docker run -d --name gridlane                                 \
    -p 4444:4444 -p 9090:9090                                 \
    -v $(pwd)/router.json:/etc/gridlane/router.json:ro        \
    -e GRIDLANE_ALICE_PASSWORD=wonderland                     \
    -e GRIDLANE_ADMIN_TOKEN=root-token                        \
    aqa-alex/gridlane:latest-release                          \
    -config /etc/gridlane/router.json                         \
    -metrics-listen :9090
```

The image runs as UID/GID `65532:65532` and exposes `4444` (main listener) and `9090` (metrics listener, when `-metrics-listen` is set).

3. Point your tests at the Gridlane endpoint — same URL shape as Selenwright:

```
http://localhost:4444/wd/hub                                # Selenium WebDriver
ws://localhost:4444/playwright/<browser>/<version>          # Playwright
```

4. Smoke-check the listener:

```bash
curl -fsS http://127.0.0.1:4444/ping
curl -fsS -u alice:wonderland http://127.0.0.1:4444/quota
curl -fsS -H 'X-Gridlane-Admin-Token: root-token' http://127.0.0.1:4444/config
```

### Running From Source

If you build Gridlane from source or pull a prebuilt binary from [releases](https://github.com/aqa-alex/selenwright-gridlane/releases/latest) instead of running the image:

```bash
go build ./cmd/gridlane
GRIDLANE_ALICE_PASSWORD=wonderland \
GRIDLANE_ADMIN_TOKEN=root-token \
./gridlane -config router.json
```

Flags and config file are identical — the image's `ENTRYPOINT` is the binary with no wrapper.

## Configuration (`router.json`)

Parsed with `DisallowUnknownFields` — typos are rejected, not silently ignored. Secret references must be `env:NAME` or `file:/absolute/path` — plaintext secrets in the config file are rejected.

| Key | Shape | Notes |
|---|---|---|
| `version` | int | Must be `1` |
| `users[]` | `{name, password_ref, quota}` | BasicAuth users; `password_ref` is `env:` / `file:` |
| `guest` | `{quota}` | Optional; enables anonymous `ScopeUser` access with its own quota |
| `catalog.browsers[]` | `{name, versions[], platforms[], protocols[]}` | Advertised via `/quota` and used to validate Playwright path protocol |
| `backend_pools[]` | `{id, endpoint, region, weight, protocols[], credentials?, health?}` | `endpoint` must be `http://` or `https://`; no embedded credentials |
| `backend_pools[*].credentials` | `{username_ref, password_ref}` | Injected as upstream BasicAuth when the backend enforces its own auth |
| `backend_pools[*].health` | `{enabled, failure_threshold, cooldown}` | `cooldown` accepts Go duration strings (`"30s"`, `"2m"`); defaults `1` / `30s` |
| `admin.token_ref` | `env:` / `file:` | When set, `/config` and `/metrics` accept `X-Gridlane-Admin-Token` or `Authorization: Bearer …` |
| `upstream_identity` | `{user_header, admin_header?, secret_ref?}` | Trusted-proxy propagation to Selenwright (see below) |

See [Router Configuration](https://aqa-alex.github.io/selenwright-gridlane/latest/#_router_configuration) for the full schema and validation rules.

## HTTP surface

| Path | Scope | Notes |
|---|---|---|
| `/ping` | public | Liveness — `{"service":"gridlane","status":"ok"}` |
| `/status` | public | Backend rollup (healthy / total) |
| `/quota` | user | Caller's own quota plus the guest quota (if enabled) |
| `/config` | admin | Sanitized `router.json` (no secrets, no resolved passwords) |
| `/metrics` | admin on main listener / public on `-metrics-listen` | Prometheus text format |
| `/wd/hub/session`, `/session`, `/session/...` | user | WebDriver; session create routes by catalog + weight, follow-ups route by the `route-token` embedded in the session ID |
| `/playwright/<browser>/<version>` | user | Playwright WebSocket upgrade |
| `/host/`, `/vnc/`, `/devtools/`, `/video/`, `/logs/`, `/download/`, `/downloads/`, `/clipboard/`, `/history/settings` | side | Upstream side endpoints (BasicAuth only — no guest fallback) |

See [HTTP API](https://aqa-alex.github.io/selenwright-gridlane/latest/#_http_api) for the per-endpoint reference.

## Authentication

Gridlane has a four-rung scope ladder — `admin` > `user` > `side` > `public`:

- **`public`** — no credentials required (`/ping`, `/status`).
- **`user`** — HTTP BasicAuth matching a `users[]` entry, **or** anonymous guest access if `guest` is configured. Session create, Playwright upgrade, `/quota`.
- **`side`** — HTTP BasicAuth matching a `users[]` entry. **Does not** fall back to guest. VNC, logs, video, devtools, clipboard, downloads, artifact history settings.
- **`admin`** — `X-Gridlane-Admin-Token: <token>` header or `Authorization: Bearer <token>`, matched against `admin.token_ref`. `/config`, `/metrics` (on the main listener).

Compared in constant time. Admin token and user passwords are resolved from `env:` / `file:` refs once at startup and on reload — Gridlane never reads secrets from the config JSON itself.

See [Authentication](https://aqa-alex.github.io/selenwright-gridlane/latest/#_authentication) for the full model and routing examples.

## Session routing

Public session IDs are:

```
r1_<route-token>_<upstream-id>
```

- `<route-token>` is a 16-char hex prefix of `HMAC-SHA256(backend_pool.id)` — it picks the backend pool for any follow-up request without Gridlane keeping session state.
- `<upstream-id>` is whatever the backend returned for WebDriver, or a Gridlane-minted `pw_<32-hex>` for Playwright (propagated to Selenwright via `X-Selenwright-External-Session-ID` on the upgrade).

This makes Gridlane **stateless across replicas** — any replica serving the same `router.json` can route a follow-up for a session the other created. Side endpoints use the public session ID as-is against upstream (Selenwright stores Playwright sessions under the public ID), so `/vnc/r1_.../` lands on the right container.

See [Session ID Format](https://aqa-alex.github.io/selenwright-gridlane/latest/#_session_id_format) and [Playwright Routing](https://aqa-alex.github.io/selenwright-gridlane/latest/#_playwright_routing).

## Identity propagation to Selenwright

When `upstream_identity` is set in `router.json`, Gridlane stamps the resolved client identity on every upstream request:

```json
"upstream_identity": {
  "user_header":  "X-Forwarded-User",
  "admin_header": "X-Admin",
  "secret_ref":   "env:GRIDLANE_ROUTER_SECRET"
}
```

- `X-Forwarded-User: <subject>` — the resolved username, or literal `guest`.
- `X-Admin: true` — only when the request authorized as Gridlane admin.
- `X-Router-Secret: <shared secret>` — proves the headers came from Gridlane, not a direct client.

Any spoofed values on these headers in the incoming request are stripped by a middleware **before** auth runs, so a client cannot set `X-Forwarded-User: alice` themselves.

The matching Selenwright invocation:

```bash
selenwright \
  -auth-mode=trusted-proxy \
  -user-header=X-Forwarded-User \
  -admin-header=X-Admin \
  -trusted-proxy-secret=$GRIDLANE_ROUTER_SECRET
```

Without this block Selenwright sees every session as coming from the pool-level BasicAuth account, and per-user quotas / session ACL / admin bypass collapse.

See [Identity Propagation](https://aqa-alex.github.io/selenwright-gridlane/latest/#_identity_propagation).

## Reload and health

`SIGHUP` reloads the config file. Reload is **fail-closed** — if the new config is invalid (schema violation, unresolvable secret, bad endpoint), the previous runtime keeps serving and the error is logged. You never serve a half-loaded config. Disable with `-reload-on-sighup=false`.

Health is **passive** — Gridlane does not probe backends on its own. A pool is marked unhealthy after `failure_threshold` consecutive failed proxy attempts (5xx, 408/425/429, 401/403 from upstream), stays out of rotation for `cooldown`, then returns. `/status` reports the live roll-up.

See [Reload](https://aqa-alex.github.io/selenwright-gridlane/latest/#_reload_sighup) and [Health](https://aqa-alex.github.io/selenwright-gridlane/latest/#_backend_health).

## Observability

- **Prometheus metrics** on `/metrics` — `gridlane_http_requests_total`, `gridlane_http_request_duration_seconds`, `gridlane_proxy_requests_total`, `gridlane_proxy_request_duration_seconds`, `gridlane_websocket_sessions_total`, `gridlane_backend_available`, `gridlane_backend_failures_total`. Route labels use `:session` placeholders, not literal IDs, so cardinality stays bounded.
- **Structured logs** — `-log-format=json` emits one-line JSON via `log/slog`.
- **Status rollup** — `/status` returns backend count and available count for external health-check probes.

See [Observability](https://aqa-alex.github.io/selenwright-gridlane/latest/#_observability).

## CLI flags

<details>
<summary>Full reference</summary>

| Flag | Default | Description |
|---|---|---|
| `-listen` | `:4444` | Main HTTP listener (WebDriver + Playwright + side endpoints) |
| `-config` | `router.json` | Path to router.json v1 |
| `-metrics-listen` | *(empty)* | Optional separate Prometheus listener, e.g. `:9090` (no auth). Leave empty to serve `/metrics` on the main listener with admin token |
| `-graceful-period` | `15s` | Shutdown drain window |
| `-session-attempt-timeout` | `30s` | Upstream session-create timeout |
| `-proxy-timeout` | `5m` | Per-request proxy timeout |
| `-reload-on-sighup` | `true` | Reload config on SIGHUP; invalid config keeps the prior runtime |
| `-log-format` | `text` | `text` or `json` |
| `-version` | — | Print version and exit |

</details>

Full reference: [CLI Flags](https://aqa-alex.github.io/selenwright-gridlane/latest/#_cli_flags).

## Build and test

```bash
go build ./cmd/gridlane                          # local binary
go test ./...                                    # unit + in-process integration
go test -race ./...                              # race detector
goreleaser build --snapshot --clean              # cross-compile (linux/darwin × amd64/arm64)
docker build -t gridlane:local .                 # multi-stage Alpine image
```

Go 1.26, module path is literally `gridlane` (stdlib-only — no `require` in `go.mod`).

## Documentation

Published HTML reference: **https://aqa-alex.github.io/selenwright-gridlane/** (generated per release from [docs/](docs/), AsciiDoc sources).

## License

Apache 2.0 — see [LICENSE](LICENSE).
