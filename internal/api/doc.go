// Package api is the authenticated JSON control plane mounted at /api.
// Handlers live in subpackages; every route is declared in one place
// (router.go) so the auth layering stays visible at a glance:
// public → Authenticate (session or API key) → RequireOrg (traffic plane).
//
// Layout:
//
//	router.go   Deps struct + Mount — the single route table
//	httpx/      shared helpers: JSON responses, nullable-column diffing
//	identity/   auth, orgs, members, invites, api_keys, audit
//	pipeline/   sources, destinations, connections, events, methods
//	cli/        CLI source lookups + WebSocket tunnel
//
// Wiring: cmd/dstream builds one Deps and calls Mount, which fans it out to
// each subpackage's Handlers struct (only the fields that group needs).
//
// Tests: the *_test.go files here exercise the full router over HTTP via
// Mount, so they stay in this package; pure unit tests live beside their
// subject inside the subpackages.
//
// Adding an endpoint: add an exported method on the right subpackage's
// Handlers, register it in Mount, cover it with a test.
package api
