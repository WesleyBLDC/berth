---
name: berth-milestone
description: "Start, plan, split, run or close a Berth milestone (M2, E2 and so on): write the plan with acceptance tests, cut it into contract first and parallel work items for several agents, check the safety checklist, record ADRs, and update the wiki. Use when beginning or finishing a milestone, or when asked to plan or parallelize Berth work."
metadata:
  author: wesley
---

# Running a milestone

The implementation plan on the wiki (https://wiki.wesleybldc.com/wiki-artifacts/berth/implementation-plan) lists each milestone's builds, tests and gate.

## Start

1. Write `docs/plans/<id>-<name>.md` from the wiki entry: goal, builds, layout, acceptance tests (each one concrete enough to fail today), exit gate. Restate the constraints to Wesley in about three bullets and wait for his yes before code when the milestone creates something new.
2. Split it with `docs/agentic-work.md`: one **contract item** first (proto messages, port interfaces, state and event enums, migration numbers reserved), then **parallel items** that each own separate packages, then an **integration item** that wires the shell and adds e2e tests.
3. Each item names its file plan, the acceptance tests it owns, and the skill to load.

## During

- The contract item lands alone before parallel items start.
- Every item ends with `make check` passing and a local commit; nothing is pushed.
- A reviewer (`berth-reviewer`) reads each item's diff; a tester (`berth-tester`) proves user facing behaviour with the real binaries.

## Close

1. All acceptance tests pass in `make check`.
2. Walk the agent safety checklist (https://wiki.wesleybldc.com/wiki-artifacts/berth/agent-safety) for what the milestone added and note the result in the plan file.
3. Add or supersede ADRs for every decision the milestone made or changed.
4. Update the wiki implementation plan: mark the milestone done with the commit, the acceptance results and the findings.
