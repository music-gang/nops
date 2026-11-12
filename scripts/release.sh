#!/usr/bin/env bash
# Cut a release: tag the version of the first section of CHANGELOG.md on
# origin/main, or a patch on release/X.Y.x once main holds a feature, show the
# section, then sign and push the tag (docs/development.md#releasing).
#
#   scripts/release.sh             interactive
#   scripts/release.sh --rc        tag the next release candidate of that version
#   scripts/release.sh --dry-run   preview only: no prompt, no tag, no push
#
# Needs git and gh (logged in), and a GPG key in `user.signingkey`.
# Colors errors and warnings on a terminal, unless NO_COLOR is set.
# Written for bash 3.2 (macOS): no associative arrays, no mapfile.

# ---- pure functions (tested by scripts/release_test.sh) ---------------------

# paint <fd> <sgr> <text>: <text> in the SGR attributes <sgr> (31 red, 1 bold...)
# when <fd> is a terminal and NO_COLOR is unset (https://no-color.org).
paint() {
  if [ -z "${NO_COLOR:-}" ] && [ -t "$1" ]; then
    printf '\033[%sm%s\033[0m\n' "$2" "$3"
  else
    printf '%s\n' "$3"
  fi
}

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

# minor_of <version>: X.Y of vX.Y.Z, prerelease ignored.
minor_of() {
  local core=${1#v}
  core=${core%%-*}
  echo "${core%.*}"
}

# stale_image_tags <X.Y>: reads a page on stdin, prints the lines whose
# ghcr.io/music-gang/nops image tag is not X.Y.
stale_image_tags() {
  local want=$1 line
  local re='ghcr\.io/music-gang/nops:([0-9A-Za-z.-]+)'
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $re ]] && [ "${BASH_REMATCH[1]}" != "$want" ]; then
      echo "$line"
    fi
  done
}

# last_release <sha>: the last final tag reachable from <sha>; nothing when
# there is none.
last_release() {
  git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude 'v*-*' "$1" 2>/dev/null || true
}

# breaking_changes: reads `git log -z --format='%h%x09%s%n%b'` on stdin and
# prints the *Breaking changes* of the release: one item per breaking commit,
# made from its BREAKING CHANGE footer (up to the first blank line) and the
# pull request number of its subject. A breaking commit without a footer gets its subject
# and a warning on stderr. Prints nothing when no commit breaks.
breaking_changes() {
  local record header hash subject body line text found count=0
  local footer='^BREAKING[ -]CHANGE:[[:space:]]*(.*)$'
  local pr='\(#([0-9]+)\)$'
  while IFS= read -r -d '' record || [ -n "$record" ]; do
    header=${record%%$'\n'*}
    hash=${header%%$'\t'*}
    subject=${header#*$'\t'}
    [ "$(classify "$subject")" = breaking ] || continue

    body=''
    case $record in *$'\n'*) body=${record#*$'\n'} ;; esac
    text='' found=0
    while IFS= read -r line; do
      if [ $found = 0 ]; then
        if [[ $line =~ $footer ]]; then
          found=1
          text=${BASH_REMATCH[1]}
        fi
      elif [ -z "${line//[[:space:]]/}" ]; then
        break
      else
        line=${line#"${line%%[![:space:]]*}"}
        text="$text $line"
      fi
    done <<EOF
$body
EOF

    if [ -z "$text" ]; then
      warn "$hash $subject has no BREAKING CHANGE footer: its subject is the note, edit the release"
      text=$subject
    elif [[ $subject =~ $pr ]]; then
      text="$text (#${BASH_REMATCH[1]})"
    fi

    if [ $count = 0 ]; then
      printf '## Breaking changes\n\n'
    fi
    printf -- '- %s\n' "$text"
    count=$((count + 1))
  done
}

# changelog_section <tag>: reads CHANGELOG.md on stdin and prints the section
# headed `## <tag>`, up to the next `## ` heading, without its heading and the
# blank lines around it. Prints nothing when there is no such section.
changelog_section() {
  local want="## $1" line in=0 blanks=0 started=0
  while IFS= read -r line || [ -n "$line" ]; do
    if [ $in = 0 ]; then
      [ "$line" = "$want" ] && in=1
      continue
    fi
    case $line in '## '*) break ;; esac
    if [ -z "${line//[[:space:]]/}" ]; then
      blanks=$((blanks + 1))
      continue
    fi
    if [ $started = 1 ]; then
      while [ $blanks -gt 0 ]; do
        echo
        blanks=$((blanks - 1))
      done
    fi
    blanks=0 started=1
    printf '%s\n' "$line"
  done
}

# changelog_version: reads CHANGELOG.md on stdin and prints the text of its
# first `## ` heading, the version of the release it writes. Prints nothing
# when there is no such heading.
changelog_version() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in
      '## '*)
        printf '%s\n' "${line#\#\# }"
        return 0
        ;;
    esac
  done
}

