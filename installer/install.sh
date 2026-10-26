#!/usr/bin/env bash
# DYLARIS platform installer.
#
# Interactive, terminal only. Installs Core + Node with a bundled or an existing
# PostgreSQL and Valkey/Redis, on one host (Docker Compose) or several (Docker
# Swarm). Nothing is changed before the plan is shown and confirmed.
#
#   bash install.sh [install|add-node|update|uninstall|diagnostics] [options]
#
# The Gateway is not installed here: it is a managed DYLARIS service.
# Design: docs/superpowers/specs/2026-10-26-installer-design.md

set -Eeuo pipefail

INSTALLER_VERSION="0.1.1"
PROJECT="dylaris"
IMAGE_CORE="ghcr.io/dylaris-dev/platform-core"
IMAGE_NODE="ghcr.io/dylaris-dev/platform-node"
IMAGE_TAG="latest"
IMAGE_PG="timescale/timescaledb:latest-pg16"
IMAGE_VALKEY="valkey/valkey:8-alpine"
PROBE_PG="postgres:16-alpine"

CMD="install"
OUT_DIR="/opt/dylaris"
CONFIG_FILE=""
DRY_RUN=0
CHECK_ONLY=0
PURGE=0
declare -A CFG=()     # answers from --config
declare -A A=()       # collected answers
CHANGES=()            # the plan, shown before anything happens

# ---------------------------------------------------------------- output ----

if [[ -t 1 ]]; then
  B=$'\e[1m'; D=$'\e[2m'; R=$'\e[31m'; G=$'\e[32m'; Y=$'\e[33m'; V=$'\e[35m'; N=$'\e[0m'
else
  B=""; D=""; R=""; G=""; Y=""; V=""; N=""
fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s  %s\n' "${V}::${N}" "$*"; }
ok()   { printf '%s  %s\n' "${G}ok${N}" "$*"; }
warn() { printf '%s  %s\n' "${Y}!!${N}" "$*"; }
fail() { printf '%s  %s\n' "${R}xx${N}" "$*" >&2; }
die()  { fail "$*"; exit 1; }
step() { printf '\n%s%s%s\n' "$B" "$*" "$N"; }

trap 'fail "Stopped at line $LINENO. Nothing after this point was changed; run the installer again to continue."' ERR

# ----------------------------------------------------------------- input ----

# A value from --config wins; without one, interactive mode asks and the
# non-interactive mode takes the default or stops on a required answer.
ask() { # ask KEY "Question" "default"
  local key=$1 q=$2 def=${3:-} v
  if [[ -n ${CFG[$key]+x} ]]; then A[$key]=${CFG[$key]}; return; fi
  if [[ -n $CONFIG_FILE ]]; then
    [[ -n $def ]] || die "$key is required in $CONFIG_FILE"
    A[$key]=$def; return
  fi
  read -r -p "$q${def:+ [$def]}: " v </dev/tty || true
  A[$key]=${v:-$def}
}

ask_secret() { # ask_secret KEY "Question"  - typed twice, never echoed
  local key=$1 q=$2 a b
  if [[ -n ${CFG[$key]+x} ]]; then A[$key]=${CFG[$key]}; return; fi
  [[ -z $CONFIG_FILE ]] || die "$key is required in $CONFIG_FILE"
  while true; do
    read -r -s -p "$q: " a </dev/tty; echo
    read -r -s -p "Again: " b </dev/tty; echo
    [[ -n $a && $a == "$b" ]] && break
    warn "Empty or not the same. Try again."
  done
  A[$key]=$a
}

ask_yn() { # ask_yn KEY "Question" y|n  -> A[KEY]=y|n
  local key=$1 q=$2 def=$3 v
  if [[ -n ${CFG[$key]+x} ]]; then v=${CFG[$key]}
  elif [[ -n $CONFIG_FILE ]]; then v=$def
  else read -r -p "$q [$( [[ $def == y ]] && echo Y/n || echo y/N )]: " v </dev/tty || true; fi
  v=${v:-$def}; v=${v,,}
  [[ $v == y* ]] && A[$key]=y || A[$key]=n
}

