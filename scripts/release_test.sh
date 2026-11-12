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

# paint: plain text off a terminal, so CI logs and pipes get no escape codes.
check paint-pipe 'warning: x' "$(paint 1 33 'warning: x')"
check paint-pipe-stderr 'warning: x' "$(paint 2 33 'warning: x' 2>&1)"

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

# breaking_changes <record>...: the notes of these `%h<TAB>%s<NL>%b` records.
notes() {
  local r
  for r in "$@"; do printf '%s\0' "$r"; done | breaking_changes 2>/dev/null
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
check notes-footer '## Breaking changes

- the server runs as "nops serve": put "serve" before its flags. The image runs it with no arguments. (#126)' \
  "$(notes "$serve")"
check notes-dash-spelling '## Breaking changes

- rename -x to -y (#2)' \
  "$(notes "b${tab}fix(config)!: rename a flag (#2)

BREAKING-CHANGE: rename -x to -y")"
check notes-order '## Breaking changes

- first (#1)
- second (#2)' \
  "$(notes "a${tab}feat!: a (#1)

BREAKING CHANGE: first" "b${tab}fix!: b (#2)

BREAKING CHANGE: second")"
check notes-no-number '## Breaking changes

- drop a column' \
  "$(notes "a${tab}fix!: drop a column

BREAKING CHANGE: drop a column")"
check notes-no-footer '## Breaking changes

- feat(web)!: rename a page (#3)' \
  "$(notes "a${tab}feat(web)!: rename a page (#3)
The body has no footer.")"
check notes-no-body '## Breaking changes

- feat!: a (#4)' "$(notes "a${tab}feat!: a (#4)")"
check notes-final-record-no-nul '## Breaking changes

- x (#5)' \
  "$(printf 'a\tfeat!: a (#5)\n\nBREAKING CHANGE: x' | breaking_changes)"
check notes-warning 'warning: a feat!: a (#4) has no BREAKING CHANGE footer: its subject is the note, edit the release' \
  "$(printf 'a\tfeat!: a (#4)\n\0' | breaking_changes 2>&1 >/dev/null)"

# changelog_section
changelog='# Changelog

Intro.

## v0.3.0

### Breaking changes

- rename a flag

### Features

- add a page

## v0.2.0

### Fixes

- stop a loop
'
check section-first '### Breaking changes

- rename a flag

### Features

- add a page' "$(printf '%s' "$changelog" | changelog_section v0.3.0)"
check section-last '### Fixes

- stop a loop' "$(printf '%s' "$changelog" | changelog_section v0.2.0)"
check section-missing '' "$(printf '%s' "$changelog" | changelog_section v0.1.0)"
check section-exact-heading '' "$(printf '%s' "$changelog" | changelog_section v0.3)"
check section-no-final-newline '- x' "$(printf '## v1.0.0\n\n- x' | changelog_section v1.0.0)"
check section-empty '' "$(printf '## v1.0.0\n\n## v0.9.0\n- x\n' | changelog_section v1.0.0)"

# changelog_version
check version-first v0.3.0 "$(printf '%s' "$changelog" | changelog_version)"
check version-skips-intro v1.0.0 "$(printf '# Changelog\n\n## v1.0.0\n\n## v0.9.0\n' | changelog_version)"
check version-none '' "$(printf '# Changelog\n\nIntro.\n\n### Fixes\n' | changelog_version)"
check version-empty '' "$(printf '' | changelog_version)"
check version-not-a-version 'Unreleased' "$(printf '## Unreleased\n\n## v0.9.0\n' | changelog_version)"
check version-no-final-newline v0.4.0 "$(printf '## v0.4.0' | changelog_version)"

# absolute_links
base=https://github.com/o/r/blob/v1.0.0
check links-relative "- [lists]($base/docs/api.md#lists), [up]($base/CHANGELOG.md)" \
  "$(printf '%s\n' '- [lists](docs/api.md#lists), [up](CHANGELOG.md)' | absolute_links "$base")"
check links-absolute '- [#2](https://github.com/o/r/pull/2)' \
  "$(printf '%s\n' '- [#2](https://github.com/o/r/pull/2)' | absolute_links "$base")"
check links-anchor '- [above](#v100)' "$(printf '%s\n' '- [above](#v100)' | absolute_links "$base")"

# unwrap
check unwrap-items '### Fixes

- engine: a job fails ([healthy](docs/a.md#healthy), [#1](https://x/1)).
- one line

Some text that goes on.' "$(printf '%s\n' '### Fixes' '' '- engine: a job' '  fails ([healthy](docs/a.md#healthy),' '  [#1](https://x/1)).' '- one line' '' 'Some text' 'that goes on.' | unwrap)"
check unwrap-numbered '1. stop it
2. start it again' "$(printf '%s\n' '1. stop' '   it' '2. start it' '   again' | unwrap)"
check unwrap-nested '- a b
  - c d' "$(printf '%s\n' '- a' '  b' '  - c' '    d' | unwrap)"
check unwrap-heading-after-text 'a b
### Fixes' "$(printf '%s\n' 'a' 'b' '### Fixes' | unwrap)"
check unwrap-code '```sh
nops serve
  -config x
```
a b' "$(printf '%s\n' '```sh' 'nops serve' '  -config x' '```' 'a' 'b' | unwrap)"
check unwrap-table '| a |
| b |' "$(printf '%s\n' '| a |' '| b |' | unwrap)"
check unwrap-no-final-newline 'a b' "$(printf 'a\nb' | unwrap)"

# has_breaking_changes
ok breaking-changes has_breaking_changes <<<"$(printf '%s' "$changelog" | changelog_section v0.3.0)"
no breaking-changes-none has_breaking_changes <<<"$(printf '%s' "$changelog" | changelog_section v0.2.0)"
no breaking-changes-in-text has_breaking_changes <<<'- see the ### Breaking changes'

# release_branch
check branch-patch release/0.15.x "$(release_branch v0.15.5)"
check branch-rc release/0.15.x "$(release_branch v0.15.5-rc.1)"

# needs_branch: a patch of the last minor while main holds more than fixes
ok needs-branch-feat needs_branch v0.15.4 v0.15.5 <<<$'fix: a\nfeat(web): b'
ok needs-branch-breaking needs_branch v1.2.0 v1.2.1 <<<'fix!: a'
no needs-branch-fixes needs_branch v0.15.4 v0.15.5 <<<$'fix: a\ndocs: b'
no needs-branch-minor needs_branch v0.15.4 v0.16.0 <<<'feat: a'
no needs-branch-first needs_branch v0.0.0 v0.1.0 <<<'feat: a'

# pickable
check pick-fix yes "$(pickable 'fix(engine): a (#1)')"
check pick-docs no "$(pickable 'docs: a (#2)')"
check pick-deps no "$(pickable 'build(deps): bump x from 1 to 2 (#3)')"
check pick-feat never "$(pickable 'feat(web): a (#4)')"
check pick-refactor never "$(pickable 'refactor(store): a')"
check pick-breaking-fix never "$(pickable 'fix!: drop a column')"

# picked_from
check picked-trailers 'abc123
def456' "$(printf '%s\n' 'fix: a (#1)' '' '(cherry picked from commit abc123)' 'docs: b' '(cherry picked from commit def456)' | picked_from)"
check picked-in-text '' "$(printf '%s\n' 'see (cherry picked from commit abc123) above' | picked_from)"
check picked-none '' "$(printf 'fix: a\n' | picked_from)"

# insert_section
section_file=$(mktemp)
trap 'rm -f "$section_file"' EXIT
printf '### Fixes\n\n- a\n' >"$section_file"
check insert-above-first '# Changelog

Intro.

## v0.15.5

### Fixes

- a

## v0.15.4

- b' "$(printf '# Changelog\n\nIntro.\n\n## v0.15.4\n\n- b\n' | insert_section v0.15.5 "$section_file")"
check insert-already-there $'## v0.15.5\n\n- old' "$(printf '## v0.15.5\n\n- old\n' | insert_section v0.15.5 "$section_file")"
check insert-no-section $'# Changelog\n\n## v0.1.0\n\n### Fixes\n\n- a' "$(printf '# Changelog\n' | insert_section v0.1.0 "$section_file")"

# toggle_picks
check toggle-none $'yes\nno\nnever' "$(toggle_picks '' yes no never)"
check toggle-two $'no\nyes\nnever' "$(toggle_picks '1 2' yes no never)"
check toggle-commas $'no\nyes' "$(toggle_picks '1,2' yes no)"
check toggle-twice $'yes\nno' "$(toggle_picks '1 1' yes no)"
check toggle-leading-zero $'yes\nno' "$(toggle_picks '02' yes yes)"
no toggle-never toggle_picks 3 yes no never 2>/dev/null
no toggle-out-of-range toggle_picks 4 yes no never 2>/dev/null
no toggle-zero toggle_picks 0 yes 2>/dev/null
no toggle-word toggle_picks 'all' yes 2>/dev/null
no toggle-glob toggle_picks '*' yes 2>/dev/null

if [ "$failed" = 0 ]; then
  echo "release_test: ok"
fi
exit "$failed"
