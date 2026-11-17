# 0013: Testing: minimal unit tests, real end to end tests

Status: accepted, 2026-11-16

## Context

Fast feedback and real confidence.

## Decision

Unit tests for transition tables and parsers; end to end tests with real headless Chrome, Playwright MCP and Chrome DevTools MCP through Berth. A canary test plants a fake secret and fails if it reaches anything agent facing.

## Why

Most bugs here live at the edges with real browsers.
