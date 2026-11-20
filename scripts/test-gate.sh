#!/bin/sh
# Proves scripts/gate.sh stops before anything moves: a failing merge, a failing check, a check without its
# pass line and a dirty tree each leave main and origin where they were, and only a clean pass merges or pushes.
# Runs in throwaway repositories with a fake check, so it never runs make check itself.
# pipefail is in POSIX.1-2024 and in every sh macOS ships (bash and dash), though shellcheck predates that.
# shellcheck disable=SC3040
set -euo pipefail

gate="$(cd "$(dirname "$0")" && pwd)/gate.sh"
config="$(cd "$(dirname "$0")/.." && pwd)/.pre-commit-config.yaml"
root=$(mktemp -d "${TMPDIR:-/tmp}/berth-test-gate.XXXXXX")
trap 'rm -rf "$root"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=gate GIT_AUTHOR_EMAIL=gate@example.invalid
export GIT_COMMITTER_NAME=gate GIT_COMMITTER_EMAIL=gate@example.invalid
# The fake check fails on a fail-check file, ends without the pass line on a no-pass file,
# and fails only in the main checkout when $root/fail-main exists.
export BERTH_GATE_CHECK="if [ -e fail-check ]; then echo boom; exit 1; fi
if [ -e no-pass ]; then echo done; exit 0; fi
if [ -e '$root/fail-main' ] && [ \"\$(basename \"\$PWD\")\" = main ]; then echo boom; exit 1; fi
echo 'check: PASS'"

failures=0
n=0
case_dir=

fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}

# setup: a bare origin, the main checkout and a worktree for branch "work", each case in its own directory.
setup() {
  n=$((n + 1))
  case_dir="$root/$n"
  mkdir -p "$case_dir"
  git init -q --bare -b main "$case_dir/origin.git"
  git init -q -b main "$case_dir/main"
  cp "$config" "$case_dir/main/.pre-commit-config.yaml"
  echo one >"$case_dir/main/a.txt"
  git -C "$case_dir/main" add -A
  git -C "$case_dir/main" commit -q -m initial
  git -C "$case_dir/main" remote add origin "$case_dir/origin.git"
  git -C "$case_dir/main" push -q origin main
  git -C "$case_dir/main" fetch -q origin
  git -C "$case_dir/main" worktree add -q "$case_dir/work" -b work
}

commit_in() {
  dir=$1
  file=$2
  text=$3
  echo "$text" >"$dir/$file"
  git -C "$dir" add -A
  git -C "$dir" commit -q -m "$file $text"
}

ref() { git -C "$case_dir/main" rev-parse "$1"; }
origin_main() { git -C "$case_dir/origin.git" rev-parse main; }

# gate_in NAME DIR ARGS...: runs the gate and keeps its output; sets st.
gate_in() {
  name=$1
  dir=$2
  shift 2
  st=0
  (cd "$dir" && "$gate" "$@") >"$case_dir/out" 2>&1 || st=$?
  last=$(tail -n 1 "$case_dir/out")
  echo "$name: exit $st, $last"
}

expect_refused() {
  [ "$st" -ne 0 ] || fail "$1: the gate succeeded"
  grep -q '^gate: FAIL' "$case_dir/out" || fail "$1: no FAIL line"
}

expect_unmoved() {
  [ "$(ref main)" = "$2" ] || fail "$1: main moved"
  [ "$(origin_main)" = "$3" ] || fail "$1: origin moved"
}

# A failing merge: the rebase conflicts, is aborted, and nothing moves.
setup
commit_in "$case_dir/main" a.txt main-side
commit_in "$case_dir/work" a.txt work-side
m=$(ref main) w=$(ref work) o=$(origin_main)
gate_in 'failing merge' "$case_dir/main" merge work
expect_refused 'failing merge'
expect_unmoved 'failing merge' "$m" "$o"
[ "$(ref work)" = "$w" ] || fail 'failing merge: the branch was not restored'
[ ! -d "$(git -C "$case_dir/work" rev-parse --git-path rebase-merge)" ] || fail 'failing merge: rebase left in progress'

# A failing check on the branch.
setup
commit_in "$case_dir/work" fail-check yes
m=$(ref main) o=$(origin_main)
gate_in 'failing check' "$case_dir/main" merge work
expect_refused 'failing check'
expect_unmoved 'failing check' "$m" "$o"

# A check that exits 0 without its pass line.
setup
commit_in "$case_dir/work" no-pass yes
m=$(ref main) o=$(origin_main)
gate_in 'check without PASS' "$case_dir/main" merge work
expect_refused 'check without PASS'
expect_unmoved 'check without PASS' "$m" "$o"

# The second check, on main after the fast forward, fails: main goes back.
setup
commit_in "$case_dir/work" b.txt two
touch "$root/fail-main"
m=$(ref main) o=$(origin_main)
gate_in 'failing check after fast forward' "$case_dir/main" merge work
rm -f "$root/fail-main"
expect_refused 'failing check after fast forward'
expect_unmoved 'failing check after fast forward' "$m" "$o"

