# 0005: Ports and adapters

Status: accepted, 2026-11-16

## Context

The core must be testable without Chrome or macOS APIs, and grow new browsers and stores.

## Decision

The core defines ports (ProcessSource, BrowserDriver, SecretStore, Approver, Store, Clock, Notifier); adapters live outside it.

## Why

Fakes make tests fast and deterministic; Firefox, Linux or new stores are new adapters, not rewrites.
