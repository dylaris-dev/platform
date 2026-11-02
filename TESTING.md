# Testing

DYLARIS platform has a real, maintained test suite. This documents the conventions so contributions stay consistent.

## Policy

- New code ships with tests. Security-critical and high-value pure logic gets tests as a standing practice, not a one-time backfill.
- No vanity coverage percentage. Chasing a number produces junk tests; the bar is "does this test catch a regression a reviewer would care about."
- (Historical note: platform ran without an automated test suite by an earlier project decision. That decision has been reversed - tests are expected going forward, starting with the Phase 1 slice covering zip-slip/decompression-bomb guards, SSRF-safe fetch, mrpack manifest parsing, config secret handling, backup path containment, JWT issuance, and Redis-ACL credential derivation.)

## Go

- stdlib `testing` only. No `testify`, no `gomock`, no other assertion/mocking library. Table-driven tests, `_test.go` beside the code it tests, same package by default (a `_test` external package only when it reads meaningfully cleaner, e.g. to avoid an import cycle).
- Run per Go module (the workspace root itself is not a runnable package set - `go test ./...` from the repo root fails with "directory prefix . does not contain modules listed in go.work"): `cd core && go test -count=1 ./...` (repeat for `node`, `pkg`, `log-shipper` and `beam/app`; `beam/app` first needs the stub frontend bundle described under CI). Keep `go build ./...` and `go vet ./...` clean alongside tests.
- For code coupled to `store.Store`, Redis, or raw SQL (Phase 2+):
  - **Store**: embed `store.Store` (interface, nil-valued) inside a small fake struct and override only the methods your code path touches. Any unoverridden method call panics - the test never makes one. See `core/handlers/gateway/warp_test.go` for the canonical example.
  - **Redis**: `github.com/alicebob/miniredis/v2` - an in-process Redis, point a real `*redis.Client` at it.
  - **Raw SQL**: `github.com/DATA-DOG/go-sqlmock`.
  - **HTTP**: `net/http/httptest`.
- **Core handler tests** (`core/handlers/<pkg>`): every handler package takes a
  `Deps` struct (same field names as `handlers.AppState`), so a test builds
  `<pkg>.Deps{Store: fake, Redis: rdb}` with only what the code path reads and
  calls the constructor. Never build a `handlers.AppState` in a handler package
  test; only package main (`core/*_test.go`) does, to drive the real router via
  `buildAPIRouter`. Shared fakes, requests and helpers live in
  `core/handlers/handlertest` (`ForeignStore`, `SettingsStore`, `OwnedNode`,
  `PlatformNode`, `AdminRequest`/`StaffRequest`/`VisAdminRequest`, miniredis
  `Redis`, `TempRoot`, `VoiceFixture`/`VoiceNode`, and the source-pin helpers
  `FuncBody`, `HandlersRoot`, `SourceFiles`). Look there before writing a fake;
  `handlertest` is for `_test.go` files only and never imports a layer-2
  handler package.
- **Source pins** (tests that read Go source, e.g. "every handler that sends a
  node command checks suspension") walk `handlertest.HandlersRoot` recursively
  or name a file by relative path. When code moves, the pin must move with it,
  and the move is only done once a mutation that REMOVES the guarded call makes
  the pin fail. They live with the guard they protect (`handlers/guard`,
  `handlers/reauth`, `handlers/backup`, `handlers/auth`, ...).
- **Layering** is a test: `core/layering_test.go` checks the import rules of
  the handler packages (layer 0 `httpx reqctx validate`, layer 1 `guard access
  audit reauth nodeops gate`, layer 2 the domain packages, only package main
  imports the root `handlers` package). Its allow-list may only shrink.
  `core/appstate_copy_test.go` pins that `main.go`/`routes.go` assign every
  `AppState` field before a `Deps` copy is taken and that every `Deps` literal
  in `routes.go` names every field.
- Golden-vector tests (e.g. `core/services/redisacl`) pin an exact known-answer value so a silent algorithm drift breaks the build loudly. Do not "fix" a failing golden test by updating the expected value without understanding why the output changed - a mismatch there usually means a real behavior change that needs to ship in lockstep with any other side depending on that exact byte format (see the `redisacl` package doc comment for a concrete case: Core and `node/redisacl.go` must derive byte-identical credentials, verified by pinning the same golden vectors in both, since the two are separate Go modules that cannot import each other).

## Panel (TypeScript)

- `vitest`, already configured (`panel/vitest.config.ts`, `environment: 'node'`). Phase 1 covers pure `src/lib/**/*.ts` logic only - no component/RSC/E2E testing yet (that needs jsdom + Testing Library + Playwright, a separate future workstream).
- `*.test.ts` beside the module it tests. See `panel/src/lib/api/core.test.ts` or `panel/src/lib/cpuset.test.ts` for the house style: `describe`/`it`/`expect`, one `describe` block per exported function, one `it` per behavior.
- Run: `cd panel && npx vitest run`.

## CI

Every push and pull request runs `.github/workflows/ci.yml`. A failure in any of
it fails the build.

- **`go-tests`** - a matrix over `core`, `node`, `pkg`, `log-shipper` and
  `beam/app`. Each cell runs `go build`, `go vet`, **gofmt** (`gofmt -l`, the
  vendored `third_party/` tree excluded), **staticcheck**, `go test` and
  **govulncheck** (reachability-based; `.github/vulngate.awk` decides what
  fails). staticcheck is a real gate and catches things the other three do
  not, so run it before pushing:
  `go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 -checks 'SA*,U1000,-SA1019,-SA1029,-SA9003' ./...`
  from the module. A push has gone red on SA4000 with everything else green.
  U1000 (unused) is in the gate because dead code here is not a style question:
  a helper written so two route handlers could not drift apart on what they
  promised the customer had no caller, so neither handler promised anything.
  Deliberate exceptions take `//lint:ignore U1000 <reason>`.
  `beam/app` needs a stub `frontend/dist/index.html` to compile at all (`app.go`
  has a `//go:embed` on a Wails build artifact); the job creates one, and the
  same works locally from `beam/app`:
  `mkdir -p frontend/dist && printf '<!doctype html><title>stub</title>' > frontend/dist/index.html`.
- **`race-tests`** - `go test -race` over every Go module, run as a docker BUILD
  because `-race` needs cgo and this runner has no gcc. See
  `.github/race.Dockerfile`, which also records why two simpler mechanisms were
  tried and abandoned. To reproduce locally on a machine without cgo:
  `docker build -f .github/race.Dockerfile .`
- **`db-tests`** - the `core/database` integration tests against a real
  `postgres:16-alpine`, built from `.github/db.Dockerfile`. The suite is
  switched on by `DYLARIS_TEST_DB_HOST` (plus `DYLARIS_TEST_DB_PORT`, `_USER`,
  `_PASSWORD`, `_NAME`) and skips itself when that is unset, so a plain
  `go test ./...` stays green without a database.
- **`panel-tests`** - vitest.
- **Release notes** - once the test jobs are green, and before any image is
  built: `cd pkg && go run ./cmd/relnotes validate ../core/updates/platform.md ../core/updates/hosted.md`.
  The format is in `core/updates/README.md`.

**A green race run proves less than it looks like.** It only covers concurrency
that a TEST creates. Every race found in this repo lived between production
goroutines no test starts, and `-race` reported nothing before or after the fix
until a test drove both sides. If you write one, make it **time-based**
(`time.Sleep`), not an iteration count - a tight loop finishes before the writer
goroutine is ever scheduled, and then the detector sees no conflicting access and
passes against completely unguarded globals. That happened here.
