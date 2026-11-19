# 0002: Connect API with one Protobuf schema

Status: accepted, 2026-11-16; the Swift client line is superseded by [ADR 0019](0019-berth-app-speaks-connect-json-over-the-socket.md) (the app speaks Connect JSON over HTTP/1.1, not grpc-swift)

## Context

Four clients (CLI, TUI, Mac app, MCP shim) share one daemon API with requests and streams.

## Decision

A Connect server in Go with one `berth.v1` schema managed by `buf`. Go clients use `connect-go`; the Swift app uses `grpc-swift` over the Unix socket.

## Why

One schema generates every client and `buf breaking` protects them; plain JSON would drift between Go and Swift. Fallback if Swift streaming over the socket fails in M0: loopback TCP with a token file.
