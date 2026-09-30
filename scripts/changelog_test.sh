#!/usr/bin/env bash
# Tests of the pure functions of scripts/changelog.sh.
#
#   bash scripts/changelog_test.sh

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/changelog.sh
. "$here/changelog.sh"

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
  if "$@" >/dev/null 2>&1; then echo "FAIL $name: expected failure"; failed=1; fi
}

# pr <title> <patch>: check_pr with the patch on stdin.
pr() {
  printf '%s' "$2" | check_pr "$1"
}

changelog='# Changelog

Intro.

## [Unreleased]

### Added

- Pause a job from the dashboard (#88).

### Fixed

- Keep a held job'"'"'s block visible (#90).

## [0.5.0] - 2026-10-01

### Changed

- **Breaking:** rename -git-url to -repo-url.

## [0.4.1] - 2026-09-30

### Fixed

- One fix.
'

empty='# Changelog

## [Unreleased]

## [0.5.0] - 2026-10-01

### Added

- Something.
'

# needs_entry
ok needs-feat needs_entry 'feat(web): add a page'
ok needs-fix needs_entry 'fix: stop the loop'
ok needs-breaking needs_entry 'refactor(config)!: rename an option'
no needs-docs needs_entry 'docs: explain releases'
no needs-chore needs_entry 'chore(release): v0.5.0'
no needs-refactor needs_entry 'refactor(store): split queries'

# check_pr
added='@@ -5,6 +5,10 @@
 ## [Unreleased]

+### Added
+
+- Pause a job from the dashboard (#88).
+'
breaking='@@ -5,6 +5,8 @@
 ## [Unreleased]

+### Changed
+
+- **Breaking:** rename -git-url to -repo-url.'
removed='@@ -5,6 +5,4 @@
 ## [Unreleased]

-### Added
-
-- Pause a job from the dashboard (#88).'
ok pr-feat-with-line pr 'feat(web): add a page' "$added"
no pr-feat-without-patch pr 'feat(web): add a page' ''
no pr-fix-only-removes pr 'fix: a' "$removed"
no pr-breaking-without-mark pr 'feat(config)!: rename' "$added"
ok pr-breaking-with-mark pr 'feat(config)!: rename' "$breaking"
ok pr-docs-without-patch pr 'docs: a' ''
ok pr-docs-with-line pr 'docs: a' "$added"
ok pr-perf-without-patch pr 'perf: cache the plan' ''

# unreleased
check unreleased "### Added

- Pause a job from the dashboard (#88).

### Fixed

- Keep a held job's block visible (#90)." "$(printf '%s' "$changelog" | unreleased)"
check unreleased-empty '' "$(printf '%s' "$empty" | unreleased)"

# section
check section "### Changed

- **Breaking:** rename -git-url to -repo-url." "$(printf '%s' "$changelog" | section v0.5.0)"
check section-last "### Fixed

- One fix." "$(printf '%s' "$changelog" | section v0.4.1)"
check section-rc "$(printf '%s' "$changelog" | section v0.5.0)" "$(printf '%s' "$changelog" | section v0.5.0-rc.2)"
no section-missing section v0.6.0 <<<"$changelog"
no section-no-prefix-match section v0.5 <<<"$changelog"
no section-empty section v0.5.0 <<<'## [Unreleased]

## [0.5.0] - 2026-10-01

## [0.4.1] - 2026-09-30

- One fix.'

# latest
check latest 0.5.0 "$(printf '%s' "$changelog" | latest)"
check latest-none '' "$(printf '%s' '## [Unreleased]' | latest)"

# cut_release
cut=$(printf '%s' "$changelog" | cut_release v0.6.0 2026-10-02)
check cut-unreleased-empty '' "$(printf '%s' "$cut" | unreleased)"
check cut-moves "$(printf '%s' "$changelog" | unreleased)" "$(printf '%s' "$cut" | section v0.6.0)"
check cut-latest 0.6.0 "$(printf '%s' "$cut" | latest)"
check cut-keeps-older "$(printf '%s' "$changelog" | section v0.5.0)" "$(printf '%s' "$cut" | section v0.5.0)"
check cut-heading '## [0.6.0] - 2026-10-02' "$(printf '%s' "$cut" | grep '^## \[0\.6\.0\]')"
check cut-rc-uses-final '## [0.6.0] - 2026-10-02' "$(printf '%s' "$changelog" | cut_release v0.6.0-rc.1 2026-10-02 | grep '^## \[0\.6\.0\]')"
no cut-existing cut_release v0.5.0 2026-10-02 <<<"$changelog"
no cut-no-unreleased cut_release v0.6.0 2026-10-02 <<<'## [0.5.0] - 2026-10-01'

if [ $failed = 0 ]; then
  echo "ok"
else
  exit 1
fi
