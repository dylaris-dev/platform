# PANEL_STAGE selects whether the panel bundle is built at all.
#
# This file builds BOTH Core and the Node (ENTRY_PATH picks the binary), and
# only Core carries the panel. Building it unconditionally would put an npm
# install and a Next build on every node image for output nothing copies. Docker
# only builds a stage something references, so the reference itself is what is
# switched: Core passes PANEL_STAGE=panel-build, the node leaves it at the empty
# default.
#
# Must sit above the first FROM to be usable in one, and be re-declared in any
# stage that reads it.
ARG PANEL_STAGE=panel-none

# --- Panel Stage: the static export Core serves ---
FROM node:22-alpine AS panel-build
WORKDIR /app

# Same workspace dance as the old panel/Dockerfile, and the lockfile is
# deliberately not copied: it was generated on Windows and misses the
# linux-musl native binaries for lightningcss/swc, so `npm install` has to
# resolve the platform-specific optional deps fresh.
COPY package.json ./
COPY packages/ ./packages/
COPY panel/package.json ./panel/
# npm is PINNED, and it is the one pin this stage cannot do without.
#
# The 10.9.8 that ships in node:22-alpine hits an arborist bug while resolving
# this workspace's peer graph - "Cannot read properties of null (reading
# 'edgesOut')" in #loadPeerSet - and it took the build down on 2026-09-04 with no
# change on our side. Measured rather than guessed: the same tree, resolved in
# the same minute, fails on 10.9.8 and succeeds on 11.19.1.
#
# Because the lockfile above is deliberately not copied, every build resolves
# fresh, so anything upstream can break this layer at any time and the failure
# arrives attached to whichever commit happened to be pushed. An exact version
# rather than npm@11: a floating npm is precisely what just did this.
RUN npm i -g npm@11.19.1
RUN npm install --workspaces --include-workspace-root

COPY panel/ ./panel/
# Next treats a nested lockfile as a second workspace root and picks the wrong one.
RUN rm -f /app/panel/package-lock.json

WORKDIR /app/panel
# `npm run build` is `next build` FOLLOWED BY scripts/stamp-nonce.mjs, which
# writes the CSP nonce placeholder onto every script tag. Skipping the second
# half produces a bundle whose scripts the browser blocks, so it is one script
# rather than two RUN lines somebody can reorder.
RUN npm run build
RUN mkdir -p /panel && cp -a out/. /panel/

# --- Panel Stage (none): an empty directory for every non-Core build ---
FROM alpine:latest AS panel-none
RUN mkdir -p /panel

# The indirection that makes the choice a build arg.
FROM ${PANEL_STAGE} AS panel

# --- Build Stage ---
FROM golang:1.26.7-alpine AS builder

# Install git (important for go mod download with some libs)
RUN apk add --no-cache git

# FIX for error 128: Allow Git to use the directory
RUN git config --global --add safe.directory '*'

WORKDIR /src

# Copy everything
COPY . .

# The panel export goes where //go:embed can see it. An empty /panel (the node
# build) leaves the committed placeholder in place, so the embed directive
# always has something to read and the build never depends on which stage ran.
COPY --from=panel /panel/ /panel/
RUN if [ -n "$(ls -A /panel 2>/dev/null)" ]; then \
      rm -rf ./core/panelfs/dist && cp -a /panel ./core/panelfs/dist && \
      echo "panel: embedded $(find ./core/panelfs/dist -name '*.html' | wc -l) pages"; \
    else \
      echo "panel: no bundle for this build; keeping the placeholder"; \
    fi

# Arguments
ARG ENTRY_PATH
ARG BUILD_TAGS=""
# RELEASE_VERSION stamps the release this image is built from into
# main.releaseVersion, so a component can report its OWN position instead of
# Core assuming they all moved together. Left empty for components that carry no
# such variable; the linker ignores -X for a symbol that does not exist, so
# passing it everywhere is harmless.
#
# Empty is also the correct value when the repo has no releases yet: the
# component then reports nothing, which reads as "not reporting" rather than as
# a version it does not have.
ARG RELEASE_VERSION=""

