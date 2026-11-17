# 0016: Brand is the Sleeping key

Status: accepted, 2026-11-16

## Context

Berth needs one logo, palette and type set before `Berth.app` (M5) and the `berth-native` dialog ship, so every surface looks like the same product.

## Decision

The logo is the "Sleeping key": a berth that is also a closed eye, holding a key.
The palette is dark first: Navy 800 `#16263F` and Navy 900 `#0A1424` as a gradient, Mist `#F4F7FA` text and Brass `#F2C46D` as the one accent; the light partner is Cream `#FBFAF6`, Ink `#14213D` and Deep brass `#B7791F`.
Fonts are Space Grotesk for the wordmark and headings, Inter for UI and body, and JetBrains Mono or system mono for code.
The kit lives in `brand/`, and app surfaces (`Berth.app` at M5, the `berth-native` dialog) take their colors from `brand/colors/tokens.json`.

## Why

The mark says what Berth does: it keeps the secret and the agent never sees it.
One token file keeps the native app, the dialog and the docs from drifting apart.
