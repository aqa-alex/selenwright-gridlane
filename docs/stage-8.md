# Stage 8 Operations, Security, and Release

## Ready

- Added Prometheus-style metrics for HTTP requests, proxy outcomes, Playwright
  WebSocket session events, and backend health state.
- Added `/metrics` on the main listener with admin-token authorization and
  optional `-metrics-listen` support for an internal metrics listener.
- Added HTTP security headers that do not interfere with proxied VNC/devtools
  browser content: `X-Content-Type-Options: nosniff` and
  `Referrer-Policy: no-referrer`.
- Added bounded upstream transport policy: dial, TLS handshake, response header,
  idle connection, max connection, and max response header limits.
- Added Dockerfile, docker-compose integration environment, and GoReleaser
  snapshot/release configuration.
- Added docs for running Gridlane against `/Users/Sasha/dev/selenwright`.

## Contracts

- Metrics labels use route templates such as `/logs/:session` and
  `/playwright/:browser/:version`, not public session IDs.
- Main-listener `/metrics` is an admin endpoint. The separate metrics listener is
  for private/internal exposure.
- Side endpoints remain user-authenticated only: guest access does not apply to
  VNC, devtools, logs, video, downloads, clipboard, or host lookup.
- Default upstream proxy transport uses `MaxConnsPerHost=128` and
  `MaxResponseHeaderBytes=1MiB`.
- Release artifacts are built from `./cmd/gridlane` with `CGO_ENABLED=0`.

## Checks

- `go test ./...`
- `go test -race ./...`
- `go test -tags websocat ./internal/integration -run TestWebsocatSmoke -count=1`
- `go vet ./...`
- `staticcheck ./...`
- `golangci-lint run ./...`
- `govulncheck ./...`
- `goreleaser check`
- `goreleaser release --snapshot --clean --skip=publish`
- `vegeta` load smoke against `/ping`

## Remaining Risks

- The compose environment depends on the sibling Selenwright checkout and its
  local image/build prerequisites.
- Metrics on a separate listener are intentionally unauthenticated and should be
  bound only to loopback, a private network, or protected by infrastructure.
- Load smoke is intentionally lightweight; sustained HTTP and WebSocket load
  scenarios should be expanded once real Selenwright backend capacity limits are
  finalized.
