---
target: dashboard
total_score: 28
max_score: 40
na_heuristics: 
p0_count: 0
p1_count: 2
timestamp: 2026-08-21T07-20-55Z
slug: web-src-routes-page-svelte
---
# Critique — llm-router-go dashboard (fleet sheet)

Method: dual-agent (A: CritiqueA · B: CritiqueB)
Target: web/src/routes/+page.svelte · slug web-src-routes-page-svelte
Mode: Operate · World contract: DESIGN.md (Vast Miura-Fold Sheet) + PRODUCT.md

## Design Health Score (28/40 — Good)

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Feed chip, last-data anchor, spike counter present; per-cell state is only the red inversion — no "queued, will clear" state distinct from saturated |
| 2 | Match System / Real World | 4 | Fluent operator vocabulary: in-flight, tier0, ewma, kv cache, est/real; honesty rule enforced visually |
| 3 | User Control and Freedom | 3 | Esc refolds + returns focus, distinct fold ✕ exit, 429s+ toggle, reversible deploy; no way to pin a fold while scrolling its readout |
| 4 | Consistency and Standards | 2 | Masthead/feed-row/stale-banner counter-skew against the sheet (verified live, matrix(1,0,0.105,1,0,0) on non-skewed ancestors); everything else cohesive |
| 5 | Error Prevention | 3 | Read-only surface; prevention = honest labeling (est/real, — for absent, `no session feed` not fake red) |
| 6 | Recognition Rather Than Recall | 3 | Tape legend, inline est labels, self-describing rows; collapsed-cell detail row is hover/focus-only (invisible at rest); 12-button switcher relies on known grouping |
| 7 | Flexibility and Efficiency | 2 | Full keyboard open/close/Esc, 3 windows, signal switcher, 429 filter; no shortcuts for the core loop, one fold at a time, no deep-link, no two-backend compare |
| 8 | Aesthetic and Minimalist Design | 3 | Near-total discipline (zero radius, red reservation, gold-as-signal all verified by scan); noise is density: opened readout is a 35-row wall; masthead skew errors tilt the first-glance band |
| 9 | Error Recovery | 3 | Stale banner excellent (frozen truth + red time anchor + re-arming); 429 stays blue; gaps: no recovery guidance on engine feed `off` beyond two honest lines, `degraded` chip unexplained |
| 10 | Help and Documentation | 2 | fleet-foot explains est/real, tape carries its legend; zero onboarding for the signature interactions — the page's best affordance (open fold) has no hint beyond cursor:pointer |
| **Total** | | **28/40** | **Good** |

## Design Specificity Verdict

**LLM (A):** Authored for this product, emphatically. The Miura fold-cell, skewed tessellation with 1px recessed seams, DEPLOY/FOLD cascade, gold-as-only-hot-color, the est/real color boundary, and 429-as-valley-blue are load-bearing translations of this router's actual mechanics (tiered admission, bounded queues, body-based estimate vs engine-scraped truth). Category-interchangeable residue is thin: the request tape (generic marquee transcript), fleet strip (aggregate KPI tiles — saved by the est/real pairing), and session rows (conventional expandable table). Two signature moments were broken in the build (masthead skew, opened-fold height), undercutting the specificity in the running product.

**Deterministic scan (B):** CLI detector exit 0 (0 findings) over web/src. In-browser detector: 96 findings / 9 rules — undersized-ui-text ×74, tiny-text ×4, wide-tracking ×2, all-caps-body ×2, dark-glow ×7, layout-transition ×3, nested-cards ×2, em-dash-overuse ×1 (advisory), marquee ×1. 8 of 9 rule groups are committed DESIGN.md decisions (10px mono labels, 9px est tag, 0.08em tape tracking, uppercase panel labels, state-only zero-offset glows, min-height fold cascade, tessellation-not-cards, em-dash field separators). The one non-false-positive — marquee (unpausable scroll) — is REFUTED: RequestTape pauses on hover, on touch-down with 1.5s grace, and pins under reduced-motion (code-verified). Net true detector findings: 0.