# absolute_links <url>: reads Markdown on stdin and puts <url>/ before each
# relative link target, read from the root of the repository: a GitHub
# Release would resolve it against its own page.
absolute_links() {
  sed -E "s|\]\(([^)/:#][^):]*)\)|](${1}/\1)|g"
}

# unwrap: reads Markdown on stdin and puts each paragraph and list item on one
# line: a GitHub Release breaks the line at each newline. Blank lines,
# headings, tables, quotes and code blocks stay as they are.
unwrap() {
  local line text held='' fence=0
  local item='^([-*+]|[0-9]+[.)])[[:space:]]'
  while IFS= read -r line || [ -n "$line" ]; do
    text=${line#"${line%%[![:space:]]*}"}
    if [ $fence = 1 ]; then
      printf '%s\n' "$line"
      case $text in '```'*) fence=0 ;; esac
      continue
    fi
    case $text in
      '' | '#'* | '|'* | '>'* | '```'*)
        [ -n "$held" ] && printf '%s\n' "$held"
        held=''
        printf '%s\n' "$line"
        case $text in '```'*) fence=1 ;; esac
        continue
        ;;
    esac
    if [[ $text =~ $item ]]; then
      [ -n "$held" ] && printf '%s\n' "$held"
      held=$line
    elif [ -n "$held" ]; then
      held="$held $text"
    else
      held=$line
    fi
  done
  [ -n "$held" ] && printf '%s\n' "$held"
  return 0
}

# has_breaking_changes: succeeds when the changelog section on stdin has an
# `### Breaking changes` heading.
has_breaking_changes() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    [ "$line" = '### Breaking changes' ] && return 0
  done
  return 1
}

# release_branch <version>: release/X.Y.x, the branch of the patches of the
# minor of <version>.
release_branch() {
  echo "release/$(minor_of "$1").x"
}

# needs_branch <last> <version>: reads the commit subjects since <last> on
# stdin and succeeds when <version> is a patch of the minor of <last> while the
# subjects need a minor or major release: the patch can't be tagged on main.
needs_branch() {
  [ "$(minor_of "$2")" = "$(minor_of "$1")" ] || return 1
  case $(suggest_bump "$1") in
    minor | major) return 0 ;;
  esac
  return 1
}

# pickable <subject>: whether a commit goes into a patch on a release branch:
# yes (picked unless unpicked), no (listed unpicked), never (a feature, a
# change or a breaking commit, which a patch never carries).
pickable() {
  case $(classify "$1") in
    fix) echo yes ;;
    other) echo no ;;
    *) echo never ;;
  esac
}

# picked_from: reads commit messages on stdin and prints the commit of each
# `(cherry picked from commit <sha>)` trailer.
picked_from() {
  local line re='^\(cherry picked from commit ([0-9a-f]+)\)$'
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ $line =~ $re ]]; then
      echo "${BASH_REMATCH[1]}"
    fi
  done
}