menu() { # menu KEY "Question" "value|Label" ...  -> A[KEY]=value (first is default)
  local key=$1 q=$2; shift 2
  local opts=("$@") i v
  if [[ -n ${CFG[$key]+x} ]]; then A[$key]=${CFG[$key]}; return; fi
  if [[ -n $CONFIG_FILE ]]; then A[$key]=${opts[0]%%|*}; return; fi
  say "$q"
  for i in "${!opts[@]}"; do printf '  %s%d)%s %s\n' "$B" $((i + 1)) "$N" "${opts[$i]#*|}"; done
  while true; do
    read -r -p "Choose [1]: " v </dev/tty || true
    v=${v:-1}
    if [[ $v =~ ^[0-9]+$ ]] && (( v >= 1 && v <= ${#opts[@]} )); then
      A[$key]=${opts[$((v - 1))]%%|*}; return
    fi
    warn "Enter a number from the list."
  done
}

load_config() { # KEY=VALUE lines; never sourced, so nothing in it can run
  local line k v
  while IFS= read -r line || [[ -n $line ]]; do
    [[ $line =~ ^[[:space:]]*(#|$) ]] && continue
    [[ $line == *=* ]] || die "Bad line in $CONFIG_FILE: $line"
    k=${line%%=*}; v=${line#*=}
    k=${k//[[:space:]]/}
    v=${v#\"}; v=${v%\"}
    CFG[$k]=$v
  done <"$CONFIG_FILE"
}

# ------------------------------------------------------------- execution ----

SUDO=""
need_root() {
  if [[ $EUID -ne 0 ]]; then
    command -v sudo >/dev/null || die "Run as root, or install sudo."
    SUDO="sudo"
  fi
}

run() { # run CMD...  - shown, not run, in --dry-run
  if (( DRY_RUN )); then printf '%s  %s\n' "${D}dry-run${N}" "$*"; return 0; fi
  "$@"
}

DOCKER=(docker)
docker_cmd() {
  if docker info >/dev/null 2>&1; then DOCKER=(docker)
  elif [[ -n $SUDO ]] && $SUDO docker info >/dev/null 2>&1; then DOCKER=("$SUDO" docker)
  else return 1; fi
}

gen_secret() { od -An -tx1 -N32 /dev/urandom | tr -d ' \n'; }
mask() { local s=$1; printf '%s...(%d chars)' "${s:0:4}" "${#s}"; }

json_escape() { local s=$1; s=${s//\\/\\\\}; s=${s//\"/\\\"}; s=${s//$'\n'/\\n}; printf '%s' "$s"; }

# ------------------------------------------------------------- preflight ----

OS_ID="" OS_LIKE="" OS_CODENAME="" ARCH="" PRE_FAIL=0 PRE_WARN=0
pre_ok()   { ok "$*"; }
pre_warn() { warn "$*"; PRE_WARN=$((PRE_WARN + 1)); }
pre_fail() { fail "$*"; PRE_FAIL=$((PRE_FAIL + 1)); }

preflight() {
  step "1. Checking this machine"
  if [[ -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_ID=${ID:-}; OS_LIKE=${ID_LIKE:-}; OS_CODENAME=${VERSION_CODENAME:-}
    case "$OS_ID" in
      debian|ubuntu|raspbian) pre_ok "System: ${PRETTY_NAME:-$OS_ID}" ;;
      *) if [[ $OS_LIKE == *debian* ]]; then pre_warn "System: ${PRETTY_NAME:-$OS_ID} (Debian-based, not tested)";
         else pre_warn "System: ${PRETTY_NAME:-$OS_ID} is not tested; Debian, Ubuntu and Raspberry Pi OS are."; fi ;;
    esac
  else
    pre_fail "No /etc/os-release: this installer runs on Linux (on Windows use install.ps1)."
  fi

  ARCH=$(uname -m)
  case "$ARCH" in
    x86_64|amd64) pre_ok "CPU: $ARCH" ;;
    aarch64|arm64) pre_ok "CPU: $ARCH (64-bit ARM)" ;;
    *) pre_fail "CPU: $ARCH is not supported. Use a 64-bit system (on a Raspberry Pi: the 64-bit Raspberry Pi OS)." ;;
  esac

  local mem_mb disk_gb inodes
  mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo 2>/dev/null || echo 0)
  if (( mem_mb < 1800 )); then pre_warn "RAM: ${mem_mb} MB. Core, the database and one small server need about 2 GB; Minecraft needs more."
  else pre_ok "RAM: ${mem_mb} MB"; fi
  local probe_dir=$OUT_DIR; while [[ ! -d $probe_dir ]]; do probe_dir=$(dirname "$probe_dir"); done
  disk_gb=$(df -Pk "$probe_dir" | awk 'NR==2 {print int($4/1048576)}')
  if (( disk_gb < 10 )); then pre_warn "Free disk at $probe_dir: ${disk_gb} GB (10 GB or more recommended)"
  else pre_ok "Free disk at $probe_dir: ${disk_gb} GB"; fi
  inodes=$(df -Pi "$probe_dir" | awk 'NR==2 {print $4}')
  [[ $inodes =~ ^[0-9]+$ ]] && (( inodes < 100000 )) && pre_warn "Few free inodes at $probe_dir ($inodes)"

  command -v curl >/dev/null || pre_fail "curl is missing (apt-get install curl)."
  if command -v docker >/dev/null; then
    pre_ok "Docker: $(docker --version 2>/dev/null | head -1)"
    if docker_cmd; then
      pre_ok "Docker is reachable${SUDO:+ (through sudo)}"
      if "${DOCKER[@]}" compose version >/dev/null 2>&1; then pre_ok "Compose: $("${DOCKER[@]}" compose version --short 2>/dev/null)"
      else pre_fail "Docker Compose v2 is missing (package docker-compose-plugin)."; fi
      if "${DOCKER[@]}" info 2>&1 | grep -q "No memory limit support"; then
        pre_warn "This kernel cannot limit a server's RAM (memory cgroup off). On Raspberry Pi OS the installer can switch it on."
        A[memcg_off]=y
      fi
      if "${DOCKER[@]}" ps -a --format '{{.Names}}' | grep -q "^${PROJECT}[-_]"; then
        pre_warn "DYLARIS containers already exist on this machine; the installer keeps their data and secrets."
      fi
    else
      pre_fail "Docker is installed but not reachable. Start it (systemctl start docker) or run with sudo."
    fi
  else
    pre_warn "Docker is not installed. The installer can install it."
    A[need_docker]=y
  fi

  if command -v timedatectl >/dev/null; then
    if [[ $(timedatectl show -p NTPSynchronized --value 2>/dev/null) == yes ]]; then pre_ok "Clock is synchronised"
    else pre_warn "Clock is not NTP-synchronised; TLS and two-factor codes need a correct time (timedatectl set-ntp true)."; fi
  fi

  local p busy=()
  if command -v ss >/dev/null; then
    for p in 25500 25501 25443 25520 25523; do
      ss -ltnH "sport = :$p" 2>/dev/null | grep -q . && busy+=("$p")
    done
    if (( ${#busy[@]} )); then
      if "${DOCKER[@]}" ps --format '{{.Names}}' 2>/dev/null | grep -q "^${PROJECT}[-_]core"; then
        pre_ok "Ports ${busy[*]} are used by the running DYLARIS installation"
      else pre_fail "Ports in use by something else: ${busy[*]}"; fi
    else pre_ok "Ports 25500, 25501, 25443, 25520, 25523 are free"; fi
  fi

  say ""
  if (( PRE_FAIL )); then fail "$PRE_FAIL check(s) failed. Fix them and run the installer again."; return 1; fi
  if (( PRE_WARN )); then ok "Preflight passed with $PRE_WARN warning(s)."; else ok "Preflight passed."; fi
}

# ---------------------------------------------------------------- docker ----

install_docker() {
  step "Installing Docker"
  need_root
  local repo=$OS_ID
  # 64-bit Raspberry Pi OS uses Docker's Debian packages.
  [[ $repo == raspbian ]] && repo=debian
  [[ $repo == debian || $repo == ubuntu ]] || {
    [[ $OS_LIKE == *ubuntu* ]] && repo=ubuntu || repo=debian
  }
  local codename=$OS_CODENAME
  [[ -n $codename ]] || die "Cannot tell the release codename; install Docker by hand: https://docs.docker.com/engine/install/"
  local arch; arch=$(dpkg --print-architecture)
  CHANGES+=("Install Docker Engine + Compose from download.docker.com ($repo $codename, $arch)")
  run $SUDO apt-get update -qq
  run $SUDO apt-get install -y -qq ca-certificates curl
  run $SUDO install -m 0755 -d /etc/apt/keyrings
  run $SUDO curl -fsSL "https://download.docker.com/linux/$repo/gpg" -o /etc/apt/keyrings/docker.asc
  run $SUDO chmod a+r /etc/apt/keyrings/docker.asc
  if (( ! DRY_RUN )); then
    echo "deb [arch=$arch signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$repo $codename stable" |
      $SUDO tee /etc/apt/sources.list.d/docker.list >/dev/null
  fi
  run $SUDO apt-get update -qq
  run $SUDO apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  run $SUDO systemctl enable --now docker
  if [[ -n $SUDO && -n ${USER:-} ]]; then
    ask_yn add_group "Add $USER to the docker group (docker without sudo after you log in again)?" y
    [[ ${A[add_group]} == y ]] && run $SUDO usermod -aG docker "$USER"
  fi
  (( DRY_RUN )) || docker_cmd || die "Docker was installed but does not answer."
  ok "Docker installed"
}

fix_pi_memcg() {
  local f
  for f in /boot/firmware/cmdline.txt /boot/cmdline.txt; do [[ -f $f ]] && break; done
  [[ -f $f ]] || return 0
  grep -q "cgroup_enable=memory" "$f" && return 0
  ask_yn memcg_fix "Switch the memory cgroup on in $f (needs a reboot afterwards)?" y
  [[ ${A[memcg_fix]} == y ]] || { warn "Then Docker cannot hold servers to their RAM."; return 0; }
  need_root
  run $SUDO cp "$f" "$f.dylaris-backup"
  run $SUDO sed -i '1 s/$/ cgroup_enable=memory cgroup_memory=1/' "$f"
  ok "Done. Reboot (sudo reboot) and run the installer again."
  exit 0
}

# --------------------------------------------------------------- answers ----

# Through sudo: the directory is root's (0750), and a check that cannot see
# the file would take an existing installation for a new one and replace its
# secrets.
existing_env() { $SUDO test -f "$OUT_DIR/.env" 2>/dev/null; }

env_get() { # value of KEY in the existing .env
  local k=$1
  existing_env || return 0
  $SUDO awk -F= -v k="$k" '$1==k { sub(/^[^=]*=/, ""); print; exit }' "$OUT_DIR/.env" 2>/dev/null || true
}

interview() {
  step "2. How to install"
  menu topology "Where should DYLARIS run?" \
    "single|One machine (Docker Compose) - right for almost everyone" \
    "swarm|Several machines (Docker Swarm) - Core here, nodes join later"
  menu mode "Generate the files only, or also start DYLARIS?" \
    "start|Generate and start" \
    "files|Generate the files only (start them yourself later)"

  step "3. Database (PostgreSQL)"
  menu db "Which PostgreSQL?" \
    "bundled|Install one with DYLARIS (TimescaleDB, recommended)" \
    "existing|Use an existing PostgreSQL server"
  if [[ ${A[db]} == existing ]]; then
    ask db_host "Host (not localhost: the containers need an address they can reach)" ""
    ask db_port "Port" "5432"
    ask db_name "Database name" "dylaris"
    ask db_user "Application user" "dylaris"
    ask_secret db_password "Password for ${A[db_user]}"
    ask db_sslmode "SSL mode (disable, require, verify-full)" "require"
    ask_yn db_timescale "Does this server have the TimescaleDB extension?" n
    check_postgres
  else
    A[db_host]=timescaledb; A[db_port]=5432; A[db_name]=dylaris; A[db_user]=dylaris
    A[db_sslmode]=disable; A[db_timescale]=y
    A[db_password]=$(env_get DB_PASSWORD); [[ -n ${A[db_password]} ]] || A[db_password]=$(gen_secret)
  fi

  step "4. Coordination bus (Valkey / Redis)"
  menu redis "Which Valkey/Redis?" \
    "bundled|Install Valkey with DYLARIS (recommended)" \
    "existing|Use an existing Redis 7+ / Valkey server with ACLs"
  if [[ ${A[redis]} == existing ]]; then
    ask redis_addr "Address (host:port)" ""
    ask redis_user "Admin user (needs ACL rights: DYLARIS creates a user per node)" "default"
    ask_secret redis_password "Password for ${A[redis_user]}"
    ask_yn redis_tls "Does it need TLS?" n
    check_redis
  else
    A[redis_addr]=redis:6379; A[redis_user]=default; A[redis_tls]=n
    A[redis_password]=$(env_get REDIS_PASSWORD); [[ -n ${A[redis_password]} ]] || A[redis_password]=$(gen_secret)
  fi

  step "5. Panel address"
  local ip; ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  ask host "Domain or IP your players and admins use for the panel" "${ip:-localhost}"
  menu tls "How is the panel reached?" \
    "self-signed|HTTPS from DYLARIS with a self-signed certificate (no proxy needed; the browser warns once)" \
    "proxy|Behind my own reverse proxy with a real certificate (nginx, Traefik, Caddy...)" \
    "files|HTTPS from DYLARIS with my own certificate files" \
    "http|Plain HTTP (only inside a private network)"
  case ${A[tls]} in
    self-signed) A[frontend]="https://${A[host]}:25443" ;;
    files)
      ask tls_cert "Path to the certificate (PEM, full chain)" ""
      ask tls_key "Path to the private key (PEM)" ""
      [[ -r ${A[tls_cert]} && -r ${A[tls_key]} ]] || die "Cannot read the certificate or key."
      A[frontend]="https://${A[host]}:25443" ;;
    proxy) A[frontend]="https://${A[host]}" ;;
    http)  A[frontend]="http://${A[host]}:25500" ;;
  esac

  step "6. Secrets"
  local k
  for k in JWT_SECRET BEAM_JWT_SECRET CLUSTER_SECRET; do
    A[$k]=$(env_get "$k"); [[ -n ${A[$k]} ]] || A[$k]=$(gen_secret)
  done
  if existing_env; then info "Kept the secrets of the existing installation."; fi
  while [[ -z $CONFIG_FILE ]]; do
    for k in JWT_SECRET BEAM_JWT_SECRET CLUSTER_SECRET; do say "  $k = $(mask "${A[$k]}")"; done
    menu regen "Keep these, or make new ones?" \
      "keep|Keep them" "JWT_SECRET|New JWT_SECRET (signs everyone out)" \
      "BEAM_JWT_SECRET|New BEAM_JWT_SECRET" "CLUSTER_SECRET|New CLUSTER_SECRET (nodes must re-pair)" \
      "all|New ones for all three"
    case ${A[regen]} in
      keep) break ;;
      all) for k in JWT_SECRET BEAM_JWT_SECRET CLUSTER_SECRET; do A[$k]=$(gen_secret); done ;;
      *) A[${A[regen]}]=$(gen_secret) ;;
    esac
  done

  step "7. Administrator"
  if [[ ${A[mode]} == start ]]; then
    menu admin "Create the first administrator now?" \
      "now|Yes, here in the terminal" "browser|No, I will open /setup in the browser"
    if [[ ${A[admin]} == now ]]; then
      ask admin_user "Username (3-32 characters)" "admin"
      ask_secret admin_password "Password (8 characters or more)"
      (( ${#A[admin_password]} >= 8 )) || die "The password needs 8 characters or more."
    fi
  fi
}

# Tested in a throwaway container: the DYLARIS containers will reach the
# server over the network the same way. The password goes through an env file,
# never the command line, where other users could read it.
check_postgres() {
  docker_cmd || { warn "Docker not ready yet; the connection is checked after Docker is installed."; A[db_check]=later; return; }
  info "Testing the database connection..."
  local envf; envf=$(mktemp); chmod 600 "$envf"
  printf 'PGPASSWORD=%s\nPGCONNECT_TIMEOUT=8\n' "${A[db_password]}" >"$envf"
  local conn="host=${A[db_host]} port=${A[db_port]} user=${A[db_user]} dbname=${A[db_name]} sslmode=${A[db_sslmode]}"
  local sql="CREATE TABLE IF NOT EXISTS dylaris_install_probe(x int); DROP TABLE dylaris_install_probe; SELECT 'ok';"
  if out=$("${DOCKER[@]}" run --rm --network host --env-file "$envf" "$PROBE_PG" psql "$conn" -tAc "$sql" 2>&1); then
    ok "Connected; ${A[db_user]} can create tables in ${A[db_name]}."
    if ! "${DOCKER[@]}" run --rm --network host --env-file "$envf" "$PROBE_PG" psql "$conn" -tAc "CREATE EXTENSION IF NOT EXISTS pgcrypto" >/dev/null 2>&1; then
      warn "${A[db_user]} cannot create the pgcrypto extension. Ask the database admin to run: CREATE EXTENSION pgcrypto; in ${A[db_name]}."
    fi
  else
    rm -f "$envf"
    fail "Could not use the database: $(tail -1 <<<"$out")"
    ask_yn db_provision "Create the database and user with an admin login (used once, not stored)?" n
    [[ ${A[db_provision]} == y ]] || die "Fix the database access and run the installer again."
    provision_postgres; check_postgres; return
  fi
  rm -f "$envf"
}

provision_postgres() {
  ask db_admin_user "Admin user (for example postgres)" "postgres"
  local pw; read -r -s -p "Password for ${A[db_admin_user]}: " pw </dev/tty; echo
  local envf; envf=$(mktemp); chmod 600 "$envf"
  printf 'PGPASSWORD=%s\n' "$pw" >"$envf"; pw=""
  local u=${A[db_user]} d=${A[db_name]} p=${A[db_password]//\'/\'\'}
  local sql="DO \$\$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='$u') THEN CREATE ROLE \"$u\" LOGIN PASSWORD '$p'; END IF; END\$\$;"
  run "${DOCKER[@]}" run --rm --network host --env-file "$envf" "$PROBE_PG" \
    psql "host=${A[db_host]} port=${A[db_port]} user=${A[db_admin_user]} dbname=postgres sslmode=${A[db_sslmode]}" -v ON_ERROR_STOP=1 -c "$sql"
  "${DOCKER[@]}" run --rm --network host --env-file "$envf" "$PROBE_PG" \
    psql "host=${A[db_host]} port=${A[db_port]} user=${A[db_admin_user]} dbname=postgres sslmode=${A[db_sslmode]}" -tAc \
    "SELECT 1 FROM pg_database WHERE datname='$d'" | grep -q 1 ||
    run "${DOCKER[@]}" run --rm --network host --env-file "$envf" "$PROBE_PG" \
      psql "host=${A[db_host]} port=${A[db_port]} user=${A[db_admin_user]} dbname=postgres sslmode=${A[db_sslmode]}" -c "CREATE DATABASE \"$d\" OWNER \"$u\""
  run "${DOCKER[@]}" run --rm --network host --env-file "$envf" "$PROBE_PG" \
    psql "host=${A[db_host]} port=${A[db_port]} user=${A[db_admin_user]} dbname=$d sslmode=${A[db_sslmode]}" -c "CREATE EXTENSION IF NOT EXISTS pgcrypto"
  rm -f "$envf"
  ok "Database ${A[db_name]} and user ${A[db_user]} are ready; the admin login was not stored."
}

check_redis() {
  docker_cmd || { warn "Docker not ready yet; checked after Docker is installed."; return; }
  info "Testing Valkey/Redis..."
  local host=${A[redis_addr]%:*} port=${A[redis_addr]##*:} envf
  [[ ${A[redis_password]} =~ [[:space:]] ]] && die "The Redis password must not contain spaces (it goes into an ACL file)."
  envf=$(mktemp); chmod 600 "$envf"; printf 'REDISCLI_AUTH=%s\n' "${A[redis_password]}" >"$envf"
  local cli=("${DOCKER[@]}" run --rm --network host --env-file "$envf" "$IMAGE_VALKEY" valkey-cli -h "$host" -p "$port" --user "${A[redis_user]}" --no-auth-warning)
  [[ ${A[redis_tls]} == y ]] && cli+=(--tls)
  local who ver probe="dylaris-install-probe-$$"
  who=$("${cli[@]}" ACL WHOAMI 2>&1) || { rm -f "$envf"; die "Could not log in: $who"; }
  ver=$("${cli[@]}" INFO server 2>/dev/null | tr -d '\r' | awk -F: '/^(redis|valkey)_version/ {print $2; exit}')
  [[ ${ver%%.*} =~ ^[0-9]+$ ]] && (( ${ver%%.*} >= 7 )) || { rm -f "$envf"; die "Version ${ver:-unknown}: ACL selectors need Redis 7 or newer (or Valkey)."; }
  if ! "${cli[@]}" ACL SETUSER "$probe" on ">x$$y" "~probe:*" +ping >/dev/null 2>&1 ||
     ! "${cli[@]}" ACL DELUSER "$probe" >/dev/null 2>&1; then
    rm -f "$envf"; die "${A[redis_user]} cannot manage ACL users; DYLARIS needs that to give every node its own login."
  fi
  rm -f "$envf"
  ok "Logged in as $who on version $ver, ACL management works."
  ask_yn redis_shared "Is this server also used by other software? DYLARIS will add and remove its own ACL users there. Continue?" y
  [[ ${A[redis_shared]} == y ]] || die "Stopped; nothing was changed."
}

# ------------------------------------------------------------ the files ----

write_env() {
  local f=$1
  cat >"$f" <<EOF
# Written by the DYLARIS installer $INSTALLER_VERSION on $(date -u +%Y-%m-%dT%H:%M:%SZ).
# Secrets: keep this file private (mode 600). A rerun of the installer keeps it.
JWT_SECRET=${A[JWT_SECRET]}
BEAM_JWT_SECRET=${A[BEAM_JWT_SECRET]}
CLUSTER_SECRET=${A[CLUSTER_SECRET]}
FRONTEND_URL=${A[frontend]}
DB_HOST=${A[db_host]}
DB_PORT=${A[db_port]}
DB_NAME=${A[db_name]}
DB_USER=${A[db_user]}
DB_PASSWORD=${A[db_password]}
DB_SSLMODE=${A[db_sslmode]}
DB_TYPE=$( [[ ${A[db_timescale]} == y ]] && echo timescaledb || echo postgres )
REDIS_ADDR=${A[redis_addr]}
REDIS_USER=${A[redis_user]}
REDIS_PASSWORD=${A[redis_password]}
PANEL_TLS=$( case ${A[tls]} in self-signed) echo self-signed ;; files) echo files ;; *) echo off ;; esac )
PANEL_TLS_HOSTS=${A[host]}
NODE_HOSTNAME=$(hostname -s 2>/dev/null || echo node-01)
EOF
}

# One generator for both targets. kind=compose: one machine, bridge network,
# secrets from .env. kind=stack: Docker Swarm, secrets as Docker secrets (the
# *_FILE variables Core, Node and Postgres read), the node service global so
# every machine that joins becomes a node, state pinned to the manager. The
# bundled database and Valkey are left out when an existing one was chosen,
# and their ports are never published.
write_compose() {
  local f=$1 kind=${2:-compose} pg=0 vk=0 sw=0
  [[ ${A[db]} == bundled ]] && pg=1
  [[ ${A[redis]} == bundled ]] && vk=1
  [[ $kind == stack ]] && sw=1
  sec() { # sec NAME - the env line for a secret in either form
    if (( sw )); then printf '      %s_FILE: "/run/secrets/%s"\n' "$1" "${1,,}"
    else printf '      %s: "${%s}"\n' "$1" "$1"; fi
  }
  secrets_of() { # secrets_of NAME... - a service's secrets list in a stack
    (( sw )) || return 0
    echo "    secrets:"; local n; for n in "$@"; do echo "      - ${n,,}"; done
  }
  manager() { (( sw )) && printf '    deploy:\n      placement:\n        constraints: [node.role == manager]\n'; return 0; }
  {
    echo "# Written by the DYLARIS installer $INSTALLER_VERSION. Values come from .env."
    (( sw )) || echo "name: $PROJECT"
    echo "services:"
    echo "  core:"
    echo "    image: $IMAGE_CORE:$IMAGE_TAG"
    if (( sw )); then
      manager
    else
      echo "    restart: unless-stopped"
      if (( pg || vk )); then
        echo "    depends_on:"
        (( pg )) && printf '      timescaledb:\n        condition: service_healthy\n'
        (( vk )) && printf '      redis:\n        condition: service_started\n'
      fi
    fi
    echo "    environment:"
    cat <<'EOF'
      API_PORT: "25500"
      FRONTEND_URL: "${FRONTEND_URL}"
      DB_HOST: "${DB_HOST}"
      DB_PORT: "${DB_PORT}"
      DB_NAME: "${DB_NAME}"
      DB_USER: "${DB_USER}"
      DB_SSLMODE: "${DB_SSLMODE}"
      DB_TYPE: "${DB_TYPE}"
      REDIS_ADDR: "${REDIS_ADDR}"
      REDIS_USER: "${REDIS_USER}"
      PANEL_TLS: "${PANEL_TLS}"
      PANEL_TLS_HOSTS: "${PANEL_TLS_HOSTS}"
EOF
    sec JWT_SECRET; sec BEAM_JWT_SECRET; sec CLUSTER_SECRET; sec DB_PASSWORD; sec REDIS_PASSWORD
    if [[ ${A[tls]} == files ]]; then
      echo '      PANEL_TLS_CERT_FILE: "/run/panel-tls/cert.pem"'
      echo '      PANEL_TLS_KEY_FILE: "/run/panel-tls/key.pem"'
    fi
    secrets_of JWT_SECRET BEAM_JWT_SECRET CLUSTER_SECRET DB_PASSWORD REDIS_PASSWORD
    cat <<'EOF'
    ports:
      - "25500:25500"
      - "25501:25501"
      - "25443:25443"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:25500/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 20s
    stop_grace_period: 90s
    networks: [dylaris_net]
    volumes:
      - core_data:/app/dylaris_data
EOF
    if [[ ${A[tls]} == files ]]; then
      echo "      - ${A[tls_cert]}:/run/panel-tls/cert.pem:ro"
      echo "      - ${A[tls_key]}:/run/panel-tls/key.pem:ro"
    fi
    echo "  node:"
    echo "    image: $IMAGE_NODE:$IMAGE_TAG"
    if (( sw )); then
      printf '    deploy:\n      mode: global\n'
    else
      echo "    restart: unless-stopped"
      (( vk )) && echo "    depends_on: [redis]"
    fi
    echo "    environment:"
    sec CLUSTER_SECRET; sec BEAM_JWT_SECRET
    echo '      CORE_GRPC_ADDR: "core:25501"'
    if (( sw )); then echo '      NODE_HOSTNAME: "{{.Node.Hostname}}"'; else echo '      NODE_HOSTNAME: "${NODE_HOSTNAME}"'; fi
    secrets_of CLUSTER_SECRET BEAM_JWT_SECRET
    cat <<'EOF'
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - dylaris_data:/app/dylaris_data
EOF
    # Host mode in a stack, so each machine's node owns its own ports instead
    # of the routing mesh; Compose knows only the short form.
    if (( sw )); then
      printf '    ports:
      - {target: 25520, published: 25520, mode: host}
      - {target: 25523, published: 25523, mode: host}
'
    else
      printf '    ports:
      - "25520:25520"
      - "25523:25523"
'
    fi
    echo "    networks: [dylaris_net]"
    if (( pg )); then
      echo "  timescaledb:"
      echo "    image: $IMAGE_PG"
      if (( sw )); then manager; else echo "    restart: unless-stopped"; fi
      echo "    environment:"
      echo '      POSTGRES_USER: "${DB_USER}"'
      echo '      POSTGRES_DB: "${DB_NAME}"'
      if (( sw )); then echo '      POSTGRES_PASSWORD_FILE: "/run/secrets/db_password"'; else echo '      POSTGRES_PASSWORD: "${DB_PASSWORD}"'; fi
      secrets_of DB_PASSWORD
      cat <<'EOF'
    volumes:
      - timescaledb_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER} -d $${POSTGRES_DB}"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 30s
    networks: [dylaris_net]
EOF
    fi
    if (( vk )); then
      echo "  redis:"
      echo "    image: $IMAGE_VALKEY"
      if (( sw )); then manager; else echo "    restart: unless-stopped"; fi
      echo '    command: ["valkey-server", "--save", "", "--appendonly", "no", "--aclfile", "/data/users.acl", "--maxmemory", "512mb", "--maxmemory-policy", "volatile-lru"]'
      echo "    volumes:"
      if (( sw )); then echo "      - $OUT_DIR/valkey-acl:/data"; else echo "      - ./valkey-acl:/data"; fi
      echo "    networks: [dylaris_net]"
    fi
    echo "volumes:"
    echo "  dylaris_data:"
    echo "  core_data:"
    (( pg )) && echo "  timescaledb_data:"
    echo "networks:"
    echo "  dylaris_net:"
    if (( sw )); then
      printf '    driver: overlay\n    attachable: true\n    driver_opts:\n      encrypted: "true"\n'
      echo "secrets:"
      local n; for n in JWT_SECRET BEAM_JWT_SECRET CLUSTER_SECRET DB_PASSWORD REDIS_PASSWORD; do
        printf '  %s:\n    external: true\n    name: %s_%s\n' "${n,,}" "$PROJECT" "${n,,}"
      done
    else
      echo "    driver: bridge"
    fi
  } >"$f"
}

write_files() {
  step "Writing the files to $OUT_DIR"
  run $SUDO install -d -m 0750 "$OUT_DIR"
  local tmp; tmp=$(mktemp -d)
  write_env "$tmp/.env"
  local cf=docker-compose.yml kind=compose
  [[ ${A[topology]} == swarm ]] && { cf=docker-stack.yml; kind=stack; }
  write_compose "$tmp/$cf" "$kind"
  if (( DRY_RUN )); then
    say "${D}--- $cf ---${N}"; cat "$tmp/$cf"
    say "${D}--- .env (secrets masked) ---${N}"; sed -E 's/^((JWT|BEAM_JWT|CLUSTER)_SECRET|DB_PASSWORD|REDIS_PASSWORD)=(.{4}).*/\1=\3.../' "$tmp/.env"
    rm -rf "$tmp"; return
  fi
  $SUDO install -m 0600 "$tmp/.env" "$OUT_DIR/.env"
  $SUDO install -m 0644 "$tmp/$cf" "$OUT_DIR/$cf"
  if [[ ${A[redis]} == bundled ]]; then
    $SUDO install -d -m 0700 "$OUT_DIR/valkey-acl"
    if ! $SUDO test -f "$OUT_DIR/valkey-acl/users.acl"; then
      printf 'user default on >%s ~* &* +@all\n' "${A[redis_password]}" | $SUDO tee "$OUT_DIR/valkey-acl/users.acl" >/dev/null
    fi
    $SUDO chmod 600 "$OUT_DIR/valkey-acl/users.acl"
    # The valkey image runs as uid 999 and must be able to ACL SAVE.
    $SUDO chown -R 999:999 "$OUT_DIR/valkey-acl"
  fi
  printf 'version=%s\ndate=%s\ntopology=%s\n' "$INSTALLER_VERSION" "$(date -u +%FT%TZ)" "${A[topology]}" |
    $SUDO tee "$OUT_DIR/.installer-state" >/dev/null
  rm -rf "$tmp"
  ok "Written: .env (600), $cf$( [[ ${A[redis]} == bundled ]] && echo ', valkey-acl/users.acl (600)')"
}

# -------------------------------------------------------------- start up ----

compose() { "${DOCKER[@]}" compose --project-directory "$OUT_DIR" -f "$OUT_DIR/docker-compose.yml" "$@"; }

start_stack() {
  step "Starting DYLARIS"
  run compose pull -q
  run compose up -d
  (( DRY_RUN )) && return
  info "Waiting for Core to answer (the first start creates the database, up to 3 minutes)..."
  local i
  for i in $(seq 1 90); do
    curl -fsS -o /dev/null http://127.0.0.1:25500/healthz 2>/dev/null && { ok "Core is up"; return; }
    sleep 2
  done
  compose logs --tail 40 core | sed -E 's/(PASSWORD|SECRET)=[^ ]+/\1=***/g' || true
  die "Core did not become healthy. The log above shows why; fix it and run: docker compose -f $OUT_DIR/docker-compose.yml up -d"
}

create_admin() {
  [[ ${A[admin]:-} == now ]] || return 0
  (( DRY_RUN )) && { say "${D}dry-run${N}  POST /api/setup/admin as ${A[admin_user]}"; return; }
  local st; st=$(curl -fsS http://127.0.0.1:25500/api/setup/status 2>/dev/null || true)
  if ! grep -q 'fresh_install' <<<"$st"; then
    info "An administrator already exists; skipped."; return
  fi
  local body; body=$(printf '{"username":"%s","password":"%s"}' "$(json_escape "${A[admin_user]}")" "$(json_escape "${A[admin_password]}")")
  local code out; out=$(mktemp)
  code=$(curl -sS -o "$out" -w '%{http_code}' -H 'Content-Type: application/json' --data-binary @- \
    http://127.0.0.1:25500/api/setup/admin <<<"$body" 2>/dev/null) || true
  if [[ $code == 200 || $code == 201 ]]; then
    ok "Administrator ${A[admin_user]} created"
    ADMIN_TOKEN=$(grep -o '"token":"[^"]*"' "$out" | head -1 | cut -d'"' -f4)
  else
    warn "Could not create the administrator (HTTP $code): $(head -c 300 "$out"). Open ${A[frontend]}/setup instead."
  fi
  rm -f "$out"
  A[admin_password]=""
}

ADMIN_TOKEN=""
verify() {
  (( DRY_RUN )) && return
  step "Checking the installation"
  curl -fsS -o /dev/null http://127.0.0.1:25500/healthz && ok "Core healthy (database and Valkey reachable)"
  if [[ ${A[tls]} == self-signed || ${A[tls]} == files ]]; then
    curl -fsSk -o /dev/null https://127.0.0.1:25443/healthz && ok "Panel HTTPS answers on 25443" || warn "Panel HTTPS does not answer on 25443"
  fi
  if [[ -n $ADMIN_TOKEN ]]; then
    local i nodes=""
    for i in $(seq 1 30); do
      nodes=$(curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" http://127.0.0.1:25500/api/nodes 2>/dev/null || true)
      grep -q '"status":"online"' <<<"$nodes" && { ok "The node is online"; break; }
      sleep 3
    done
    grep -q '"status":"online"' <<<"$nodes" || warn "The node is not online yet; check: docker compose -f $OUT_DIR/docker-compose.yml logs node"
  fi
}

summary_done() {
  step "Done"
  say "  Panel:      ${B}${A[frontend]}${N}"
  [[ ${A[tls]} == self-signed ]] && say "              The browser warns about the self-signed certificate once; that is expected."
  [[ ${A[admin]:-} == browser ]] && say "  First step: open ${A[frontend]}/setup and create the administrator."
  [[ ${A[tls]} == proxy ]] && say "  Proxy:      point https://${A[host]} at http://127.0.0.1:25500 (WebSocket upgrade on)."
  say "  Files:      $OUT_DIR (.env holds the secrets, mode 600)"
  say "  Data:       Docker volumes ${PROJECT}_dylaris_data, ${PROJECT}_core_data$( [[ ${A[db]} == bundled ]] && echo ", ${PROJECT}_timescaledb_data")"
  say "  Firewall:   open 25500/25443 (panel) and 25600-25699 (Minecraft servers, direct routing); keep 25501 private."
  say "  Backups:    set a backup target in the panel (Settings > Backups) before people play."
  say "  Update:     bash install.sh update     Remove: bash install.sh uninstall"
}

plan_and_confirm() {
  step "8. Plan"
  [[ ${A[need_docker]:-} == y ]] && say "  - Install Docker"
  say "  - Write $OUT_DIR/.env and docker-compose.yml"
  say "  - Topology: ${A[topology]}; PostgreSQL: ${A[db]}; Valkey: ${A[redis]}; panel: ${A[frontend]} (${A[tls]})"
  [[ ${A[mode]} == start ]] && say "  - Pull the images and start: core, node$( [[ ${A[db]} == bundled ]] && echo ', timescaledb')$( [[ ${A[redis]} == bundled ]] && echo ', redis')"
  [[ ${A[admin]:-} == now ]] && say "  - Create the administrator ${A[admin_user]}"
  existing_env && say "  - ${Y}An installation exists in $OUT_DIR: its secrets and data are kept${N}"
  say ""
  (( DRY_RUN )) && { info "Dry run: nothing is changed."; return; }
  ask_yn go "Go ahead?" y
  [[ ${A[go]} == y ]] || die "Stopped; nothing was changed."
}

# ------------------------------------------------------------------ swarm ----

swarm_install() {
  step "Docker Swarm"
  local state; state=$("${DOCKER[@]}" info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo inactive)
  if [[ $state == active ]]; then
    "${DOCKER[@]}" info --format '{{.Swarm.ControlAvailable}}' | grep -q true || die "This machine is a Swarm worker; run the installer on a manager."
    ask_yn swarm_use "This machine already manages a Swarm. Deploy DYLARIS into it?" n
    [[ ${A[swarm_use]} == y ]] || die "Stopped; the Swarm was not touched."
  else
    local ip; ip=$(hostname -I 2>/dev/null | awk '{print $1}')
    ask swarm_addr "Address the other machines reach this one on (advertise address)" "$ip"
    ask_yn swarm_init "Initialise a new Swarm on this machine?" y
    [[ ${A[swarm_init]} == y ]] || die "Stopped."
    run "${DOCKER[@]}" swarm init --advertise-addr "${A[swarm_addr]}"
  fi
  write_files
  # Secrets go into Docker's encrypted store once. An existing one is kept:
  # a Swarm secret cannot be changed in place, and the running stack holds it.
  local n v
  for n in JWT_SECRET BEAM_JWT_SECRET CLUSTER_SECRET DB_PASSWORD REDIS_PASSWORD; do
    case $n in DB_PASSWORD) v=${A[db_password]} ;; REDIS_PASSWORD) v=${A[redis_password]} ;; *) v=${A[$n]} ;; esac
    if "${DOCKER[@]}" secret inspect "${PROJECT}_${n,,}" >/dev/null 2>&1; then
      info "Secret ${PROJECT}_${n,,} exists; kept."
    elif (( DRY_RUN )); then
      say "${D}dry-run${N}  docker secret create ${PROJECT}_${n,,}"
    else
      printf '%s' "$v" | "${DOCKER[@]}" secret create "${PROJECT}_${n,,}" - >/dev/null
    fi
  done
  run sh -c "cd '$OUT_DIR' && set -a && . ./.env && set +a && ${DOCKER[*]} stack deploy -c docker-stack.yml $PROJECT"
  (( DRY_RUN )) && return
  info "Waiting for Core to answer..."
  local i; for i in $(seq 1 90); do
    curl -fsS -o /dev/null http://127.0.0.1:25500/healthz 2>/dev/null && { ok "Core is up"; break; }
    sleep 2
  done
  create_admin
  verify
  say "  Add a machine: on it run ${B}bash install.sh add-node${N}; it needs this manager's address and"
  say "  the worker join token (a secret): docker swarm join-token -q worker"
}

cmd_add_node() {
  step "Add this machine to an existing DYLARIS Swarm"
  preflight || exit 1
  [[ ${A[need_docker]:-} == y ]] && install_docker
  [[ ${A[memcg_off]:-} == y ]] && fix_pi_memcg
  local state; state=$("${DOCKER[@]}" info --format '{{.Swarm.LocalNodeState}}' 2>/dev/null || echo inactive)
  [[ $state == active ]] && { ok "This machine is already in a Swarm; the node service runs on it automatically."; return; }
  ask manager "Manager address (host:2377)" ""
  ask_secret join_token "Worker join token (on the manager: docker swarm join-token -q worker)"
  run "${DOCKER[@]}" swarm join --token "${A[join_token]}" "${A[manager]}"
  ok "Joined. The DYLARIS node starts here by itself and shows up in the panel within a minute."
}

# ------------------------------------------------------- other commands ----

cmd_update() {
  step "Updating DYLARIS in $OUT_DIR"
  docker_cmd || die "Docker is not reachable."
  [[ -f $OUT_DIR/docker-compose.yml ]] || die "No installation in $OUT_DIR."
  if [[ -f $OUT_DIR/docker-stack.yml ]]; then
    run sh -c "cd '$OUT_DIR' && set -a && . ./.env && set +a && ${DOCKER[*]} stack deploy --with-registry-auth -c docker-stack.yml $PROJECT"
  else
    run compose pull -q
    run compose up -d
  fi
  ok "Updated; secrets and data were kept."
}

cmd_uninstall() {
  step "Removing DYLARIS from this machine"
  docker_cmd || die "Docker is not reachable."
  if (( PURGE )); then
    warn "This deletes every server, world, backup in the volumes, the database and the secrets in $OUT_DIR."
    local t; read -r -p "Type DELETE EVERYTHING to continue: " t </dev/tty
    [[ $t == "DELETE EVERYTHING" ]] || die "Stopped; nothing was deleted."
  fi
  if [[ -f $OUT_DIR/docker-stack.yml ]]; then run "${DOCKER[@]}" stack rm "$PROJECT"
  elif [[ -f $OUT_DIR/docker-compose.yml ]]; then
    if (( PURGE )); then run compose down -v; else run compose down; fi
  fi
  # Game servers are siblings the node started, not part of the compose project.
  local c; for c in $("${DOCKER[@]}" ps -aq --filter name=mc_); do run "${DOCKER[@]}" rm -f "$c" >/dev/null; done
  if (( PURGE )); then
    run $SUDO rm -rf "$OUT_DIR"
    ok "Removed, including all data."
  else
    ok "Containers removed. Data volumes and $OUT_DIR were kept; install again to continue, or uninstall --purge to delete them."
  fi
}

cmd_diagnostics() {
  docker_cmd || die "Docker is not reachable."
  local f; f="/tmp/dylaris-diagnostics-$(date +%Y%m%d-%H%M%S).txt"
  local secrets=()
  if [[ -f $OUT_DIR/.env ]]; then
    mapfile -t secrets < <($SUDO awk -F= '/(SECRET|PASSWORD)=/ { sub(/^[^=]*=/, ""); if (length($0) > 3) print }' "$OUT_DIR/.env")
  fi
  {
    echo "installer $INSTALLER_VERSION, $(date -u)"; uname -a; cat /etc/os-release 2>/dev/null | head -3
    "${DOCKER[@]}" version --format '{{.Server.Version}}' 2>/dev/null
    "${DOCKER[@]}" ps -a --format '{{.Names}}  {{.Image}}  {{.Status}}' | grep -E "^${PROJECT}|mc_" || true
    for s in core node timescaledb redis; do echo "--- $s"; "${DOCKER[@]}" logs --tail 60 "${PROJECT}-$s-1" 2>&1 || true; done
  } >"$f.raw" 2>&1
  local s; for s in "${secrets[@]}"; do sed -i "s|$s|***|g" "$f.raw"; done
  sed -E 's/((SECRET|PASSWORD|TOKEN)[=: ]+)[^ ]+/\1***/Ig' "$f.raw" >"$f"; rm -f "$f.raw"
  ok "Wrote $f (secrets masked). Read it before you share it."
}

cmd_install() {
  preflight || { (( CHECK_ONLY )) && exit 1; exit 1; }
  (( CHECK_ONLY )) && exit 0
  [[ ${A[need_docker]:-} == y ]] && {
    ask_yn docker_ok "Install Docker now?" y
    [[ ${A[docker_ok]} == y ]] || die "DYLARIS needs Docker."
    install_docker
  }
  [[ ${A[memcg_off]:-} == y ]] && fix_pi_memcg
  existing_env && info "Found an existing installation in $OUT_DIR; its secrets and data stay."
  interview
  [[ ${A[db_check]:-} == later ]] && check_postgres
  plan_and_confirm
  if [[ ${A[topology]} == swarm ]]; then swarm_install; summary_done; return; fi
  write_files
  [[ ${A[mode]} == start ]] || { summary_done; say "  Start:      cd $OUT_DIR && docker compose up -d"; return; }
  start_stack
  create_admin
  verify
  summary_done
}

usage() {
  cat <<EOF
DYLARIS installer $INSTALLER_VERSION

  bash install.sh [command] [options]

Commands:
  install       interview, write the files, optionally start (default)
  add-node      join this machine to a DYLARIS Swarm as a node
  update        pull new images and restart, keeping secrets and data
  uninstall     remove the containers; --purge also deletes all data
  diagnostics   write a redacted report for support

Options:
  --config FILE      answers from a KEY=VALUE file (non-interactive)
  --check            only check this machine
  --dry-run          ask everything, show the plan and files, change nothing
  --output-dir DIR   where the files go (default $OUT_DIR)
  --purge            with uninstall: delete data too
  --version          print the version
EOF
}

main() {
  local orig=("$@")
  while (( $# )); do
    case $1 in
      install|add-node|update|uninstall|diagnostics) CMD=$1 ;;
      --config) CONFIG_FILE=${2:?}; shift ;;
      --check) CHECK_ONLY=1 ;;
      --dry-run) DRY_RUN=1 ;;
      --output-dir) OUT_DIR=${2:?}; shift ;;
      --purge) PURGE=1 ;;
      --version) echo "$INSTALLER_VERSION"; exit 0 ;;
      -h|--help) usage; exit 0 ;;
      *) usage; die "Unknown argument: $1" ;;
    esac
    shift
  done
  [[ -n $CONFIG_FILE ]] && { [[ -r $CONFIG_FILE ]] || die "Cannot read $CONFIG_FILE"; load_config; }
  # Root from the start: it writes /opt, talks to Docker and installs
  # packages. Asking sudo once here beats a password prompt halfway through.
  if [[ $EUID -ne 0 ]]; then
    command -v sudo >/dev/null || die "Run this as root (sudo is not installed)."
    say "The installer needs root; asking sudo..."
    exec sudo -E bash "$0" "${orig[@]}"
  fi
  say "${B}${V}DYLARIS${N}${B} installer $INSTALLER_VERSION${N}  ${D}(the Gateway is a managed service and is not installed here)${N}"
  case $CMD in
    install) cmd_install ;;
    add-node) cmd_add_node ;;
    update) cmd_update ;;
    uninstall) cmd_uninstall ;;
    diagnostics) cmd_diagnostics ;;
  esac
}

main "$@"
