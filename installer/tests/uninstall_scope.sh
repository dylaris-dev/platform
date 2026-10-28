#!/usr/bin/env bash
# Uninstall must remove this install's game servers and nobody else's.
#
# Needs Docker and root, and a machine WITHOUT a real "dylaris" project: it
# brings up a stand-in project of that name. Run: sudo bash installer/tests/uninstall_scope.sh
set -u
here=$(cd "$(dirname "$0")/.." && pwd)
docker network ls --format '{{.Name}}' | grep -q '^dylaris_dylaris_net$' && { echo "a dylaris project exists here; refusing"; exit 2; }

T=$(mktemp -d /tmp/uninstall-scope.XXXXXX)
cleanup() {
  docker rm -f mc_own_stopped mc_own_running mc_foreign mc_nonet >/dev/null 2>&1
  docker network rm other_dylaris_net dylaris_dylaris_net >/dev/null 2>&1
  rm -rf "$T"
}
trap cleanup EXIT

cat >"$T/docker-compose.yml" <<'Y'
name: dylaris
services:
  idle:
    image: alpine
    command: sleep 600
    networks: [dylaris_net]
networks:
  dylaris_net: {}
Y
docker compose --project-directory "$T" -f "$T/docker-compose.yml" up -d >/dev/null 2>&1
docker network create other_dylaris_net >/dev/null
docker create --name mc_own_stopped --network dylaris_dylaris_net alpine sleep 600 >/dev/null
docker run -d --name mc_own_running --network dylaris_dylaris_net alpine sleep 600 >/dev/null
docker run -d --name mc_foreign --network other_dylaris_net alpine sleep 600 >/dev/null
docker run -d --name mc_nonet alpine sleep 600 >/dev/null

bash "$here/install.sh" uninstall --output-dir "$T" >/dev/null 2>&1

left=$(docker ps -a --format '{{.Names}}' | grep -E '^mc_(own_stopped|own_running|foreign|nonet)$' | sort | tr '\n' ' ')
want="mc_foreign mc_nonet "
if [[ $left == "$want" ]] && ! docker network ls --format '{{.Name}}' | grep -q '^dylaris_dylaris_net$'; then
  echo "PASS: own servers removed, foreign kept, network gone"
else
  echo "FAIL: left [$left], want [$want]; network: $(docker network ls --format '{{.Name}}' | grep -c '^dylaris_dylaris_net$')"
  exit 1
fi
