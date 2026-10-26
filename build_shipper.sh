#!/usr/bin/env bash
set -e
cd "$(dirname "$0")/log-shipper"
# One binary per image platform: the mc-java Dockerfiles copy
# log-shipper-linux-${TARGETARCH}.
for arch in amd64 arm64; do
  echo "Building log-shipper for linux/${arch}..."
  CGO_ENABLED=0 GOOS=linux GOARCH=${arch} GOWORK=off go build -trimpath -o ../docker/mc-server/log-shipper-linux-${arch} .
done
echo "Done: docker/mc-server/log-shipper-linux-{amd64,arm64}"
