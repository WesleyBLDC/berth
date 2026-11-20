#!/bin/sh
# The fail fast gate around check, merge and push.
# Every step prints its exit code, a failure stops the gate before anything moves, and no pipe hides a status.
#
#   scripts/gate.sh check            make check, which must end with "check: PASS"
#   scripts/gate.sh merge <branch>   from the main checkout: rebase the branch onto main in its worktree,
#                                    check it, fast forward main, and check main again
#   scripts/gate.sh push             gitleaks and the personal data grep, then a fast forward push of main
#
# BERTH_GATE_CHECK replaces "make check" for the gate's own test (scripts/test-gate.sh).
# pipefail is in POSIX.1-2024 and in every sh macOS ships (bash and dash), though shellcheck predates that.
# shellcheck disable=SC3040
set -euo pipefail

check_cmd=${BERTH_GATE_CHECK:-make check}
pass_line='check: PASS'
# Built from pieces so this file does not match its own grep.
personal='wezz''dc|/Users''/wes|auto''complete|auto''protect'

say() { printf 'gate: %s\n' "$*"; }
die() {
  say "FAIL: $*"
  exit 1
}
usage() {
  echo 'usage: scripts/gate.sh check | merge <branch> | push' >&2
  exit 2
}

# run NAME COMMAND...: an external command only, since errexit is off inside a function called beside ||.
run() {
  name=$1
  shift
  say "$name: $*"
  st=0
  "$@" || st=$?
  say "$name: exit $st"
  [ "$st" -eq 0 ] || die "$name"
}

clean() {
  status=$(git -C "$1" status --porcelain)
  if [ -n "$status" ]; then
    printf '%s\n' "$status"
    say "clean tree in $1: exit 1"
    die "uncommitted or untracked files in $1"
  fi
  say "clean tree in $1: exit 0"
}

passed_file() { printf '%s/berth-gate-passed\n' "$(git rev-parse --path-format=absolute --git-common-dir)"; }

# check_in DIR: returns nonzero unless the check exits 0 and its last line is the pass line.
# A pass on a clean tree is recorded by tree hash, so push can prove the exact tree passed.
check_in() {
  dir=$1
  log=$(mktemp "${TMPDIR:-/tmp}/berth-gate.XXXXXX")
  say "check in $dir: $check_cmd"
  st=0
  (cd "$dir" && sh -c "$check_cmd") 2>&1 | tee "$log" || st=$?
  last=$(tail -n 1 "$log")
  rm -f "$log"
  say "check in $dir: exit $st"
  if [ "$st" -ne 0 ]; then
    return 1
  fi
  if [ "$last" != "$pass_line" ]; then
    say "check in $dir: last line is '$last', not '$pass_line'"
    return 1
  fi
  if [ -z "$(git -C "$dir" status --porcelain)" ]; then
    git -C "$dir" rev-parse 'HEAD^{tree}' >>"$(passed_file)"
  else
    say "check in $dir: tree not clean, so this pass is not recorded for push"
  fi
}

main_checkout() {
  list=$(git worktree list --porcelain)
  first=${list%%"
"*}
  printf '%s\n' "${first#worktree }"
}

on_main_checkout() {
  top=$(git rev-parse --show-toplevel)
  [ "$top" = "$(main_checkout)" ] || die "run $1 from the main checkout, $(main_checkout), not $top"
  cur=$(git symbolic-ref --short -q HEAD || true)
  [ "$cur" = main ] || die "the main checkout is on '$cur', not main"
}

worktree_of() {
  git worktree list --porcelain >"$1.list"
  awk -v ref="branch refs/heads/$2" '/^worktree /{w=substr($0,10)} $0==ref{print w}' "$1.list"
  rm -f "$1.list"
}

do_check() {
  check_in "$(git rev-parse --show-toplevel)" || die check
  say 'check: PASS'
}

do_merge() {
  branch=$1
  [ "$branch" != main ] || die 'merge a work branch into main, not main itself'
  on_main_checkout merge
  top=$(git rev-parse --show-toplevel)
  git show-ref --verify --quiet "refs/heads/$branch" || die "no local branch $branch"
  tmp=$(mktemp "${TMPDIR:-/tmp}/berth-gate.XXXXXX")
  wt=$(worktree_of "$tmp" "$branch")
  rm -f "$tmp"
  [ -n "$wt" ] || die "branch $branch is not checked out in any worktree"
  clean "$top"
  clean "$wt"
  before=$(git rev-parse main)

  say "rebase $branch onto main in $wt"
  st=0
  git -C "$wt" rebase main || st=$?
  say "rebase: exit $st"
  if [ "$st" -ne 0 ]; then
    git -C "$wt" rebase --abort || true
    die "rebase of $branch onto main; aborted, nothing merged"
  fi

  check_in "$wt" || die "check of $branch after the rebase; nothing merged"
  run fast-forward git merge --ff-only "$branch"
  if ! check_in "$top"; then
    git reset --keep "$before"
    die "check of main after the fast forward; main reset to $before, nothing merged"
  fi
  say "merge: PASS, main is at $(git rev-parse --short main); nothing pushed"
}

do_push() {
  on_main_checkout push
  top=$(git rev-parse --show-toplevel)
  clean "$top"
  run fetch git fetch origin main
  say 'fast forward of origin/main'
  st=0
  git merge-base --is-ancestor origin/main main || st=$?
  say "fast forward of origin/main: exit $st"
  [ "$st" -eq 0 ] || die 'main is not a fast forward of origin/main; rebase it, never force'
  if [ "$(git rev-parse main)" = "$(git rev-parse origin/main)" ]; then
    say 'push: nothing to push'
    return 0
  fi

  tree=$(git rev-parse 'main^{tree}')
  say "tree $tree passed check"
  pf=$(passed_file)
  st=0
  [ -f "$pf" ] && grep -qxF "$tree" "$pf" || st=$?
  say "tree $tree passed check: exit $st"
  [ "$st" -eq 0 ] || die 'this exact tree has not passed check; run scripts/gate.sh check first'

  version=$(awk '/gitleaks\/gitleaks/{f=1;next} f&&/rev:/{print $2;exit}' "$top/.pre-commit-config.yaml")
  [ -n "$version" ] || die 'no gitleaks rev in .pre-commit-config.yaml'
  run gitleaks go run "github.com/zricethezav/gitleaks/v8@$version" git --no-banner --redact \
    --log-opts=origin/main..main "$top"

  say 'personal data grep'
  st=0
  git grep -nE "$personal" -- . ':!gen' || st=$?
  say "personal data grep: exit $st"
  case $st in
    1) ;;
    0) die 'personal data in the tree' ;;
    *) die 'personal data grep could not run' ;;
  esac

  run push git push origin main:main
  say 'push: PASS'
}

[ $# -ge 1 ] || usage
case $1 in
  check)
    [ $# -eq 1 ] || usage
    do_check
    ;;
  merge)
    [ $# -eq 2 ] || usage
    do_merge "$2"
    ;;
  push)
    [ $# -eq 1 ] || usage
    do_push
    ;;
  *) usage ;;
esac
