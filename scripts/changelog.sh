#!/usr/bin/env bash
# Read and edit CHANGELOG.md (Keep a Changelog): the checks of a pull request,
# the notes of a release, and the release PR's edit
# (docs/development.md#changelog).
#
#   scripts/changelog.sh check-pr <pr-title>   < patch of CHANGELOG.md in the PR
#   scripts/changelog.sh section <tag>         < CHANGELOG.md   (the release notes)
#   scripts/changelog.sh unreleased            < CHANGELOG.md
#   scripts/changelog.sh latest                < CHANGELOG.md   (newest released version)
#   scripts/changelog.sh cut <version> <date>  < CHANGELOG.md   (prints the edited file)
#
# Written for bash 3.2 (macOS), like release.sh, whose classify it reuses.

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/release.sh
. "$here/release.sh"

# ---- pure functions (tested by scripts/changelog_test.sh) -------------------

# needs_entry <pr-title>: succeeds when the change must add a line to
# CHANGELOG.md: a feat, a fix or a breaking change.
needs_entry() {
  case $(classify "$1") in
    feat | fix | breaking) return 0 ;;
  esac
  return 1
}

# check_pr <pr-title>: reads the patch of CHANGELOG.md in the pull request on
# stdin (empty when the PR does not touch it); on failure says why on stderr.
check_pr() {
  local title=$1 line added=0 breaking=0
  local bullet='^\+[[:space:]]*- ' mark='^\+.*\*\*Breaking:\*\*'
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $bullet ]]; then added=1; fi
    if [[ $line =~ $mark ]]; then breaking=1; fi
  done
  needs_entry "$title" || return 0
  if [ $added = 0 ]; then
    echo "a feat, fix or breaking change adds a line under \"## [Unreleased]\" in CHANGELOG.md (docs/development.md#changelog)" >&2
    return 1
  fi
  if [ "$(classify "$title")" = breaking ] && [ $breaking = 0 ]; then
    echo "a breaking change (\"!\" in the title) adds a \"- **Breaking:** ...\" line to CHANGELOG.md saying what to do when upgrading" >&2
    return 1
  fi
}

# body_of <heading-regex>: reads CHANGELOG.md on stdin, prints the lines under
# the first "## " heading matching the regex, up to the next "## " heading,
# without the blank lines around them.
body_of() {
  local want=$1 line in=0 started=0 blanks=0
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line == '## '* ]]; then
      if [ $in = 1 ]; then break; fi
      if [[ $line =~ $want ]]; then in=1; fi
      continue
    fi
    [ $in = 1 ] || continue
    if [ -z "${line//[[:space:]]/}" ]; then
      if [ $started = 1 ]; then blanks=$((blanks + 1)); fi
      continue
    fi
    while [ $blanks -gt 0 ]; do
      echo
      blanks=$((blanks - 1))
    done
    started=1
    printf '%s\n' "$line"
  done
}

# unreleased: reads CHANGELOG.md on stdin, prints what is under
# "## [Unreleased]" (nothing when it is empty).
unreleased() {
  body_of '^## \[Unreleased\][[:space:]]*$'
}

# section <tag>: reads CHANGELOG.md on stdin, prints the section of vX.Y.Z; a
# prerelease (vX.Y.Z-rc.N) uses the section of the release it leads to. Fails
# when the section is missing or empty.
section() {
  local version=${1#v} body
  version=${version%%-*}
  body=$(body_of "^## \\[${version//./\\.}\\]( |\$)")
  if [ -z "$body" ]; then
    echo "CHANGELOG.md has no \"## [$version]\" section, or it is empty" >&2
    return 1
  fi
  printf '%s\n' "$body"
}

# latest: reads CHANGELOG.md on stdin, prints the newest released version
# (X.Y.Z, the first "## [X.Y.Z]" heading), or nothing.
latest() {
  local line re='^## \[([0-9]+\.[0-9]+\.[0-9]+)\]'
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $re ]]; then
      echo "${BASH_REMATCH[1]}"
      return 0
    fi
  done
}

# cut_release <version> <date>: reads CHANGELOG.md on stdin and prints it with what was
# under "## [Unreleased]" moved to a new "## [X.Y.Z] - <date>" section, and an
# empty "## [Unreleased]" above it.
cut_release() {
  local version=${1#v} date=$2 line done=0 existing
  version=${version%%-*}
  existing="^## \\[${version//./\\.}\\]( |\$)"
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $existing ]]; then
      echo "CHANGELOG.md already has a \"## [$version]\" section" >&2
      return 1
    fi
    printf '%s\n' "$line"
    if [ $done = 0 ] && [[ $line =~ ^'## [Unreleased]'[[:space:]]*$ ]]; then
      printf '\n## [%s] - %s\n' "$version" "$date"
      done=1
    fi
  done
  if [ $done = 0 ]; then
    echo "CHANGELOG.md has no \"## [Unreleased]\" heading" >&2
    return 1
  fi
}

# ---- command line --------------------------------------------------------------

changelog_main() {
  set -euo pipefail
  case ${1:-} in
    check-pr) [ $# = 2 ] || changelog_usage; check_pr "$2" ;;
    section) [ $# = 2 ] || changelog_usage; section "$2" ;;
    unreleased) [ $# = 1 ] || changelog_usage; unreleased ;;
    latest) [ $# = 1 ] || changelog_usage; latest ;;
    cut) [ $# = 3 ] || changelog_usage; cut_release "$2" "$3" ;;
    *) changelog_usage ;;
  esac
}

changelog_usage() {
  sed -n '2,12p' "$here/changelog.sh" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  changelog_main "$@"
fi