# A dirty tree in the main checkout, then in the branch's worktree.
setup
commit_in "$case_dir/work" b.txt two
echo stray >"$case_dir/main/stray.txt"
m=$(ref main) o=$(origin_main)
gate_in 'dirty main checkout' "$case_dir/main" merge work
expect_refused 'dirty main checkout'
expect_unmoved 'dirty main checkout' "$m" "$o"
rm "$case_dir/main/stray.txt"
echo edit >>"$case_dir/work/a.txt"
gate_in 'dirty branch worktree' "$case_dir/main" merge work
expect_refused 'dirty branch worktree'
expect_unmoved 'dirty branch worktree' "$m" "$o"

# Merge from a worktree instead of the main checkout.
git -C "$case_dir/work" checkout -q -- a.txt
gate_in 'merge from a worktree' "$case_dir/work" merge work
expect_refused 'merge from a worktree'
expect_unmoved 'merge from a worktree' "$m" "$o"

# A clean merge moves main to the rebased branch and pushes nothing.
gate_in 'clean merge' "$case_dir/main" merge work
[ "$st" -eq 0 ] || fail 'clean merge: refused'
[ "$(ref main)" = "$(ref work)" ] || fail 'clean merge: main is not at the branch'
[ "$(origin_main)" = "$o" ] || fail 'clean merge: origin moved'

# Push refuses a dirty tree.
echo stray >"$case_dir/main/stray.txt"
m=$(ref main)
gate_in 'push of a dirty tree' "$case_dir/main" push
expect_refused 'push of a dirty tree'
expect_unmoved 'push of a dirty tree' "$m" "$o"
rm "$case_dir/main/stray.txt"

# Push refuses a tree that never passed check.
commit_in "$case_dir/main" c.txt unchecked
m=$(ref main)
gate_in 'push of an unchecked tree' "$case_dir/main" push
expect_refused 'push of an unchecked tree'
expect_unmoved 'push of an unchecked tree' "$m" "$o"

# Push refuses personal data, even on a checked tree.
printf '%s\n' "/Users""/wes/notes" >"$case_dir/main/c.txt"
git -C "$case_dir/main" commit -q -am personal
gate_in 'check of the personal tree' "$case_dir/main" check
m=$(ref main)
gate_in 'push of personal data' "$case_dir/main" push
expect_refused 'push of personal data'
expect_unmoved 'push of personal data' "$m" "$o"
grep -q 'personal data in the tree' "$case_dir/out" || fail 'push of personal data: refused for another reason'
git -C "$case_dir/main" revert --no-edit HEAD >/dev/null

# Push refuses main when origin moved on: never a force push.
gate_in 'check before the race' "$case_dir/main" check
git clone -q "$case_dir/origin.git" "$case_dir/other"
commit_in "$case_dir/other" d.txt elsewhere
git -C "$case_dir/other" push -q origin main
o=$(origin_main) m=$(ref main)
gate_in 'push that is not a fast forward' "$case_dir/main" push
expect_refused 'push that is not a fast forward'
expect_unmoved 'push that is not a fast forward' "$m" "$o"
grep -q 'not a fast forward of origin/main' "$case_dir/out" || fail 'push that is not a fast forward: refused for another reason'

# Push refuses a commit that gitleaks flags, even when a later commit removes it.
git -C "$case_dir/main" pull -q --rebase origin main
printf 'token = "%s%s"\n' 'ghp_' 'R8t2kQ7vXw4mZp9LcY3nBf6hJs1dGa5eUo0i' >"$case_dir/main/e.txt"
git -C "$case_dir/main" add e.txt
git -C "$case_dir/main" commit -q -m leak
git -C "$case_dir/main" rm -q e.txt
git -C "$case_dir/main" commit -q -m unleak
gate_in 'check of the leaky history' "$case_dir/main" check
o=$(origin_main) m=$(ref main)
gate_in 'push of a leak' "$case_dir/main" push
expect_refused 'push of a leak'
expect_unmoved 'push of a leak' "$m" "$o"
grep -q '^gate: FAIL: gitleaks' "$case_dir/out" || fail 'push of a leak: refused for another reason'
git -C "$case_dir/main" reset -q --hard origin/main

# A checked, clean fast forward pushes.
commit_in "$case_dir/main" f.txt clean
gate_in 'check before the push' "$case_dir/main" check
gate_in 'clean push' "$case_dir/main" push
[ "$st" -eq 0 ] || {
  cat "$case_dir/out"
  fail 'clean push: refused'
}
[ "$(origin_main)" = "$(ref main)" ] || fail 'clean push: origin is not at main'
grep -q '^gate: gitleaks: exit 0' "$case_dir/out" || fail 'clean push: gitleaks did not run'

if [ "$failures" -ne 0 ]; then
  echo "test-gate: $failures failures"
  exit 1
fi
echo 'test-gate: PASS'
