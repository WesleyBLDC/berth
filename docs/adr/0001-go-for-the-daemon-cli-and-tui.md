# 0001: Go for the daemon, CLI and TUI

Status: accepted, 2026-11-16

## Context

Berth needs a long running daemon, a CLI and a TUI on macOS, built mostly with coding agents.

## Decision

Go, with Swift only for native macOS work.

## Why

Single static binaries, simple concurrency, fast builds, ports to Linux later. Swift covers Keychain, Touch ID, ScreenCaptureKit and SMAppService, so Go stays free of cgo. Rust was considered and dropped: macOS only removes its cross compiling advantage.
