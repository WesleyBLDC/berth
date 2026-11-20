#!/bin/sh
# The fail fast gate around check, merge and push.
# Every step prints its exit code, a failure stops the gate before anything moves, and no pipe hides a status.
#
#   scripts/gate.sh check            make check, which must end with "check: PASS"
#   scripts/gate.sh merge <branch>   from the main checkout: rebase the branch onto main in its worktree,
#                                    check it, fast forward main, and check main again
#   scripts/gate.sh push             gitleaks and the personal data scan over the new commits, then a fast
#                                    forward push of main, only of a tree that passed make check
#
# BERTH_GATE_CHECK replaces "make check" for the gate's own test (scripts/test-gate.sh); push accepts the
# passes it records only when BERTH_GATE_TEST_ACCEPT_FAKE=1, which only that test sets.
# pipefail is in POSIX.1-2024 and in every sh macOS ships (bash and dash), though shellcheck predates that.
# shellcheck disable=SC3040
set -euo pipefail

real_check='make check'
check_cmd=${BERTH_GATE_CHECK:-$real_check}
if [ "$check_cmd" = "$real_check" ]; then
  check_label=$real_check
else
  check_label=BERTH_GATE_CHECK
fi
pass_line='check: PASS'
# Built from pieces so this file does not match its own scan.
personal='wezz''dc|/Users''/wes|auto''complete|auto''protect'
common=$(git rev-parse --path-format=absolute --git-common-dir)
passed="$common/berth-gate-passed"
lock="$common/berth-gate.lock"
scratch=$(mktemp -d "${TMPDIR:-/tmp}/berth-gate.XXXXXX")
locked=0
cleanup() {
  rm -rf "$scratch"
  if [ "$locked" -eq 1 ]; then rm -rf "$lock"; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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

take_lock() {
  if mkdir "$lock" 2>/dev/null; then
    locked=1
    echo "$$" >"$lock/pid"
    say "lock $lock: taken"
    return 0
  fi
  holder=$(cat "$lock/pid" 2>/dev/null || echo unknown)
  die "another gate merge or push (pid $holder) holds $lock; if no gate is running, remove that directory"
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

record_pass() {
  printf '%s %s\n' "$1" "$check_label" >>"$passed"
  tail -n 100 "$passed" >"$scratch/passed"
  cat "$scratch/passed" >"$passed"
}

forget_pass() {
  [ -f "$passed" ] || return 0
  grep -v "^$1 " "$passed" >"$scratch/passed" || true
  cat "$scratch/passed" >"$passed"
}

# check_in DIR: returns nonzero unless the check exits 0 and its last line is the pass line.
# The pass is recorded for push only when the tree was clean before the check and is the same clean tree after.
check_in() {
  dir=$1
  before_tree=
  if [ -z "$(git -C "$dir" status --porcelain)" ]; then
    before_tree=$(git -C "$dir" rev-parse 'HEAD^{tree}')
  fi
  say "check in $dir: $check_label"
  st=0
  (cd "$dir" && sh -c "$check_cmd") 2>&1 | tee "$scratch/log" || st=$?
  last=$(tail -n 1 "$scratch/log" | tr -d '\000-\037\177')
  say "check in $dir: exit $st"
  if [ "$st" -ne 0 ]; then
    return 1
  fi
  if [ "$last" != "$pass_line" ]; then
    say "check in $dir: last line is '$last', not '$pass_line'"
    return 1
  fi
  after_tree=$(git -C "$dir" rev-parse 'HEAD^{tree}')
  if [ -z "$before_tree" ]; then
    say "check in $dir: the tree was not clean before the check, so this pass is not recorded for push"
  elif [ "$after_tree" != "$before_tree" ] || [ -n "$(git -C "$dir" status --porcelain)" ]; then
    say "check in $dir: the tree changed during the check, so this pass is not recorded for push"
  else
    record_pass "$before_tree"
    say "check in $dir: recorded tree $before_tree"
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
  git worktree list --porcelain >"$scratch/list"
  awk -v ref="branch refs/heads/$1" '/^worktree /{w=substr($0,10)} $0==ref{print w}' "$scratch/list"
}

do_check() {
  check_in "$(git rev-parse --show-toplevel)" || die check
  say 'check: PASS'
}

do_merge() {
  branch=$1
  [ "$branch" != main ] || die 'merge a work branch into main, not main itself'
  on_main_checkout merge
  take_lock
  top=$(git rev-parse --show-toplevel)
  git show-ref --verify --quiet "refs/heads/$branch" || die "no local branch $branch"
  wt=$(worktree_of "$branch")
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
    forget_pass "$(git rev-parse 'HEAD^{tree}')"
    after=$(git rev-parse main)
    git reset --keep "$before" ||
      die "check of main after the fast forward, and main could not be reset: main left at $after; restore it with git reset --keep $before once the tree is clean"
    die "check of main after the fast forward; main reset to $before, nothing merged"
  fi
  say "merge: PASS, main is at $(git rev-parse --short main); nothing pushed"
}

# scan_personal BASE SHA: the added lines of every commit in BASE..SHA and the tree at SHA, case insensitively.
scan_personal() {
  say "patches of $1..$2"
  st=0
  git -C "$top" log -p --no-color --format='commit %H' "$1..$2" -- ':/' ':(exclude,top)gen' >"$scratch/patches" || st=$?
  say "patches of $1..$2: exit $st"
  [ "$st" -eq 0 ] || die "patches of $1..$2"
  say 'personal data in the added lines'
  st=0
  grep -inE "^\+.*($personal)" "$scratch/patches" || st=$?
  say "personal data in the added lines: exit $st"
  case $st in
    1) ;;
    0) die 'personal data in the new commits' ;;
    *) die 'personal data scan of the patches could not run' ;;
  esac
  say "personal data in the tree at $2"
  st=0
  git -C "$top" grep -inE "$personal" "$2" -- ':/' ':(exclude,top)gen' || st=$?
  say "personal data in the tree at $2: exit $st"
  case $st in
    1) ;;
    0) die 'personal data in the tree' ;;
    *) die 'personal data grep could not run' ;;
  esac
}

