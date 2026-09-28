#!/usr/bin/env bash
# Cut a release: list what landed on origin/main since the last tag, suggest a
# version, let the maintainer choose, then sign and push the tag. The `release`
# workflow does the rest (docs/development.md#releasing).
#
#   scripts/release.sh             interactive
#   scripts/release.sh --dry-run   preview only: no prompt, no tag, no push
#
# Needs git and gh (logged in), and a GPG key in `user.signingkey`.
# Written for bash 3.2 (macOS): no associative arrays, no mapfile.

# ---- pure functions (tested by scripts/release_test.sh) ---------------------

# classify <subject>: breaking | feat | fix | change | other
# `change` is refactor, perf and revert; docs, ci, chore, build, test, style and
# anything that is not an Angular subject are `other`.
classify() {
  local subject=$1
  local breaking='^[a-z]+(\([^)]*\))?!:'
  local feat='^feat(\([^)]*\))?:'
  local fix='^fix(\([^)]*\))?:'
  local change='^(refactor|perf|revert)(\([^)]*\))?:'
  if [[ $subject =~ $breaking ]]; then
    echo breaking
  elif [[ $subject =~ $feat ]]; then
    echo feat
  elif [[ $subject =~ $fix ]]; then
    echo fix
  elif [[ $subject =~ $change ]]; then
    echo change
  else
    echo other
  fi
}

# suggest_bump <current-version>: reads commit subjects on stdin, prints
# patch | minor | major.
# At v0 a minor may break and a patch never does, so anything but fix, docs, ci,
# chore, build, test and style is a minor; from v1 it is plain semver.
suggest_bump() {
  local major=${1#v}
  major=${major%%.*}
  local has_breaking=0 has_feat=0 has_change=0 subject
  while IFS= read -r subject || [ -n "$subject" ]; do
    case $(classify "$subject") in
      breaking) has_breaking=1 ;;
      feat) has_feat=1 ;;
      change) has_change=1 ;;
    esac
  done
  if [ "$major" = 0 ]; then
    if [ $has_breaking = 1 ] || [ $has_feat = 1 ] || [ $has_change = 1 ]; then
      echo minor
    else
      echo patch
    fi
  elif [ $has_breaking = 1 ]; then
    echo major
  elif [ $has_feat = 1 ]; then
    echo minor
  else
    echo patch
  fi
}

# bump_version <version> <patch|minor|major>: vX.Y.Z; a prerelease suffix on the
# input is ignored.
bump_version() {
  local core=${1#v}
  core=${core%%-*}
  local major minor patch
  IFS=. read -r major minor patch <<EOF
$core
EOF
  case $2 in
    major) echo "v$((major + 1)).0.0" ;;
    minor) echo "v$major.$((minor + 1)).0" ;;
    patch) echo "v$major.$minor.$((patch + 1))" ;;
    *)
      echo "bump_version: unknown kind '$2'" >&2
      return 1
      ;;
  esac
}

# next_rc <vX.Y.Z>: reads existing tags on stdin, prints the next free
# vX.Y.Z-rc.N.
next_rc() {
  local base=$1 tag n max=0
  while IFS= read -r tag || [ -n "$tag" ]; do
    case $tag in
      "$base"-rc.*)
        n=${tag#"$base"-rc.}
        case $n in
          '' | *[!0-9]*) ;;
          *) if [ "$n" -gt "$max" ]; then max=$n; fi ;;
        esac
        ;;
    esac
  done
  echo "$base-rc.$((max + 1))"
}

# is_semver <version>: vX.Y.Z with an optional -prerelease.
is_semver() {
  local re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
  [[ $1 =~ $re ]]
}