# Build
# We explicitly specify the path and use sh expansion
# BUILD_TAGS allows excluding packages (e.g. "noxdp" to skip eBPF)
RUN echo "Building from: ${ENTRY_PATH} (tags: ${BUILD_TAGS:-none}, release: ${RELEASE_VERSION:-unstamped})" && \
    LD="-s -w"; \
    if [ -n "${RELEASE_VERSION}" ]; then LD="${LD} -X main.releaseVersion=${RELEASE_VERSION}"; fi && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags="${BUILD_TAGS}" -ldflags="${LD}" -o /app/binary ${ENTRY_PATH} && \
    chmod +x /app/binary

# --- Run Stage ---
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/binary .

ARG INSTALL_QUOTA=""
RUN chmod +x /app/binary && apk add --no-cache ca-certificates
RUN if [ "$INSTALL_QUOTA" = "1" ]; then apk add --no-cache quota-tools e2fsprogs-extra xfsprogs xfsprogs-extra && echo "Quota tools installed"; fi

# This image builds both Core and Node (ENTRY_PATH selects the binary); they
# need different runtime privileges, so both are threaded through as build
# args (ci.yml passes SERVICE=core/RUN_AS=dylaris for Core; Node's build
# leaves both at their root defaults below).
#   SERVICE - selects the HEALTHCHECK branch below (Core has an HTTP surface
#             to probe, Node does not).
#   RUN_AS  - the USER the container runs as. Core has no reason to run as
#             root. Node stays root: it mounts the host Docker socket
#             (/var/run/docker.sock) to manage MC server containers, which
#             needs root (or the socket's host GID, not portable across
#             hosts) - documented exception.
ARG SERVICE=node
ARG RUN_AS=root
ENV SERVICE=$SERVICE

# nftables, Node only (~1 MB). It is what writes the per-server ingress rules.
#
# The node does NOT use it on itself. It starts a throwaway container from THIS
# image joined to a game server's network namespace
# (--network container:<mc_uuid> --cap-add NET_ADMIN), which writes the ruleset
# and exits - so the capability is borrowed for a second by something that goes
# away, and the node keeps exactly the privileges it already had. That is only
# possible because the helper image is one every host running a node has
# already pulled: this one. See node/netpolicy.go.
RUN if [ "$SERVICE" = "node" ]; then apk add --no-cache nftables && echo "nftables installed (per-server ingress policy)"; fi
# mkdir the data mount point BEFORE chown so a fresh named volume mounted here
# inherits uid-1000 ownership (Docker seeds a new volume's ownership from the
# image dir). Without this, non-root Core (RUN_AS=dylaris) cannot write the volume.
# pg_dump / pg_restore for platform backups, Core only. Node never touches a
# database and would carry 16 MB for nothing.
#
# TWO clients, and each one is load-bearing in a different direction. The
# constraint is asymmetric and was MEASURED, not assumed:
#
#   pg_dump    refuses a server NEWER than itself.
#   pg_restore emits SQL the target must understand, so a client newer than the
#              target fails - 17 and 18 write "SET transaction_timeout = 0",
#              which PostgreSQL 15 does not know, and the restore stops there.
#
# One client therefore cannot serve both a PG15 platform database and a
# self-hoster on PG18. services.PGClientFor picks the smallest installed client
# that is at least as new as the server; they live in versioned directories and
# do not collide.
RUN if [ "$SERVICE" = "core" ]; then apk add --no-cache postgresql16-client postgresql18-client; fi

RUN adduser -D -u 1000 dylaris && mkdir -p /app/dylaris_data && chown -R dylaris:dylaris /app
USER ${RUN_AS}

# No-op for Node (SERVICE != core, the `if` is skipped and the shell exits 0).
# Core: pings its own /healthz (DB + Redis), added in core/handlers/health.go.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD if [ "$SERVICE" = "core" ]; then wget -q -O /dev/null "http://127.0.0.1:${API_PORT:-25500}/healthz" || exit 1; fi

CMD ["./binary"]
