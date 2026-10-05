#!/usr/bin/env bash
# Tests of the pure functions of scripts/release.sh. The flow around them
# (git, gh, prompts) is exercised with `scripts/release.sh --dry-run`.
#
#   bash scripts/release_test.sh

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/release.sh
. "$here/release.sh"

failed=0

# check <name> <want> <got>
check() {
  if [ "$2" != "$3" ]; then
    printf 'FAIL %s\n  want: %s\n  got:  %s\n' "$1" "$2" "$3"
    failed=1
  fi
}

# ok <name> <command...>: the command must succeed; no <name>: it must fail.
ok() {
  local name=$1
  shift
  "$@" || { echo "FAIL $name: expected success"; failed=1; }
}
no() {
  local name=$1
  shift
  if "$@"; then echo "FAIL $name: expected failure"; failed=1; fi
}

# suggest <version> <subject>...: the suggested bump for these commit subjects.
suggest() {
  local v=$1
  shift
  printf '%s\n' "$@" | suggest_bump "$v"
}

# classify
check classify-feat feat "$(classify 'feat(web): add a page')"
check classify-feat-no-scope feat "$(classify 'feat: add a page')"
check classify-fix fix "$(classify 'fix(engine): stop the loop')"
check classify-breaking breaking "$(classify 'feat(config)!: rename -git-url')"
check classify-breaking-fix breaking "$(classify 'fix!: drop a column')"
check classify-change change "$(classify 'refactor(store): split queries')"
check classify-perf change "$(classify 'perf: cache the plan')"
check classify-docs other "$(classify 'docs: explain releases')"
check classify-build other "$(classify 'build(deps): bump x from 1 to 2 (#43)')"
check classify-free-text other "$(classify 'Merge branch main')"
check classify-featx other "$(classify 'feature: not a feat')"

# suggest_bump at v0: a minor may break, a patch never does
check v0-fix patch "$(suggest v0.1.0 'fix(engine): a')"
check v0-only-build patch "$(suggest v0.1.0 'build(deps): bump x' 'ci: pin y')"
check v0-docs-and-fix patch "$(suggest v0.1.0 'docs: a' 'fix: b' 'chore: c')"
check v0-feat minor "$(suggest v0.1.0 'fix: a' 'feat(web): b')"
check v0-breaking minor "$(suggest v0.1.0 'feat(config)!: a')"
check v0-refactor minor "$(suggest v0.1.0 'refactor: a')"
check v0-free-text patch "$(suggest v0.1.0 'something else')"
check v0-empty patch "$(suggest v0.1.0)"

# suggest_bump from v1: plain semver
check v1-fix patch "$(suggest v1.2.3 'fix: a')"
check v1-refactor patch "$(suggest v1.2.3 'refactor: a' 'docs: b')"
check v1-feat minor "$(suggest v1.2.3 'fix: a' 'feat: b')"
check v1-breaking major "$(suggest v1.2.3 'feat: a' 'fix(x)!: b')"
check v2-breaking major "$(suggest v2.0.0 'docs!: a')"

# bump_version
check bump-patch v0.1.1 "$(bump_version v0.1.0 patch)"
check bump-minor v0.2.0 "$(bump_version v0.1.4 minor)"
check bump-major v1.0.0 "$(bump_version v0.9.9 major)"
check bump-carry v1.3.0 "$(bump_version v1.2.7 minor)"
check bump-ignores-rc v0.2.1 "$(bump_version v0.2.0-rc.1 patch)"
no bump-unknown bump_version v0.1.0 huge 2>/dev/null

# next_rc
check rc-first v0.2.0-rc.1 "$(printf '' | next_rc v0.2.0)"
check rc-next v0.2.0-rc.3 "$(printf 'v0.2.0-rc.1\nv0.2.0-rc.2\n' | next_rc v0.2.0)"
check rc-gap v0.2.0-rc.11 "$(printf 'v0.2.0-rc.10\nv0.2.0-rc.2\n' | next_rc v0.2.0)"
check rc-other-version v0.2.0-rc.1 "$(printf 'v0.3.0-rc.4\nv0.1.0\n' | next_rc v0.2.0)"

# is_semver
ok semver-plain is_semver v0.2.0
ok semver-rc is_semver v1.0.0-rc.1
no semver-no-v is_semver 0.2.0
no semver-two-parts is_semver v0.2
no semver-words is_semver vlatest
no semver-trailing is_semver 'v0.2.0 '

