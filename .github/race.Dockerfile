# Race-detector gate for core, run as a Docker BUILD rather than a container
# the workflow starts itself.
#
# That indirection is not decoration. Two simpler mechanisms were tried on this
# self-hosted runner and both failed for reasons that have nothing to do with
# the code under test:
#
#   docker run -v "$PWD:/src"  - the runner is itself containerized, so that
#     path does not exist for the host daemon. Docker then CREATES an empty
#     directory and mounts it instead of failing, and the run died with
#     "directory prefix . does not contain main module".
#
#   a job-level `container:`   - job containers need the runner's externals
#     mounted at /__e, and this runner does not pass them through, so
#     actions/checkout could not exec node ("/__e/node24/bin/node: no such
#     file or directory").
#
# A build works because the context is UPLOADED through the Docker API instead
# of mounted, which is exactly why the image-build jobs on this same runner
# have always worked. A failing RUN fails the build, which fails the step.
#
# Deliberately unoptimized: one COPY, one RUN, no BuildKit cache mounts and no
# go.mod-first layer split. Both would help the runtime and both are more
# moving parts than a gate that has already failed twice should carry. Optimize
# once this has been green on a real run.

FROM golang:1.26

WORKDIR /src

# The whole repo, because GOWORK must stay ON: core/go.mod replaces only
# dylaris-pkg, while dylaris-proto resolves through go.work, and a workspace
# refuses to load unless every member directory it lists is present.
COPY . .

# Running ./... from core still compiles only core's own packages, so beam/app
# is never built here and needs no Wails frontend stub.
WORKDIR /src/core
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# node joined 2026-08-02, which is the follow-up the header describes: it was
# checked by hand first, that check found two real races (the shared-storage
# ticker against the heartbeat, and the 30s mode refresh against every reader of
# routingMode and friends), and the module is clean afterwards.
#
# Worth knowing what this does NOT prove. Both races were invisible to the
# detector until a test started the goroutines together - node's concurrency
# lives in production goroutines that tests do not run, so a green result here
# means "nothing racy in what the tests exercise", not "no races". Adding a test
# is what extends the coverage; the gate only reports.
#
# And make such a test time-based, not an iteration count: a tight read loop
# finishes before the writer goroutine is ever scheduled, the detector then sees
# no conflicting access, and the test passes against completely unguarded
# globals. That happened here, and the test looked perfectly reasonable.
#
# One RUN per module rather than one build step per module: the context upload
# and the module download are the expensive parts and this way they happen once.
WORKDIR /src/node
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# pkg joined the same day, under the same rule. Its concurrency is one package:
# queue's worker pool. Everything else (validate, protocol, storageplacement,
# migration, errlog, beam) is pure. The hand check found no race there -
# Consumer's fields are write-once before Run and the pool communicates over a
# channel - but it did find a dead-letter that ACKed a message it had failed to
# park, which is why the check is worth doing whatever the detector says.
WORKDIR /src/pkg
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# log-shipper: nothing to guard. No mutable package state at all, and its two
# scanner goroutines hand lines to one shipper over a channel that is closed
# only after a WaitGroup says both writers are done.
WORKDIR /src/log-shipper
RUN CGO_ENABLED=1 go test -race -count=1 ./...

# beam/app carries the repo's most exposed concurrency: Wails dispatches every
# frontend binding call on its own goroutine, so user clicks are the scheduler.
# The hand check found it already correct throughout - each guarded field is
# reachable only through a locking accessor, and the teardown paths return the
# old client so the caller Closes it OUTSIDE the mutex, keeping network I/O off
# the lock. Gated so it stays that way.
#
# The frontend/dist stub is required, not cosmetic: app.go has a //go:embed on a
# Wails build artifact that a fresh checkout does not carry, so the module does
# not compile without it. ci.yml's go-tests job stubs it the same way.
WORKDIR /src/beam/app
RUN mkdir -p frontend/dist && printf '<!doctype html><title>stub</title>' > frontend/dist/index.html
RUN CGO_ENABLED=1 go test -race -count=1 ./...
