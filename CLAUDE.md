@AGENTS.md

## Claude Code specifics

- Repo skills live in `.agents/skills/` and are linked into `.claude/skills/`; load the one that matches the change before you start.
- Project subagents live in `.claude/agents/`: `berth-worker` builds one item in a worktree, `berth-reviewer` reads a diff and reports blockers only, `berth-tester` proves a change end to end with the real binaries.
- Hooks in `.claude/settings.json` lint every file you edit and send problems back; fix them in the same turn instead of moving on.
- Set `DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer` for Swift and signing commands (the Makefile does this).
- Worktree shells may refuse compound commands; put multi step work in a script under `scripts/` or run steps one by one.
- Subagents default to Opus 5.5 at medium effort.
- A worker reports `make check`'s last line and never pushes; the lead rebases the branch onto `main`, runs `make check` again, and pushes only after it printed `check: PASS` on that commit.
