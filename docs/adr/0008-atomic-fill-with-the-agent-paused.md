# 0008: Atomic fill with the agent paused

Status: accepted, 2026-11-16

## Context

A filled field can be read back by any CDP client or screenshot.

## Decision

Berth pauses the agent's CDP traffic, checks the real origin and form target, fills, submits, wipes on failure, scans held messages for the secret, then resumes. The agent gets only a status.

## Why

Field blurring alone does not protect the value.