**Visual overlays:** injected on a [Human]-labelled tab; 86 overlay boxes rendered; console capture was unreachable through the exposed browser API — fallback signal: `window.impeccableScanAsync()` structured results (verbatim) + overlay screenshot.

## Overall Impression

The strongest design work in this repo — a committed world executed with unusual discipline. What drags it is its own load-bearing geometry: the top band (the operator's first glance) leans against the sheet, and the signature interaction (open a fold) breaks the page's physics by expanding ~1081px instead of the committed 308px. Biggest opportunity: fix the two skew/height bugs and give saturation a story; the surface goes from "good with blemishes" to "the reference."

## What's Working

1. **The est/real honesty boundary as a color system.** Gold `est` vs steel `real` on every token figure across four bands; the fleet strip sits the estimated prefill rate and engine-measured decode rate side by side with a footnote stating what is measured. The product's core principle made visually enforceable.
2. **429 is valley blue, everywhere.** Tape, sessions, request table — capacity rejection rendered as policy working, while 5xx takes the only red allowed. Red reservation held on full scan.
3. **The folded sheet IS mobile.** At 390px the sheet auto-folds to 46px packet rows with expandable inline readout; DEPLOY hidden in narrow mode rather than lying; empty state is an unfolded tessellation. One sheet, every state.

## Priority Issues

1. **[P1] Masthead, feed row (and stale banner) are counter-skewed against the sheet.**
   - What: `.unskew` (= skewX(6deg)) on `.mast-mid`, `.mast-right`, `.feed-row`, `.stale-banner` — none inside a skewed container (`.masthead` is flat by contract: "the flat margin of the sheet"). Verified live: computed `matrix(1,0,0.105104,1,0,0)` on all three at 1560px and 390px.
   - Why: the first-glance band tilts the opposite direction from everything beneath it; reads as mis-assembly at exactly where the eye starts; violates the "skewed container, unskewed text" invariant.
   - Fix: remove `unskew` from `.mast-mid`, `.mast-right`, `.feed-row` in +page.svelte and the stale-banner element; keep `.unskew` only where the nearest transformed ancestor is actually skewed.
   - Note: the same bug class was suspected on `.fleet-foot` — REFUTED. It is a sibling of the skewed strip (not a descendant), carries exactly one −6deg skew, and aligns with the strip. Correct as built.
   - Command: `$impeccable layout`
2. **[P1] The opened fold is ~1081px tall, not the committed 308px; the readout never scrolls.**
   - What: `.fold-cell.is-open` sets `min-height: var(--cell-h-open)` with no cap; the 35-row readout grows the cell to ~1081px (screenshot-verified), `readout` overflow-y:auto never engages, and the trace stretches to ~1000px of scope — destroying the 30s/60s glance scale. The fleet strip, tape, and sessions are pushed off-screen.
   - Why: the signature interaction changes the page's physics and makes the opened scope non-comparable to the collapsed cell's; progressive disclosure (totals behind ＋) was designed around a fixed 308px viewport.
   - Fix: cap `.fold-cell.is-open` at `height: var(--cell-h-open)` (grid row locked), give `.cell-open` a fixed track, let `.readout` own the scroll (CSS already there). Re-test the 720px single-column collapse.
   - Command: `$impeccable layout`
3. **[P2] Saturation has color but no story.**
   - What: `.is-saturated` inverts the cell red-on-ground (frequent on the subject backend), but nothing within one glance says what it means: bounded queue full → 429 with Retry-After → policy working. The proof (blue 429s) is three bands down, different color, no cross-reference.
   - Why: red is reserved for failure; the operator's read of a red cell is "broken" even though the design's own semantics say "the bounded queue is doing its job." Trust tax at the highest-stakes moment.
   - Fix: on `.is-saturated`, show a persistent 10px line: `queue full · 429 + Retry-After` (valley-blue 429 per the rule), and/or a small `N 429/min` counter fed from the tape's existing spike logic.
   - Command: `$impeccable clarify`
4. **[P2] The 12-button switcher row overloads the opened fold's first decision.**
   - What: 5 router signals + 3 engine signals + 3 windows + 1 exit in one flat ~972px row; window (temporal) shares a row with signal (what-to-watch) choices; engine group is crease-separated but unlabeled.
   - Why: a wall of same-weight buttons at the moment of the signature interaction — the exact cognitive-load checklist failure; 12 ≫ 4 visible options.
   - Fix: two labeled crease sub-headers (router / engine, 9px muted matching `.grp`), window group moved to the legend row's right edge, `fold ✕` kept distinct at the far right.
   - Command: `$impeccable distill`
5. **[P3] No onboarding for the signature interactions.**
   - What: nothing says cells open, the detail row is hover-only, FOLD is the mobile mode, or what the GPU segments count.
   - Why: best features are discoverable only by accident; in a dark room the affordances are the only teacher, and the teaching is weak.
   - Fix: one-time 10px muted hint line under the sheet (`click a fold to open · hover for detail · esc refolds`), dismissed on first open; surface the GPU gauge's used/budget in a `title` tooltip. No tour.
   - Command: `$impeccable onboard`

## Persona Red Flags

**Alex (Impatient Power User)** — loop "see saturation → open King fold → check rejections":
- Opening one fold HIDE the rest of the sheet (P1-2): tape, sessions, fleet strip scroll away — exactly the context wanted co-located during an incident. Biggest Alex-killer.
- Cannot compare: one fold at a time; King vs Subject side by side (the tier-spillover question) is impossible.
- No keyboard path to "filter 429s" (button sits far-right of the tape head, off-screen at mobile width); 3–4 clicks from open to "engine decode at 5m."

**Sam (Screen reader / keyboard only)** — loop "confirm fleet health, inspect one backend":
- Fold cell `aria-label` is static ("vllm-king — 2 in flight") and never updates; live numbers exist only in the sr-only flat table, which is orphaned (nothing links it to the sheet).
- Hover-only detail row: a tabbing user hears the stale label, not the inf/wait/prefill/kv/ewma/ttft metrics.
- The request tape — the band an operator most wants line-by-line — has no accessible equivalent (role="presentation", no text alternative, no keyboard pause; hover/touch pause only).
- Contrast defensible (measured: muted 5.05–5.48:1, red 5.53:1, valley 7.9:1); `gpu-seg.wait` is a 1px red top border on a 12×9px segment — sub-pixel at arm's length, saved only by the `+N waiting` text.

## Minor Observations

- DESIGN.md's Trace Scope/Do's still say "lit newest 30%, afterglow fade"; Trace.svelte deliberately dropped it (uniform color, no afterglow) — the doc is stale, the code is right.
- SessionSheet polls /metrics/sessions at 1s in parallel with the SSE frame: `in flight · N` can disagree with the masthead's in-flight for up to a second.
- Tape `spikes n/5s` — no definition of "spike" anywhere; one word in the header would do it.
- `pkt-id` fixed 86px with ellipsis starves the trace at 390px, where the name is the only identity.
- The dev mock saturates the subject ~80% of the time: the default first impression of the shipped UI is a permanently red cell. Calm-traffic-with-occasional-storms would demo both states.
- Flat table: 16 live columns, no `aria-live` — a screen reader user who reads it once gets a snapshot. Acceptable for v1.
- `.hollow` is skewed (correct per contract) but was unreachable on the live mock (feed always on) — untested in practice.

## Questions to Consider

1. The opened fold replaces the fleet glance with one backend's depth — is the real incident flow "two cells plus the tape"? If so, opening a fold that draws the OTHER cells' traces as overlays on the same time axis beats a row-span that hides everything.
2. Red is reserved for failure, but bounded-failure (429) is the product's success condition under contention — should saturation get its own third treatment (gold-lit "busy" cell, red only on the queue metric)? As built, the calmest policy-compliant state is the reddest state on screen.
3. The two skew bugs are the same class: `.unskew` applied to a non-skewed ancestor. Would a viewport-level skew (one transform, zero per-element counter-skews) remove the bug class entirely — at the cost of the per-cell cascade transform?
4. Who is the arm's-length reader — desk operator or walk-past wall display? The 10px hover-detail and 9px group headers answer one and not the other.
