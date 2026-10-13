#!/usr/bin/env bash
# Run Nomad and Nops on this machine: a throwaway Nomad dev agent and a Nops
# built from this checkout, with its state in .dev/. Ctrl-C stops the jobs on
# that agent, then both.
#
#   scripts/dev.sh [--reset] [nops serve flags...]
#
# --reset deletes .dev/ first. The repository of jobs is .dev/jobs, made from
# examples/approval; NOPS_DEV_GIT_URL replaces it. The dashboard is on
# http://127.0.0.1:8080, login admin/admin; Nomad on http://127.0.0.1:14646.
# The jobs' ports are on the address of the default route, so the network
# there reaches them.
#
# Needs go, nomad, git, curl and Docker running.
# Written for bash 3.2 (macOS): no wait -n.

nomad_addr=http://127.0.0.1:14646
nops_addr=http://127.0.0.1:8080
# The password is admin: the dashboard listens only on this machine. A bcrypt
# hash, not an expansion.
# shellcheck disable=SC2016
admin_user='admin:$2y$10$T7p5EbzmlqY8Tqz79Ee8oOANA0MXxFPIIouW32DajYFEDnnVyGFlO'

# ---- state (tested by scripts/dev_test.sh) -----------------------------------

# setup_state <dir> <nops binary> <examples dir>: what Nops needs in <dir>.
# A file already there is kept, so a restart finds the same token and jobs.
setup_state() {
  local dir=$1 nops=$2 examples=$3
  mkdir -p "$dir"
  [ -f "$dir/users" ] || echo "$admin_user" >"$dir/users"
  [ -f "$dir/groups" ] || echo "admin: admin" >"$dir/groups"
  if [ ! -f "$dir/bootstrap.token" ]; then
    (umask 077 && "$nops" secret generate >"$dir/bootstrap.token")
  fi
  if [ ! -d "$dir/jobs/.git" ]; then
    mkdir -p "$dir/jobs"
    cp "$examples"/* "$dir/jobs/"
    git -C "$dir/jobs" init -q -b main
    git -C "$dir/jobs" add .
    git -C "$dir/jobs" -c user.name=nops-dev -c user.email=nops-dev@localhost \
      -c commit.gpgsign=false commit -q -m "jobs from $(basename "$examples")"
  fi
}

# ---- flow --------------------------------------------------------------------

die() {
  echo "dev: $*" >&2
  exit 1
}

usage() {
  sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
}

# port_free <port>: nothing listens on it, so the agent we reach is ours.
port_free() {
  local code=0
  curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$1" || code=$?
  [ "$code" = 7 ]
}

# default_interface: the network interface of the default route. Nomad gives
# the jobs' ports its address: on the loopback a hook's container could not
# reach the service it checks.
default_interface() {
  if [ "$(uname)" = Darwin ]; then
    route -n get default 2>/dev/null | awk '/interface:/ { print $2 }'
  else
    ip route show default 2>/dev/null | awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }'
  fi
}

# stop_jobs: stop every job on our agent and wait for its allocations to stop,
# so no container outlives the agent.
stop_jobs() {
  local job i
  for job in $(nomad job status -address="$nomad_addr" 2>/dev/null | awk 'NR > 1 && $1 != "" { print $1 }'); do
    nomad job stop -address="$nomad_addr" -detach "$job" >/dev/null || echo "dev: could not stop job $job" >&2
  done
  for i in $(seq 30); do
    curl -s "$nomad_addr/v1/allocations" | grep -qE '"ClientStatus":"(running|pending)"' || return 0
    sleep 1
  done
  echo "dev: allocations still running after 30s: see docker ps" >&2
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$nops_pid" ]; then
    kill -TERM "$nops_pid" 2>/dev/null
    wait "$nops_pid" 2>/dev/null
  fi
  if [ -n "$nomad_pid" ]; then
    if kill -0 "$nomad_pid" 2>/dev/null; then
      echo "dev: stopping the jobs and Nomad"
      stop_jobs
      kill -INT "$nomad_pid" 2>/dev/null
    fi
    wait "$nomad_pid" 2>/dev/null
  fi
  exit "$status"
}

main() {
  set -euo pipefail
  local reset=0 root dir i var iface
  case ${1:-} in
    --reset)
      reset=1
      shift
      ;;
    -h | --help)
      usage
      return 0
      ;;
  esac

  for i in go nomad git curl docker; do
    command -v "$i" >/dev/null || die "$i is not installed"
  done
  docker info >/dev/null 2>&1 || die "Docker is not running: the example jobs need it"
  for i in 14646 14647 14648 8080; do
    port_free "$i" || die "port $i is in use: stop what listens on it"
  done
  iface=$(default_interface)
  [ -n "$iface" ] || die "no default route: the hooks reach the jobs on its interface, connect to a network"

  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
  dir=$root/.dev
  if [ "$reset" = 1 ]; then
    rm -rf "$dir"
  fi

  # No Nomad or Nops setting of this shell reaches the two processes: they
  # could point at a real cluster or send real notifications.
  for var in $(compgen -e); do
    case $var in
      NOPS_DEV_*) ;;
      NOMAD_* | NOPS_*) unset "$var" ;;
    esac
  done

  echo "dev: building nops"
  mkdir -p "$dir/bin"
  go -C "$root" build -o "$dir/bin/nops" ./cmd/nops
  setup_state "$dir" "$dir/bin/nops" "$root/examples/approval"
  cat >"$dir/nomad.hcl" <<'EOF'
ports {
  http = 14646
  rpc  = 14647
  serf = 14648
}
EOF

  nomad_pid='' nops_pid=''
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # The two children start in their own process group (set -m): Ctrl-C
  # reaches only this script, which stops the jobs before Nomad.
  echo "dev: starting Nomad, log in $dir/nomad.log"
  set -m
  nomad agent -dev -network-interface="$iface" -config="$dir/nomad.hcl" >"$dir/nomad.log" 2>&1 &
  nomad_pid=$!
  set +m
  for i in $(seq 30); do
    curl -sf -o /dev/null "$nomad_addr/v1/agent/health" && break
    kill -0 "$nomad_pid" 2>/dev/null || die "Nomad exited: see $dir/nomad.log"
    sleep 1
  done
  curl -sf -o /dev/null "$nomad_addr/v1/agent/health" || die "Nomad is not healthy after 30s: see $dir/nomad.log"

  set -m
  "$dir/bin/nops" serve \
    -nomad-addr="$nomad_addr" \
    -nomad-ui-url="$nomad_addr" \
    -git-url="${NOPS_DEV_GIT_URL:-file://$dir/jobs}" \
    -git-poll-interval=10s \
    -listen-addr=127.0.0.1:8080 \
    -auth-mode=basic \
    -users-file="$dir/users" \
    -groups-file="$dir/groups" \
    -acl \
    -acl-bootstrap-token-file="$dir/bootstrap.token" \
    -db-path="$dir/nops.db" \
    "$@" &
  nops_pid=$!
  set +m

  cat <<EOF
dev: dashboard  $nops_addr  (admin / admin)
dev: Nomad UI   $nomad_addr
dev: jobs       ${NOPS_DEV_GIT_URL:-$dir/jobs}
dev: CLI        export NOPS_ADDR=$nops_addr NOPS_TOKEN_FILE=$dir/bootstrap.token
dev: Ctrl-C stops both
EOF

  while kill -0 "$nomad_pid" 2>/dev/null && kill -0 "$nops_pid" 2>/dev/null; do
    sleep 1
  done
  if kill -0 "$nops_pid" 2>/dev/null; then
    die "Nomad exited: see $dir/nomad.log"
  fi
  die "nops exited"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
