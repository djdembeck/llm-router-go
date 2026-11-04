---
target: all changed design surfaces in this branch
total_score: 32
max_score: 40
na_heuristics: 
p0_count: 0
p1_count: 1
target_identity: "file:/home/djdembeck/orca/workspaces/llm-router-go/screen-size-optimizing/web/src/routes/+page.svelte"
target_fingerprint: "sha256:be80870fcb0fe67f4fd0774f680dc4e5c4a59363250b96d89960e65e1af5e203"
target_path: /home/djdembeck/orca/workspaces/llm-router-go/screen-size-optimizing/web/src/routes/+page.svelte
timestamp: 2026-10-28T22-09-34Z
slug: web-src-routes-page-svelte
closed: true
---
# Critique — llm-router-go dashboard, branch screen-size-optimizing (run 2026-10-28)

Method: dual-agent (A: CritiqueA design review · B: CritiqueB detector/browser evidence)
Target: entire web page (web/src/routes/+page.svelte + web/src/lib components)

## Design Health Score
| # | Heuristic | Score | Key issue |
|---|---|---:|---|
| 1 | Visibility of System Status | 4 | LIVE/PAUSED/DEV MOCK chips, feed source + last-data time, per-cell source tags with printed windows, held· pin. Genuinely excellent. |
| 2 | Match System / Real World | 4 | Domain lexicon throughout; survey world is annotation, never interference. |
| 3 | User Control and Freedom | 3 | Pause, range, layer, cursor L/Esc, log toggle, theme — nothing missing, nothing exemplary. |
| 4 | Consistency and Standards | 4 | One control grammar; 2px focus offsets; labelled slider with aria-valuetext. |
| 5 | Error Prevention | 3 | Read-only surface; all interactions non-destructive — vacuously adequate. |
| 6 | Recognition Rather Than Recall | 3 | Hover/pin readout great; rail-row grammar must be learned cold. |
| 7 | Flexibility and Efficiency | 2 | Keyboard covers the cursor only; range, pause, layer, theme, log are mouse-only; no rail→filtered-log route. |
| 8 | Aesthetic and Minimalist Design | 3 | Disciplined, dense; nine 10px-labelled cells heavy on first contact. |
| 9 | Error Recovery | 3 | STALE band, engine-error row titles, re-arming list — designed correctly; not renderable in mock. |
| 10 | Help and Documentation | 2 | Tooltip + footer gloss only; thin for an operator meeting the sheet cold. |
Total: 32/40 — Good (80%)

## Design Specificity Verdict
Strongly product-specific. The data-honesty apparatus (source tags + measurement windows on every figure, est at token-figure level, — and 45° hatch for absence, 429 kept water-colored by the One Failure Rule) is the quiet carrier; the Fleet Profile is a grammar for shared-GPU contention, not chart-kit decoration. Character blurs only at 1280-tall, where the compressed canvas's live-edge spot heights run close to lane baselines.

Deterministic scan: CLI detector over web/src = 0 findings (exit 0; fonts.css link warning noted). In-page overlay: 210 findings — 205 undersized-ui-text (10px, the committed Label step, intentional), 5 tiny-text, 1 cream-palette (day-sheet ground, briefed), 1 repeating-stripes-gradient (null-data hatch, known false positive). The 8–9px live-session microtype flagged on 2026-10-21 no longer renders — that P2 is fixed. No net new detector defects on this branch.

## Overall Impression
The fold cap lands the band at 768.0px at 1280/1366×768 — to the pixel — with the canvas above its 320px floor and the session list flexing. Its biggest gap: the cap is built from a constant chrome offset, not the band's actual top, so the STALE banner's 34px insertion breaks the fold during feed loss — exactly when the first viewport matters.

## What's Working
- Measurement honesty as a system: source + window on every figure, est tags, —/hatch, 429 in water.
- First-viewport live band complete at all ≥1280 desk sizes; the cap shares height with the canvas.
- The survey cursor: click-to-pin, arrow-walk, L/Esc, T HH:MM:SS / KV n% readout, clipped accessible twin table.

## Priority Issues
1. [P1] Fold cap's constant --band-offset (213/258px) breaks the fold when the STALE banner or history notice mounts above the band (band bottom ~802 > 768 at 1280/1366×768; server-mode identity row flex-wrap grows the real offset). Fix: derive offset from the band's measured getBoundingClientRect().top via ResizeObserver/effect; keep min(643px, …) cap. Command: $impeccable polish (measure-cap in +page.svelte).
2. [P2] No saturation figure in the first viewport for a 429/90%-KV event: rail vermilion bar is 1D, 5m 429 count lives below the fold in the collapsed REQUESTS header. Fix: 5m 429 count (water tag) beside q N in FleetRail + 429 n footed on the lane spot height in FleetProfile when non-zero. Command: $impeccable polish.
3. [P2] Pinned readout mirrors companion KV at full 12px (TTFT 698ms / KV 698%) — reads as duplication glitch. Fix: demote companion to data-detail/muted with ·, or label both. Command: $impeccable polish.
4. [P3] 10px label floor small at 1366×768 100% scale — committed Label step; recorded as intentional exception, changing requires a DESIGN.md decision.
5. [P3] Expert loop mouse-pinned: no keys for range/pause/layer/theme/SHOW; no rail-row → filtered request-log route. Needs a dedicated wiring pass.

## Persona Red Flags
- Alex (Power User): range segmented control and readings-strip layer buttons are mouse-only; pin → compare-layer → check-log takes two mouse trips plus a scroll.
- Sam (Accessibility-Dependent): session-row identity (path, session prefix) only in title attributes; chart's accessible twin carries stats, not the series.
- Riley (on-call operator): STALE banner + fold cap starts the stalled-feed play one scroll late; KV ≥90% has no first-viewport figure.

## Minor Observations
- 390px overflow P1 from 2026-10-21 verified fixed (scrollWidth = clientWidth = 390).
- 1440+: band ends 44px above fold (cap inert, 643px sheet) — lower band starts by a seam, not a preview.
- Log SHOW→HIDE re-trade 560→320→560px, no canvas jump; theme localStorage round-trip verified.
- Phone top bar wraps to three lines; DAY alone on the last.
- Reduced-motion: CSS 0.01ms + canvas matchMedia snap code-verified only, not render-exercised.

## Questions to Consider
- Should the STALE banner slot in place (become the band's top rule) instead of pushing the band down?
- Is oldest-first the right bulk direction for the live session stack when the newest row is the actionable one?
- Server mode (258px offset, identity row wrap) is untestable in the mock — is a MOCK_BACKENDS knob worth adding?