# insert_section <tag> <file>: reads CHANGELOG.md on stdin and prints it with
# `## <tag>` and the section in <file> above its first `## ` heading, or at its
# end when it has none. Prints it unchanged when it already has `## <tag>`.
insert_section() {
  local want="## $1" file=$2 line n=0 i done=0
  local lines=()
  while IFS= read -r line || [ -n "$line" ]; do
    lines[n]=$line
    n=$((n + 1))
    [ "$line" = "$want" ] && done=1
  done
  for ((i = 0; i < n; i++)); do
    line=${lines[i]}
    if [ $done = 0 ]; then
      case $line in
        '## '*)
          printf '%s\n\n' "$want"
          cat "$file"
          echo
          done=1
          ;;
      esac
    fi
    printf '%s\n' "$line"
  done
  if [ $done = 0 ]; then
    printf '\n%s\n\n' "$want"
    cat "$file"
  fi
}

# toggle_picks <answer> <pick>...: the picks (yes, no or never, one per
# commit, one per line) with those whose numbers are in <answer> (from 1,
# separated by spaces or commas) turned from yes to no or from no to yes.
# Fails, saying why on stderr, on anything but the number of a commit that
# can be picked.
toggle_picks() {
  local words=() picks=() k n i
  IFS=', ' read -r -a words <<<"$1" || true
  shift
  picks=("$@")
  for ((k = 0; k < ${#words[@]}; k++)); do
    n=${words[k]}
    case $n in
      '' | *[!0-9]*)
        echo "'$n' is not the number of a commit" >&2
        return 1
        ;;
    esac
    i=$((10#$n - 1))
    if [ "$i" -lt 0 ] || [ "$i" -ge ${#picks[@]} ]; then
      echo "there is no commit $n" >&2
      return 1
    fi
    case ${picks[i]} in
      yes) picks[i]=no ;;
      no) picks[i]=yes ;;
      *)
        echo "commit $n can't go in a patch" >&2
        return 1
        ;;
    esac
  done
  printf '%s\n' "${picks[@]}"
}

# ---- flow --------------------------------------------------------------------

die() {
  paint 2 '1;31' "release: $*" >&2
  exit 1
}

warn() {
  paint 2 33 "warning: $*" >&2
}

# note <message>: a line to read, neither an error nor a warning.
note() {
  paint 2 36 "$*" >&2
}

# problem <message>: an error in a real run, a warning in a dry run (which is
# only a look, not a release).
problem() {
  if [ "$dry_run" = 1 ]; then
    warn "$*"
  else
    die "$*"
  fi
}

# ci_problems <sha>: one line per check run of the commit that is not green.
ci_problems() {
  gh api --paginate "repos/{owner}/{repo}/commits/$1/check-runs" \
    --jq '.check_runs[] | select(.status != "completed" or (.conclusion | IN("success", "skipped", "neutral") | not)) | "\(.name): \(.status) \(.conclusion // "")"'
}

# check_image_tag <last> <tag> <sha>: a new minor (or major) release needs the
# image tag in docs/running-nops.md on <sha> to be its X.Y. A patch is covered
# by the minor tag, and a release candidate gets no X.Y tag.
check_image_tag() {
  local last=$1 tag=$2 sha=$3 want stale
  case $tag in *-*) return 0 ;; esac
  want=$(minor_of "$tag")
  [ "$want" != "$(minor_of "$last")" ] || return 0
  stale=$(git show "$sha:docs/running-nops.md" | stale_image_tags "$want") ||
    die "could not read docs/running-nops.md on $sha"
  if [ -n "$stale" ]; then
    echo "$stale" >&2
    problem "docs/running-nops.md does not show image tag $want: move it in a pull request first"
  fi
}

# check_changelog <tag> <sha> <breaking-changes>: a final release needs its
# section in CHANGELOG.md on <sha>, the release's notes, with *Breaking
# changes* when a commit breaks something. A release candidate's notes are generated.
check_changelog() {
  local tag=$1 sha=$2 notes=$3 section
  case $tag in *-*) return 0 ;; esac
  section=$(git show "$sha:CHANGELOG.md" 2>/dev/null | changelog_section "$tag" || true)
  if [ -z "$section" ]; then
    problem "CHANGELOG.md has no section for $tag: add it in a pull request first"
    return 0
  fi
  if [ -n "$notes" ] && ! has_breaking_changes <<<"$section"; then
    problem "the CHANGELOG.md section for $tag has no '### Breaking changes', and a commit breaks something"
  fi
}

usage() {
  sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
}

main() {
  set -euo pipefail
  dry_run=0
  local rc=0
  while [ $# -gt 0 ]; do
    case $1 in
      -n | --dry-run) dry_run=1 ;;
      --rc) rc=1 ;;
      -h | --help)
        usage
        return 0
        ;;
      *)
        usage >&2
        return 2
        ;;
    esac
    shift
  done

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
  last=$(last_release "$sha")
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
  git log --reverse --format='  %h  %s' "$range" >"$commits"
  if [ ! -s "$commits" ]; then
    echo "Nothing to release: origin/main ($(git rev-parse --short "$sha")) is $last."
    return 0
  fi

  # The version is the changelog's: the first section is the release's.
  local version
  version=$(git show "$sha:CHANGELOG.md" 2>/dev/null | changelog_version) ||
    die "could not read CHANGELOG.md on $sha"
  if [ -z "$version" ]; then
    die "CHANGELOG.md has no '## vX.Y.Z' section: add it in a pull request first"
  fi
  if ! is_semver "$version" || [[ $version == *-* ]]; then
    die "the first section of CHANGELOG.md is '## $version', not '## vX.Y.Z': fix it in a pull request first"
  fi

  # A patch once main holds a feature goes on the release branch of its minor.
  if [ "$last" != v0.0.0 ] && git log --format=%s "$range" | needs_branch "$last" "$version"; then
    release_on_branch "$sha" "$last" "$version" "$rc"
    return
  fi

  echo "origin/main is $(git rev-parse --short "$sha"), last release is $last."
  echo "$(wc -l <"$commits" | tr -d ' ') commit(s) since $last:"
  cat "$commits"
  echo

  # A release candidate's notes start with the same text; a final release's
  # section of CHANGELOG.md needs its own.
  local notes
  notes=$(git log -z --reverse --format='%h%x09%s%n%b' "$range" | breaking_changes)
  if [ -n "$notes" ]; then
    paint 1 1 "Breaking changes, from the BREAKING CHANGE footers:"
    echo
    echo "$notes"
    echo
  fi

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
    warn "$open_prs open pull request(s): is nothing you want in this release still open?"
  fi
  # A prompt, not a gate: what the docs claim is only checked by an audit.
  note "reminder: audit the docs for what changed since $last (docs/development.md#doc-audit):"
  note "  git diff --stat $range -- '*.md' cmd internal scripts examples .github"

  if [ -z "$(git config --get user.signingkey || true)" ]; then
    problem "git config user.signingkey is not set: the tag has to be signed"
  fi

  if [ "$last" != v0.0.0 ] && ! semver_gt "$version" "$last"; then
    die "the first section of CHANGELOG.md is $version, not above $last: write the section of the release in a pull request first"
  fi

  local tag=$version
  if [ "$rc" = 1 ]; then
    tag=$(git tag -l "$version-rc.*" | next_rc "$version")
  fi
  if git rev-parse -q --verify "refs/tags/$tag" >/dev/null ||
    [ -n "$(git ls-remote --tags origin "refs/tags/$tag")" ]; then
    problem "$tag already exists (tags are never moved or recreated)"
  fi
  check_image_tag "$last" "$tag" "$sha"
  check_changelog "$tag" "$sha" "$notes"

  if [ "$rc" = 1 ]; then
    echo "A release candidate's notes are generated from the commits."
  else
    paint 1 1 "Release notes, the $version section of CHANGELOG.md:"
    echo
    git show "$sha:CHANGELOG.md" | changelog_section "$version"
  fi

  sign_and_push "$tag" "$sha"
}

