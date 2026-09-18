This is the complete route table of the Core HTTP API, generated from
`core/routes.go`, the `RequireCap` call on each route, the authorization maps in
`core/authz/coverage.go`, and each handler's doc comment.

**Do not edit below the preamble.** To change a route's description, write the
handler's doc comment and run `go run ./cmd/apidocs` from `platform/core`. The
prose above this line lives in `core/apidoc/preamble.md`.

## Listeners

Core serves this API, and the panel, on its main HTTP port. One origin, so a
browser reaches the API at `/api` on whatever host the pages came from.

A proxied custom tab is served on a HOSTNAME of its own, under
`TAB_PROXY_HOST_SUFFIX`, dispatched ahead of this router - so a tenant's
container never shares an origin with the panel or with anything below. That
host serves the container and one mint endpoint and nothing else; the ticket it
stores is decided here, on the panel's origin, where the session cookie is.

## Credentials (the Auth column)

**`session`** - `Authorization: Bearer <JWT>`, HS256, issued by
`POST /api/auth/login`. Two URL-borne fallbacks exist because `EventSource`
cannot set headers, and both are confined to `GET`: `?token=<jwt>` and
`?ticket=<t>` (a short-lived opaque ticket from `POST /api/sse-ticket`, whose
TTL slides on every accepted request so a reconnecting stream keeps working).
Either fallback makes the response `no-store` with `Referrer-Policy:
no-referrer`, so the credential does not leak through caches or the `Referer`
header. A mutating verb never authenticates from the URL.

Two token purposes are restricted rather than accepted: a `2fa_setup` token
reaches only the enrollment allowlist, and a `tab_proxy` ticket is rejected
outright on `/api` even though it is signed with the same key.

A session whose account row has disappeared is `401`. A session Core could not
*verify* - a database blip - is `503`, deliberately: logging everyone out would
hide an outage. The demo account is read-only, so any non-`GET` returns `403`,
and an unverifiable demo status returns `503` rather than assuming "not demo".

**`user API key`** - `Authorization: Bearer dyl_<secret>`, minted per user with
its own permission scope and server allowlist. Throttled to 120 requests per
minute per source IP *before* the database lookup, so keys that do not exist
cannot be used to hammer it. Today exactly one route accepts it.

**`warp key`** - `Authorization: Bearer <key>`, a separate credential class held
by a customer's warp client, not by a user.

**`none`** means no credential middleware, which is **not** the same as open.
Check the Gates column and the description: `/api/store/*` authenticates with a
shared `X-Store-Key` header inside the handler, and the four tab-proxy routes
trust only the host-only `dyl_tabproxy` ticket cookie. Genuinely public are the
health probe, the pack mirror a node installs a built pack from, share links,
and the login, registration, password-reset and setup-wizard endpoints.

## Authorization (the Capability column)

A capability id means `RequireCap` gates that route. **An admin short-circuits
every capability check** in the resolver, so an admin passes regardless.

The italic values say why a route declares none:

- **_in-handler_** - a capability does apply, but which one depends on the
  request body, so the check moved inside. `POST /api/servers/{id}/power` is the
  archetype: the action lives in the body, so the route cannot know whether it
  needs `power.start` or `power.kill`.
- **_no capability_** - a credential is required (see the Auth column) and no
  capability gates the route. This is **not** the same as open. Most of these
  are scoped to the caller inside the handler - your own profile, your own
  billing, your own route-only entries - and the rest are reference data or
  helpers that any authenticated caller may use. Which one it is, is in the
  Notes.
- **_public_** - no credential and no capability. These are the login,
  registration and reset endpoints, the health probe, the pack mirror, share
  links, and the tab proxy, which authenticates
  itself with a ticket cookie inside the handler.
- **_uncapped method_** - this method carries no capability, but its path
  template does, guarding a different method on the same path. Five `GET`s sit
  beside a capped write that way, each one an explicit decision recorded next to
  its entry in `requiredCaps`. Route coverage cannot show this, because coverage
  is keyed by template and never sees methods.

Every route lands in exactly one of capability / in-handler / exempt, and
`TestEveryRouteIsClassified` fails the build if one lands in none. The exempt
bucket is the one split above: the source keeps its two halves apart under
`PUBLIC` and `AUTHED-EXEMPT` headings in `authz/coverage.go`, but files the warp
and API-key routes under `PUBLIC` because they need no *session*. The table
splits on the credential instead, which is what a reader is actually asking.

## What wraps every /api route

Two pieces of middleware run before any per-route handling:

1. **Setup lock.** Until the first-run wizard completes, every `/api` route
   answers `503 setup_required`; `/api/setup/*` short-circuits so the wizard
   itself stays reachable.
2. **Maintenance gate.** While maintenance is active it blocks writes or all
   traffic according to the configured block level. It resolves admin status
   from the token itself, before per-route auth, so an admin can always switch
   maintenance back off.

Routes registered on the root router bypass both by design: `/healthz` (infra
probes must answer during setup and maintenance), `/mirror/{rest}` (a node
installing a built pack must reach it whatever the panel's state), `/api/share/{token}`,
and the four tab-proxy routes.

## Errors and limits

Errors are `{"success": false, "message": "..."}` with the matching HTTP status.

`Limit` in the Gates column is a per-client-IP request budget per minute; over
it, the answer is `429` with `Retry-After: 60`. `LimitBody` caps the request
body so an anonymous caller cannot make Core allocate without bound. The
`Require...Enabled` gates are feature flags and answer `503` when the feature is
off; `AllowReadOnlyWhenDisabled` lets reads through a disabled feature so the UI
can still show what exists.

## Reading the table

| Column | Meaning |
| --- | --- |
| Method | `(any)` means the registration constrains no method |
| Path | the mux template, including regex constraints on parameters |
| Auth | the credential the middleware accepts |
| Capability | the `RequireCap` id, or why there is none |
| Gates | the remaining middleware, outermost first |
| Handler | the Go method that serves it |
| Notes | the first sentence of that method's doc comment |
