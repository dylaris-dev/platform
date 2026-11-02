#!/usr/bin/env bash
# The Compose -> Swarm move, update, diagnostics and uninstall on a Swarm, against
# stand-in services. Needs a throwaway Docker daemon (it runs swarm init), so it
# runs inside docker:dind:
#   docker run -d --privileged --name x4 -v "$PWD/installer:/src:ro" docker:27-dind
#   docker exec x4 sh -c 'apk add --no-cache bash >/dev/null; bash /src/tests/swarm_move.sh'
#   docker rm -f x4
set -u
fail=0
check() { if eval "$2"; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
apk add --no-cache bash coreutils >/dev/null 2>&1
for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
docker pull -q alpine >/dev/null

O=/opt/dylaris; mkdir -p $O
cat >$O/docker-compose.yml <<'Y'
name: dylaris
services:
  core:
    image: alpine
    command: ["sh", "-c", "echo 'SETUP CODE: ABCD-EFGH-1234'; sleep 600"]
    volumes: [core_data:/data]
    networks: [dylaris_net]
  node:
    image: alpine
    command: sleep 600
    networks: [dylaris_net]
volumes:
  core_data: {}
networks:
  dylaris_net: {}
Y
printf 'JWT_SECRET=supersecretvalue123\n' >$O/.env
docker compose --project-directory $O -f $O/docker-compose.yml up -d 2>&1 | tail -3
docker run --rm -v dylaris_core_data:/data alpine sh -c 'echo keep >/data/marker'
docker run -d --name mc_test --network dylaris_dylaris_net alpine sleep 600 >/dev/null

sed '$d' /src/install.sh >/tmp/lib.sh   # drop the trailing main "$@"
# shellcheck disable=SC1091
source /tmp/lib.sh
set +e; trap - ERR
DOCKER=(docker); SUDO=""; OUT_DIR=$O

check "compose_running detects the compose install" "compose_running"
compose_to_swarm >/dev/null 2>&1
check "compose containers gone" "[[ -z \$(docker ps -aq --filter label=com.docker.compose.project=dylaris) ]]"
check "game container gone" "! docker inspect mc_test >/dev/null 2>&1"
check "bridge network freed" "! docker network inspect dylaris_dylaris_net >/dev/null 2>&1"
check "docker-compose.yml removed" "[[ ! -f $O/docker-compose.yml ]]"
check "volume kept with data" "[[ \$(docker run --rm -v dylaris_core_data:/data alpine cat /data/marker) == keep ]]"

docker swarm init >/dev/null 2>&1
cat >$O/docker-stack.yml <<'Y'
services:
  core:
    image: alpine
    command: ["sh", "-c", "echo 'SETUP CODE: ABCD-EFGH-1234 JWT_SECRET=supersecretvalue123'; sleep 600"]
    volumes: [core_data:/data]
    networks: [dylaris_net]
  node:
    image: alpine
    command: sleep 600
    networks: [dylaris_net]
    deploy: {mode: global}
volumes:
  core_data: {}
networks:
  dylaris_net: {driver: overlay, attachable: true}
Y
(cd $O && docker stack deploy -c docker-stack.yml dylaris 2>&1 | tail -3)
check "stack deploys over the freed network name" "stack_running"
sleep 8

out=$(cmd_update 2>&1); rc=$?
check "update works on a swarm install (rc=$rc)" "[[ $rc == 0 ]] && grep -q Updated <<<\"\$out\""

out=$(cmd_diagnostics 2>&1)
f=$(ls -t /tmp/dylaris-diagnostics-*.txt | head -1)
check "diagnostics has swarm service logs" "grep -q 'SETUP CODE' \"$f\""
check "diagnostics masks the setup code" "! grep -q ABCD-EFGH \"$f\""
check "diagnostics masks secrets" "! grep -q supersecretvalue \"$f\""
check "diagnostics mode 600" "[[ \$(stat -c %a \"$f\") == 600 ]]"

docker run -d --name mc_swarm --network dylaris_dylaris_net alpine sleep 600 >/dev/null
out=$(cmd_uninstall 2>&1); rc=$?
check "uninstall rc=0" "[[ $rc == 0 ]]"
check "stack gone" "! stack_running"
check "no stack containers left" "[[ -z \$(docker ps -aq --filter label=com.docker.stack.namespace=dylaris) ]]"
check "game container gone" "! docker inspect mc_swarm >/dev/null 2>&1"
echo "$out" | tail -3

# --purge on the moved install: the volume carries Compose's labels.
(cd $O && docker stack deploy -c docker-stack.yml dylaris >/dev/null 2>&1)
printf 'x' | docker secret create dylaris_jwt_secret - >/dev/null
sleep 8
sed '$d' /src/install.sh | sed 's|</dev/tty|</tmp/answer|' >/tmp/lib2.sh
echo "DELETE EVERYTHING" >/tmp/answer
# shellcheck disable=SC1091
source /tmp/lib2.sh
set +e; trap - ERR
# shellcheck disable=SC2034  # read by the sourced installer
DOCKER=(docker) SUDO="" OUT_DIR=$O PURGE=1
out=$(cmd_uninstall 2>&1); rc=$?
check "purge rc=0" "[[ $rc == 0 ]]"
check "purge removed the volume" "! docker volume inspect dylaris_core_data >/dev/null 2>&1"
check "purge removed the secret" "! docker secret inspect dylaris_jwt_secret >/dev/null 2>&1"
check "purge removed the dir" "[[ ! -d $O ]]"
echo "$out" | tail -3
exit $fail
