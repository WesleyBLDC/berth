# 0011: Secret stores beyond the Keychain are helper programs

Status: accepted, 2026-11-16

## Context

Users want 1Password, Bitwarden and others.

## Decision

The Keychain is built in; other stores are `berth-secret-<name>` helpers with a JSON stdin and stdout contract, pinned by code signing identity and added with Touch ID.

## Why

Vendor code stays out of the daemon; anyone can add a store.