# sign_and_push <tag> <sha>: show the commit, then, once confirmed, sign <tag>
# on <sha>, push it and offer to follow the release run. A dry run only shows
# the commands.
sign_and_push() {
  local tag=$1 sha=$2
  echo
  echo "Tag $tag on $sha"
  echo "  $(git log -1 --format='%h %s' "$sha")"
  if [ "$dry_run" = 1 ]; then
    echo
    echo "Dry run: nothing was tagged. A release would run:"
    echo "  git tag -s $tag -m $tag $sha"
    echo "  git push origin refs/tags/$tag"
    return 0
  fi

  local answer
  read -r -p "Sign and push $tag? [y/N] " answer
  case $answer in y | Y | yes) ;; *) die "aborted, nothing was tagged" ;; esac

  git tag -s "$tag" -m "$tag" "$sha"
  if ! git push origin "refs/tags/$tag"; then
    paint 2 '1;31' "The tag exists locally but was not pushed. Remove it with: git tag -d $tag" >&2
    return 1
  fi

  local repo
  repo=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
  echo
  paint 1 '1;32' "Pushed $tag. Release: https://github.com/$repo/releases/tag/$tag"
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

# print_picks: the commits of release_on_branch, numbered, with their picks.
# Reads its caller's hashes, subjects and picks.
print_picks() {
  local i mark
  for ((i = 0; i < ${#hashes[@]}; i++)); do
    case ${picks[i]} in
      yes) mark='[x]' ;;
      no) mark='[ ]' ;;
      *) mark=' - ' ;;
    esac
    printf '  %2d %s %s %s\n' $((i + 1)) "$mark" "${hashes[i]:0:7}" "${subjects[i]}"
  done
}

