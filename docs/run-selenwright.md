# Running Gridlane with Selenwright

Expects the Selenwright checkout to sit next to the Gridlane one — e.g. both under `$HOME/dev/selen/` as `selenwright/` and `selenwright-gridlane/`. The exact parent directory is up to you; adjust the `cd` paths below to match your layout.

## Local Compose

Build or refresh the Selenwright local image first if its `dist/` binaries have
changed:

```sh
cd ../selenwright   # or $HOME/dev/selen/selenwright, etc.
goreleaser build --snapshot --clean
```

Start Gridlane with two Selenwright-compatible backend nodes:

```sh
cd -   # back into the gridlane checkout
GRIDLANE_ALICE_PASSWORD=wonderland GRIDLANE_ADMIN_TOKEN=root-token docker compose -f docker-compose.integration.yml up --build
```

Gridlane listens on `127.0.0.1:4444`; metrics listen on `127.0.0.1:9090`.

## Smoke Checks

```sh
curl -fsS http://127.0.0.1:4444/ping
curl -fsS -H 'X-Gridlane-Admin-Token: root-token' http://127.0.0.1:4444/config
curl -fsS -u alice:wonderland http://127.0.0.1:4444/quota
curl -fsS http://127.0.0.1:9090/metrics
```

Run a lightweight load smoke:

```sh
printf 'GET http://127.0.0.1:4444/ping\n' | vegeta attack -duration=5s -rate=20 | vegeta report
```

For a Playwright WebSocket handshake smoke, use a configured Playwright catalog
version and backend image:

```sh
websocat -E -n --basic-auth "$(printf 'alice:wonderland' | base64)" --protocol playwright-json ws://127.0.0.1:4444/playwright/chrome/stable
```

## Security Notes

- `/metrics` on the main Gridlane listener requires `X-Gridlane-Admin-Token`.
- The separate metrics listener is intended for an internal-only bind or private
  network path.
- Side endpoints such as `/vnc/`, `/devtools/`, `/logs/`, `/video/`,
  `/download/`, `/downloads/`, and `/clipboard/` require user BasicAuth and do
  not fall back to guest access.