# semver_gt <a> <b>: succeeds when a is a higher version than b.
semver_gt() {
  local a=${1#v} b=${2#v}
  local acore=${a%%-*} bcore=${b%%-*} apre='' bpre=''
  if [ "$a" != "$acore" ]; then apre=${a#*-}; fi
  if [ "$b" != "$bcore" ]; then bpre=${b#*-}; fi

  local a1 a2 a3 b1 b2 b3
  IFS=. read -r a1 a2 a3 <<EOF
$acore
EOF
  IFS=. read -r b1 b2 b3 <<EOF
$bcore
EOF
  if [ "$a1" -ne "$b1" ]; then [ "$a1" -gt "$b1" ]; return; fi
  if [ "$a2" -ne "$b2" ]; then [ "$a2" -gt "$b2" ]; return; fi
  if [ "$a3" -ne "$b3" ]; then [ "$a3" -gt "$b3" ]; return; fi

  # Same X.Y.Z: a final release is above any prerelease of it.
  if [ "$apre" = "$bpre" ]; then return 1; fi
  if [ -z "$apre" ]; then return 0; fi
  if [ -z "$bpre" ]; then return 1; fi
  local rc='^rc\.([0-9]+)$'
  if [[ $apre =~ $rc ]]; then
    local an=${BASH_REMATCH[1]}
    if [[ $bpre =~ $rc ]]; then
      [ "$an" -gt "${BASH_REMATCH[1]}" ]
      return
    fi
  fi
  [[ $apre > $bpre ]]
}

# ---- flow --------------------------------------------------------------------

die() {
  echo "release: $*" >&2
  exit 1
}

# problem <message>: an error in a real run, a warning in a dry run (which is
# only a look, not a release).
problem() {
  if [ "$dry_run" = 1 ]; then
    echo "warning: $*" >&2
  else
    die "$*"
  fi
}

# print_group <title> <class> <file>: the commits of one class, if any.
print_group() {
  local title=$1 class=$2 file=$3 hash subject printed=0
  while IFS="$(printf '\t')" read -r hash subject; do
    if [ "$(classify "$subject")" = "$class" ]; then
      if [ $printed = 0 ]; then
        printf '\n%s\n' "$title"
        printed=1
      fi
      printf '  %s  %s\n' "$hash" "$subject"
    fi
  done <"$file"
}

# ci_problems <sha>: one line per check run of the commit that is not green.
ci_problems() {
  gh api --paginate "repos/{owner}/{repo}/commits/$1/check-runs" \
    --jq '.check_runs[] | select(.status != "completed" or (.conclusion | IN("success", "skipped", "neutral") | not)) | "\(.name): \(.status) \(.conclusion // "")"'
}

# choose_version <last> <suggested-kind>: asks, prints the chosen tag.
choose_version() {
  local last=$1 kind=$2
  local patch minor major suggested rc default choice custom
  patch=$(bump_version "$last" patch)
  minor=$(bump_version "$last" minor)
  major=$(bump_version "$last" major)
  case $kind in
    patch) suggested=$patch default=1 ;;
    minor) suggested=$minor default=2 ;;
    major) suggested=$major default=3 ;;
  esac
  rc=$(git tag -l "$suggested-rc.*" | next_rc "$suggested")

  {
    echo
    echo "Which version? (suggested: $suggested)"
    echo "  1) patch   $patch"
    echo "  2) minor   $minor"
    echo "  3) major   $major"
    echo "  4) rc      $rc"
    echo "  5) custom"
  } >&2

  while true; do
    read -r -p "Choice [$default]: " choice
    choice=${choice:-$default}
    case $choice in
      1) echo "$patch"; return ;;
      2) echo "$minor"; return ;;
      3) echo "$major"; return ;;
      4) echo "$rc"; return ;;
      5)
        read -r -p "Version (vX.Y.Z or vX.Y.Z-rc.N): " custom
        case $custom in v*) ;; *) custom=v$custom ;; esac
        if ! is_semver "$custom"; then
          echo "not a semver version: $custom" >&2
        elif [ "$last" != v0.0.0 ] && ! semver_gt "$custom" "$last"; then
          echo "$custom is not higher than $last" >&2
        else
          echo "$custom"
          return
        fi
        ;;
      *) echo "type 1-5" >&2 ;;
    esac
  done
}

usage() {
  sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
}

