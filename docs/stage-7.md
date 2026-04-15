# Stage 7 Integration and Mixed-Protocol Validation

## Ready

- Added reloadable runtime wiring for `router.json` so SIGHUP can replace the
  active handler without losing the previous runtime on invalid config.
- Added mixed-protocol integration coverage with two fake Selenwright-compatible
  backends: one WebDriver route and one Playwright WebSocket route through one
  Gridlane instance.
- Added stateless cross-router routing coverage: a session created through one
  Gridlane instance can be followed up through another instance using the same
  config.
- Added reload coverage for invalid reload rollback, removed backend route
  rejection, and changed endpoint/region/weight application after reload.
- Added an optional `websocat` tagged smoke test for the Playwright WebSocket
  upgrade path.

## Contracts

- Public session IDs remain `r1_<route-token>_<upstream-or-external-session-id>`.
- WebDriver follow-up requests route by `route-token` and strip the public route
  prefix before forwarding upstream.
- Playwright side endpoints preserve the public external session ID when
  forwarding to Selenwright-compatible backends.
- `/status`, `/quota`, `/config`, `/host/<session-id>`, and side endpoints are
  covered against fake mixed-protocol backends.
- Reload is fail-closed for invalid config: the prior runtime keeps serving until
  a valid replacement config is loaded.

## Checks

- `go test ./...`
- `go test -race ./...`
- `go test -tags websocat ./internal/integration -run TestWebsocatSmoke -count=1`

## Remaining Risks

- The stage uses in-process fake Selenwright backends, not the real
  `/Users/Sasha/dev/selenwright` service.
- SIGHUP is wired through the process runtime; the integration test exercises the
  same reload path directly rather than sending an OS signal.
- WebSocket smoke validates upgrade/header routing through `websocat`; deeper
  Playwright protocol message semantics remain a later real-backend concern.
