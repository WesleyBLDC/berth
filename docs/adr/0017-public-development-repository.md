# 0017: Public repository for development

Status: accepted, 2026-11-16

## Context

ADR 0014 kept the repository local until the open source milestone (M8).
Developing in the open makes review, backups and outside reading easier, and the repository already keeps personal values out of code by rule.

## Decision

The repository is public at `WesleyBLDC/berth` for development, and this supersedes the "nothing is pushed until M8" part of ADR 0014.
Changes are pushed directly to `main` after `make check` passes and the change is reviewed.
There are no releases or tags until M8, so nothing is presented as ready to install.
Personal values still hardcoded today (the signing team, bundle identifiers, profile names and paths) move to local config over time.
`ARCHITECTURE.md` is the canonical architecture document; the maintainer's private wiki mirrors it, not the other way round.
Secrets, personal paths and private notes are never pushed.

## Why

The code is already written to be publishable at any time, so keeping it private bought nothing but friction.
One canonical document in the repository keeps public readers and coding agents on the same facts.
