// Package app wires the gridlane CLI, runtime, reloading HTTP handler and
// middleware chain. The package is intentionally split across concern-
// specific files (options, runtime, reload, server, mux, middleware) so
// each responsibility lives in isolation.
package app

// serviceName is the "gridlane" label embedded in public payloads and
// degraded-status fallbacks. Kept here as the package-level root so every
// file can reference it without alias gymnastics.
const serviceName = "gridlane"
