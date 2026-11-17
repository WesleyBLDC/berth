# 0012: Agents can store and use secrets, never read them

Status: accepted, 2026-11-16

## Context

Coding agents need secrets in terminals without seeing them.

## Decision

Storing goes through a native dialog the person types into; using goes through env injection pinned to a command, a header proxy, or a browser fill. Reading is a human only action with Touch ID, to the clipboard.

## Why

Removes manual Keychain chores without putting values in transcripts.