main() {
  set -euo pipefail
  dry_run=0
  case ${1:-} in
    '') ;;
    -n | --dry-run) dry_run=1 ;;
    -h | --help)
      usage
      return 0
      ;;
    *)
      usage >&2
      return 2
      ;;
  esac

  command -v git >/dev/null || die "git is not installed"
  command -v gh >/dev/null || die "gh is not installed (https://cli.github.com)"
  git rev-parse --git-dir >/dev/null 2>&1 || die "not inside the nops repository"
  if [ "$dry_run" = 0 ] && [ ! -t 0 ]; then
    die "needs a terminal (use --dry-run for a preview)"
  fi

  git fetch --quiet --tags origin || die "git fetch origin failed"
  local sha last range
  sha=$(git rev-parse origin/main)

  # The last final tag reachable from origin/main; v0.0.0 when there is none.
  last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude 'v*-*' "$sha" 2>/dev/null || true)
  if [ -n "$last" ]; then
    range="$last..$sha"
  else
    last=v0.0.0
    range=$sha
  fi

  local commits
  commits=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '$commits'" EXIT
  git log --reverse --format='%h%x09%s' "$range" >"$commits"
  if [ ! -s "$commits" ]; then
    echo "Nothing to release: origin/main ($(git rev-parse --short "$sha")) is $last."
    return 0
  fi

  echo "origin/main is $(git rev-parse --short "$sha"), last release is $last."
  echo "$(wc -l <"$commits" | tr -d ' ') commit(s) since $last:"
  print_group "Breaking changes" breaking "$commits"
  print_group "Features" feat "$commits"
  print_group "Fixes" fix "$commits"
  print_group "Changes" change "$commits"
  print_group "Other (not in the changelog)" other "$commits"
  echo

  # CI on the commit, and open PRs (a warning only: the maintainer may leave one out).
  local ci
  ci=$(ci_problems "$sha") || die "could not read the checks of $sha (gh auth status?)"
  if [ -n "$ci" ]; then
    echo "$ci" >&2
    problem "checks on $(git rev-parse --short "$sha") are not all green"
  fi
  local open_prs
  open_prs=$(gh pr list --state open --json number --jq 'length') || die "gh pr list failed"
  if [ "$open_prs" != 0 ]; then
    echo "warning: $open_prs open pull request(s): is nothing you want in this release still open?" >&2
  fi
  # A prompt, not a gate: what the docs claim is only checked by an audit.
  echo "reminder: audit the docs for what changed since $last (docs/development.md#doc-audit):" >&2
  echo "  git diff --stat $range -- '*.md' cmd internal scripts examples .github" >&2

  local kind suggested
  kind=$(cut -f2 "$commits" | suggest_bump "$last")
  suggested=$(bump_version "$last" "$kind")

  if [ -z "$(git config --get user.signingkey || true)" ]; then
    problem "git config user.signingkey is not set: the tag has to be signed"
  fi

  local tag
  if [ "$dry_run" = 1 ]; then
    tag=$suggested
    echo "Suggested version: $tag ($kind)"
    echo
    echo "Dry run: nothing was tagged. A release would run:"
    echo "  git tag -s $tag -m $tag $sha"
    echo "  git push origin refs/tags/$tag"
    return 0
  fi

  tag=$(choose_version "$last" "$kind")
  if git rev-parse -q --verify "refs/tags/$tag" >/dev/null ||
    [ -n "$(git ls-remote --tags origin "refs/tags/$tag")" ]; then
    die "$tag already exists (tags are never moved or recreated: pick another version)"
  fi

  echo
  echo "Tag $tag on $sha"
  echo "  $(git log -1 --format='%h %s' "$sha")"
  local answer
  read -r -p "Sign and push? [y/N] " answer
  case $answer in y | Y | yes) ;; *) die "aborted, nothing was tagged" ;; esac

  git tag -s "$tag" -m "$tag" "$sha"
  if ! git push origin "refs/tags/$tag"; then
    echo "The tag exists locally but was not pushed. Remove it with: git tag -d $tag" >&2
    return 1
  fi

  local repo
  repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  echo
  echo "Pushed $tag. Release: https://github.com/$repo/releases/tag/$tag"
  read -r -p "Follow the release run? [y/N] " answer
  case $answer in y | Y | yes) ;; *) return 0 ;; esac

  local run=''
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    run=$(gh run list --workflow release.yml --event push --commit "$sha" --limit 1 \
      --json databaseId --jq '.[0].databaseId // empty')
    [ -n "$run" ] && break
    sleep 3
  done
  [ -n "$run" ] || die "no release run found yet: gh run list --workflow release.yml"
  gh run watch "$run" --exit-status
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
