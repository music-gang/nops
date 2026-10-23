#!/usr/bin/env bash
# Tests of the state scripts/dev.sh prepares in .dev/. Starting Nomad and
# Nops is exercised by running `scripts/dev.sh`.
#
#   bash scripts/dev_test.sh

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/dev.sh
. "$here/dev.sh"

failed=0

# check <name> <want> <got>
check() {
  if [ "$2" != "$3" ]; then
    printf 'FAIL %s\n  want: %s\n  got:  %s\n' "$1" "$2" "$3"
    failed=1
  fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# A stand-in for `nops secret generate`.
printf '#!/bin/sh\necho nops_stub\n' >"$tmp/nops"
chmod +x "$tmp/nops"
mkdir "$tmp/examples"
echo 'job "web" {}' >"$tmp/examples/web.nomad.hcl"
echo 'count = 1' >"$tmp/examples/web.vars.hcl"
dir=$tmp/state

setup_state "$dir" "$tmp/nops" "$tmp/examples" || { echo "FAIL setup: exit $?"; failed=1; }

check users "$admin_user" "$(cat "$dir/users")"
check groups 'admin: admin' "$(cat "$dir/groups")"
check token nops_stub "$(cat "$dir/bootstrap.token")"
check token-mode "$dir/bootstrap.token" "$(find "$dir/bootstrap.token" -perm 600)"
check jobs-files 'web.nomad.hcl
web.vars.hcl' "$(git -C "$dir/jobs" ls-files)"
check jobs-branch main "$(git -C "$dir/jobs" branch --show-current)"
check jobs-clean '' "$(git -C "$dir/jobs" status --porcelain)"

# A second start keeps what is there.
echo nops_kept >"$dir/bootstrap.token"
echo 'admin:kept' >"$dir/users"
setup_state "$dir" "$tmp/nops" "$tmp/examples" || { echo "FAIL rerun: exit $?"; failed=1; }
check rerun-token nops_kept "$(cat "$dir/bootstrap.token")"
check rerun-users admin:kept "$(cat "$dir/users")"
check rerun-commits 1 "$(git -C "$dir/jobs" rev-list --count HEAD)"

# paint: plain text off a terminal, so logs and pipes get no escape codes.
check paint-pipe 'dev: x' "$(paint 1 31 'dev: x')"

if [ "$failed" = 0 ]; then
  echo "dev_test: ok"
fi
exit "$failed"
