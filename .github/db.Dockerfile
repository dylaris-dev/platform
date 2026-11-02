# Postgres integration gate, packaged as an image the workflow then RUNS on a
# docker network - not as a job that talks to a service container.
#
# That indirection is forced by the same environment fact race.Dockerfile
# records: THIS RUNNER IS ITSELF CONTAINERIZED. A `services:` block does start
# a healthy Postgres and `docker port` does show 5432/tcp -> 0.0.0.0:5432, but
# that publish lands on the HOST daemon, outside the runner's network
# namespace, so the job cannot reach it - localhost and 127.0.0.1 alike were
# refused, and InitDB then retried the ping for two minutes per test.
#
# Two containers on one user-defined network reach each other by NAME, which
# needs no published port and no shared namespace. The image is built here
# rather than mounted for the reason race.Dockerfile gives: a build UPLOADS its
# context through the Docker API, and a bind mount would point at a path the
# host daemon does not have.
#
# The tests are the RUN target of `docker run`, not of a build step: a build
# cannot join a user-defined network, so the database has to be reachable at
# container runtime instead.

FROM golang:1.26@sha256:670bb5aa5349ae03b8516764718486f4f16399134bdc9acd0c46a23f043ab05c

WORKDIR /src

# The whole repo, because GOWORK must stay ON: core/go.mod replaces only
# dylaris-pkg while dylaris-proto resolves through go.work, and a workspace
# refuses to load unless every member directory it lists is present.
COPY . .

WORKDIR /src/core

# Compile the package under test at build time so the run stage is the test
# itself. Also warms the module cache into the image, so a failing database
# fails in seconds rather than after a download.
RUN go build ./... && go vet ./database/

# Only the database-backed tests. Everything else in core already runs in the
# go-tests matrix, against fakes, where it belongs.
CMD ["go", "test", "-count=1", "-v", "-run", "Integration|Prepares|FreshInstall", "./database/"]