# semver_gt
ok gt-patch semver_gt v0.1.1 v0.1.0
ok gt-minor semver_gt v0.2.0 v0.1.9
ok gt-numeric semver_gt v0.10.0 v0.9.0
ok gt-final-over-rc semver_gt v0.2.0 v0.2.0-rc.3
ok gt-rc-order semver_gt v0.2.0-rc.10 v0.2.0-rc.9
ok gt-rc-of-next-minor semver_gt v0.2.0-rc.1 v0.1.0
no gt-equal semver_gt v0.1.0 v0.1.0
no gt-lower semver_gt v0.1.0 v0.1.1
no gt-rc-under-final semver_gt v0.2.0-rc.1 v0.2.0
no gt-rc-order-reverse semver_gt v0.2.0-rc.2 v0.2.0-rc.10

# minor_of
check minor-of-final 0.6 "$(minor_of v0.6.2)"
check minor-of-rc 0.7 "$(minor_of v0.7.0-rc.1)"
check minor-of-major 1.0 "$(minor_of v1.0.0)"

# stale_image_tags
page='docker run --rm ghcr.io/music-gang/nops:0.6 -version
      image = "ghcr.io/music-gang/nops:0.6"
Pin a minor or an exact version.'
check stale-current '' "$(printf '%s\n' "$page" | stale_image_tags 0.6)"
check stale-old-minor '      image = "ghcr.io/music-gang/nops:0.4"' \
  "$(printf '%s\n' "$page" | sed 's/0\.6"/0.4"/' | stale_image_tags 0.6)"
check stale-exact 'run ghcr.io/music-gang/nops:0.6.0 -version' \
  "$(printf 'run ghcr.io/music-gang/nops:0.6.0 -version\n' | stale_image_tags 0.6)"
check stale-new-minor 'run ghcr.io/music-gang/nops:0.6 -version' \
  "$(printf 'run ghcr.io/music-gang/nops:0.6 -version\n' | stale_image_tags 0.7)"
check stale-no-final-newline 'ghcr.io/music-gang/nops:0.5' \
  "$(printf 'ghcr.io/music-gang/nops:0.5' | stale_image_tags 0.6)"
check stale-other-image '' "$(printf 'ghcr.io/other/app:0.1\n' | stale_image_tags 0.6)"

# upgrade_notes <record>...: the notes of these `%h<TAB>%s<NL>%b` records.
notes() {
  local r
  for r in "$@"; do printf '%s\0' "$r"; done | upgrade_notes 2>/dev/null
}
tab=$(printf '\t')
serve="a068300${tab}feat(cli)!: run the server as nops serve (#126)
The nops binary has a client.

BREAKING CHANGE: the server runs as \"nops serve\": put \"serve\" before its
flags. The image runs it with no arguments.

Closes #118
"

check notes-none '' "$(notes "abc${tab}feat(web): add a page (#1)
BREAKING CHANGE: not a breaking commit")"
check notes-empty '' "$(notes)"
check notes-footer '## Upgrade notes

- the server runs as "nops serve": put "serve" before its flags. The image runs it with no arguments. (#126)' \
  "$(notes "$serve")"
check notes-dash-spelling '## Upgrade notes

- rename -x to -y (#2)' \
  "$(notes "b${tab}fix(config)!: rename a flag (#2)

BREAKING-CHANGE: rename -x to -y")"
check notes-order '## Upgrade notes

- first (#1)
- second (#2)' \
  "$(notes "a${tab}feat!: a (#1)

BREAKING CHANGE: first" "b${tab}fix!: b (#2)

BREAKING CHANGE: second")"
check notes-no-number '## Upgrade notes

- drop a column' \
  "$(notes "a${tab}fix!: drop a column

BREAKING CHANGE: drop a column")"
check notes-no-footer '## Upgrade notes

- feat(web)!: rename a page (#3)' \
  "$(notes "a${tab}feat(web)!: rename a page (#3)
The body has no footer.")"
check notes-no-body '## Upgrade notes

- feat!: a (#4)' "$(notes "a${tab}feat!: a (#4)")"
check notes-final-record-no-nul '## Upgrade notes

- x (#5)' \
  "$(printf 'a\tfeat!: a (#5)\n\nBREAKING CHANGE: x' | upgrade_notes)"
check notes-warning 'warning: a feat!: a (#4) has no BREAKING CHANGE footer: its subject is the note, edit the release' \
  "$(printf 'a\tfeat!: a (#4)\n\0' | upgrade_notes 2>&1 >/dev/null)"

if [ "$failed" = 0 ]; then
  echo "release_test: ok"
fi
exit "$failed"