# release_on_branch <main-sha> <last> <version> <rc>: the patch <version> of
# the minor of <last>, once main holds more than fixes. <last> is the newest
# final release, since every minor is tagged on main. The maintainer picks
# the commits of main since <last> that release/X.Y.x doesn't hold yet; they
# are cherry-picked there, in a worktree of their own, with the release's
# section of the changelog, and the branch is pushed. Once nothing is left to
# pick and the checks of the branch are green, its head is tagged.
release_on_branch() {
  local main_sha=$1 start=$2 version=$3 rc=$4
  local branch base last
  branch=$(release_branch "$version")
  echo "origin/main holds more than fixes since $start: $version is a patch on $branch."
  echo

  if git rev-parse -q --verify "refs/remotes/origin/$branch" >/dev/null; then
    base=$(git rev-parse "origin/$branch")
    last=$(last_release "$base")
  else
    base=$(git rev-parse "$start^{commit}")
    last=$start
  fi
  if ! semver_gt "$version" "$last"; then
    die "the first section of CHANGELOG.md is $version, not above $last: write the section of the release in a pull request first"
  fi

  # The commits of main the branch doesn't hold yet, by their trailer. One
  # that changes only the changelog is left out: the script carries the
  # release's section itself.
  local held h s
  local hashes=() subjects=() picks=()
  held=$(git log --format=%B "$start..$base" | picked_from)
  while IFS=$'\t' read -r h s; do
    case $'\n'$held$'\n' in *$'\n'"$h"$'\n'*) continue ;; esac
    [ "$(git diff-tree --no-commit-id --name-only -r "$h")" != CHANGELOG.md ] || continue
    hashes+=("$h")
    subjects+=("$s")
    picks+=("$(pickable "$s")")
  done < <(git log --reverse --format='%H%x09%s' "$start..$main_sha")

  if [ ${#hashes[@]} = 0 ]; then
    echo "$branch holds every commit of origin/main since $start it can."
  else
    echo "Commits of origin/main since $start that $branch doesn't hold"
    echo "([x] goes in the patch, [ ] doesn't, - never can):"
    print_picks
    echo
    local answer new p
    while [ "$dry_run" = 0 ]; do
      read -r -p "Enter to go on, or the numbers of the commits to pick or unpick: " answer
      [ -z "${answer//[[:space:]]/}" ] && break
      if new=$(toggle_picks "$answer" "${picks[@]}"); then
        picks=()
        while IFS= read -r p; do picks+=("$p"); done <<<"$new"
        print_picks
        echo
      fi
    done
  fi

  local chosen=() i k
  for ((i = 0; i < ${#hashes[@]}; i++)); do
    if [ "${picks[i]}" = yes ]; then chosen+=("$i"); fi
  done

  # A final release carries its section of the changelog, written on main.
  local section
  section=$(mktemp)
  # shellcheck disable=SC2064
  trap "rm -f '$commits' '$section'" EXIT
  local add_section=0
  if [ "$rc" = 0 ] && [ -z "$(git show "$base:CHANGELOG.md" | changelog_section "$version")" ]; then
    git show "$main_sha:CHANGELOG.md" | changelog_section "$version" >"$section"
    [ -s "$section" ] || die "CHANGELOG.md on main has no section for $version: add it in a pull request first"
    add_section=1
  fi

  if [ ${#chosen[@]} -gt 0 ] || [ $add_section = 1 ]; then
    if [ "$dry_run" = 1 ]; then
      echo "Dry run: nothing was picked or pushed. A release would cherry-pick ${#chosen[@]} commit(s) onto $branch,"
      if [ $add_section = 1 ]; then echo "add the $version section of CHANGELOG.md,"; fi
      echo "push $branch, and tag it once its checks are green."
      return 0
    fi
    if [ -z "$(git config --get user.signingkey || true)" ]; then
      die "git config user.signingkey is not set: the commits and the tag have to be signed"
    fi

    local wt
    wt=$(mktemp -d)
    git worktree add --quiet --detach "$wt" "$base" || die "could not make a worktree of $branch in $wt"
    for ((k = 0; k < ${#chosen[@]}; k++)); do
      i=${chosen[k]}
      if ! git -C "$wt" cherry-pick -x -S "${hashes[i]}" >/dev/null; then
        paint 2 '1;31' "release: ${hashes[i]:0:7} ${subjects[i]} does not apply on $branch" >&2
        cat >&2 <<EOF
Resolve it in $wt and finish it there:
  git -C $wt cherry-pick --continue
  git -C $wt push origin HEAD:refs/heads/$branch
  git worktree remove $wt
then run scripts/release.sh again for the rest.
EOF
        exit 1
      fi
    done
    if [ $add_section = 1 ]; then
      git -C "$wt" show HEAD:CHANGELOG.md | insert_section "$version" "$section" >"$wt/CHANGELOG.md"
      git -C "$wt" commit --quiet -S -m "docs: write the changelog of $version" -- CHANGELOG.md
    fi
    git -C "$wt" push --quiet origin "HEAD:refs/heads/$branch" || die "could not push $branch: the commits are in $wt"
    git worktree remove "$wt"
    echo
    paint 1 '1;32' "Pushed $branch. Run scripts/release.sh again once its checks are green to tag $version."
    return 0
  fi

  if [ "$(git rev-list --count "$last..$base")" = 0 ]; then
    echo "Nothing to release: $branch is $last and nothing was picked."
    return 0
  fi

  local tag=$version
  if [ "$rc" = 1 ]; then
    tag=$(git tag -l "$version-rc.*" | next_rc "$version")
  fi
  if git rev-parse -q --verify "refs/tags/$tag" >/dev/null ||
    [ -n "$(git ls-remote --tags origin "refs/tags/$tag")" ]; then
    problem "$tag already exists (tags are never moved or recreated)"
  fi

  # Checks that have yet to start would read as green.
  local runs ci
  runs=$(gh api "repos/{owner}/{repo}/commits/$base/check-runs" --jq .total_count) ||
    die "could not read the checks of $base (gh auth status?)"
  if [ "$runs" = 0 ]; then
    problem "$branch ($(git rev-parse --short "$base")) has no checks yet: wait for CI to run on it"
  fi
  ci=$(ci_problems "$base") || die "could not read the checks of $base (gh auth status?)"
  if [ -n "$ci" ]; then
    echo "$ci" >&2
    problem "checks on $branch ($(git rev-parse --short "$base")) are not all green"
  fi
  if [ -z "$(git config --get user.signingkey || true)" ]; then
    problem "git config user.signingkey is not set: the tag has to be signed"
  fi
  check_changelog "$tag" "$base" ""

  if [ "$rc" = 1 ]; then
    echo "A release candidate's notes are generated from the commits."
  else
    paint 1 1 "Release notes, the $version section of CHANGELOG.md on $branch:"
    echo
    git show "$base:CHANGELOG.md" | changelog_section "$version"
  fi
  sign_and_push "$tag" "$base"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main "$@"
fi