do_push() {
  on_main_checkout push
  take_lock
  top=$(git rev-parse --show-toplevel)
  clean "$top"
  sha=$(git rev-parse main)
  say "pushing main at $sha"
  run fetch git fetch origin main
  base=$(git rev-parse origin/main)
  say "fast forward of origin/main $base"
  st=0
  git merge-base --is-ancestor "$base" "$sha" || st=$?
  say "fast forward of origin/main $base: exit $st"
  [ "$st" -eq 0 ] || die 'main is not a fast forward of origin/main; rebase it, never force'
  if [ "$sha" = "$base" ]; then
    say 'push: nothing to push'
    return 0
  fi

  tree=$(git rev-parse "$sha^{tree}")
  say "tree $tree passed $real_check"
  st=1
  if [ -f "$passed" ]; then
    if grep -qxF "$tree $real_check" "$passed"; then
      st=0
    elif [ "${BERTH_GATE_TEST_ACCEPT_FAKE:-}" = 1 ] && grep -qxF "$tree BERTH_GATE_CHECK" "$passed"; then
      st=0
    fi
  fi
  say "tree $tree passed $real_check: exit $st"
  [ "$st" -eq 0 ] || die "this exact tree has not passed $real_check; run scripts/gate.sh check on a clean tree first"

  version=$(awk '/gitleaks\/gitleaks/{f=1;next} f&&/rev:/{print $2;exit}' "$top/.pre-commit-config.yaml")
  [ -n "$version" ] || die 'no gitleaks rev in .pre-commit-config.yaml'
  run gitleaks go run "github.com/zricethezav/gitleaks/v8@$version" git --no-banner --redact \
    "--log-opts=$base..$sha" "$top"

  scan_personal "$base" "$sha"

  run push git push origin "$sha:refs/heads/main"
  say "push: PASS, origin/main is at $sha"
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
