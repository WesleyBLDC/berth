@AGENTS.md

## Claude Code specifics

- Repo skills live in `.agents/skills/` and are linked into `.claude/skills/`; load the one that matches the change before you start.
- Project subagents live in `.claude/agents/`: `berth-worker` builds one item in a worktree, `berth-reviewer` reads a diff and reports blockers only, `berth-tester` proves a change end to end with the real binaries.
- Hooks in `.claude/settings.json` lint every file you edit and send problems back; fix them in the same turn instead of moving on.
- Set `DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer` for Swift and signing commands (the Makefile does this).
- Worktree shells may refuse compound commands; put multi step work in a script under `scripts/` or run steps one by one.
- Subagents default to Opus 5.5 at medium effort.
- A worker reports `make check`'s last line and never pushes; the lead runs `scripts/gate.sh merge <branch>` from the main checkout, then `scripts/gate.sh push`, never a hand written chain of rebase, check, merge and push.
