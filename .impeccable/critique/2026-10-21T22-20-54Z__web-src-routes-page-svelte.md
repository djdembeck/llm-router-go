---
target: entire web page
total_score: 25
max_score: 40
na_heuristics: 
p0_count: 0
p1_count: 2
p2_count: 2
target_identity: "file:/home/djdembeck/projects/github/llm-router-go/web/src/routes/+page.svelte"
target_fingerprint: "sha256:9e7736141881c195230ed6aebd025e8d3f823aaa1fcb8b99a4fb5af40332cb59"
target_path: /home/djdembeck/projects/github/llm-router-go/web/src/routes/+page.svelte
timestamp: 2026-10-21T22-20-54Z
slug: web-src-routes-page-svelte
---
## Design Health Score

| # | Heuristic | Score | Key issue |
|---|-----------|------:|-----------|
| 1 | Visibility of System Status | 3/4 | LIVE/PAUSED/STALE, feed source, and last-data time are clear; stale recovery gives no reason or next step. |
| 2 | Match System / Real World | 3/4 | Source labels build trust, but T0, KV HIT, and ENG MEM require domain recall. |
| 3 | User Control and Freedom | 3/4 | Pause, range, scrub, and Return to Live work; the specified L/Esc shortcuts are missing. |
| 4 | Consistency and Standards | 3/4 | Source colors and panel patterns are coherent; live-session labels drop to 8-9px. |
| 5 | Error Prevention | 3/4 | Mock, missing-engine, and stale states are distinct; high pressure has no interpretation cue. |
| 6 | Recognition Rather Than Recall | 3/4 | Estimates are labeled, but abbreviations and the screen-reader chart gap still require recall. |
| 7 | Flexibility and Efficiency | 2/4 | Range, slider, sorting, and CSV help experts; no visible keyboard accelerators. |
| 8 | Aesthetic and Minimalist Design | 2/4 | The survey sheet is cohesive, but the readings, sessions, and table compete for attention. |
| 9 | Error Recovery | 2/4 | Stale truth is held, but the cause and useful next step are absent. |
| 10 | Help and Documentation | 1/4 | Tooltips help; there is no visible, task-focused help. |
| **Total** |  | **25/40** | **Acceptable (62.5%)** |

## Design Specificity Verdict

Strongly product-specific. The Fleet Profile's backend lanes, KV ridges, request bars, live edge, and cursor are a grammar for shared-GPU inference, not a generic ops template. Source tags (router/engine/device/est), explicit mock/stale treatment, and prefill/queue signals carry the product. The strongest missed opportunity is that the load-bearing King/Subject vocabulary is reduced to T0/T1 in the shell.

Deterministic scan: file detector over web/src returned 0 findings / 23 files. The live overlay reported 210 anti-patterns (211 detail rows): 207 undersized functional text at 8-10px, 4 tiny-text, 1 repeating-stripes gradient. The stripe finding is a false positive: it is the intentional null-data hatch. The 11px overlay threshold flags the deliberate 10px design-system label layer broadly; the genuine failures are the live-session 8-9px labels.

## Overall Impression

A distinctive, credible live-operations surface with unusually legible data provenance. Its biggest gap is not visual identity but dependability at the edges: a 390px horizontal overflow, no backend-series equivalent for assistive technology, and microtype below the design floor.

## What's Working

- The Fleet Profile earns its signature role: lanes, live edge, cursor, and request marks express backend capacity rather than decorating a generic chart.
- Data honesty is visible: source tags, DEV MOCK, held stale truth, and est boundaries are legible.
- The desktop composition fits the job: readings, profile, and live sessions share the first viewport; the request log is secondary.

## Priority Issues

1. [P1] 390px horizontal overflow. The document is 524-552px wide in a 390px viewport. Browser evidence isolates the root: the table.sr-only history table in FleetProfile.svelte (516px wide) is not clipped by the global visually-hidden style. Fix: wrap the table in a clipped sr-only container (or otherwise contain its table layout) and verify scrollWidth === innerWidth at 390px. Suggested command: $impeccable adapt web/src/routes/+page.svelte.

2. [P1] No accessible backend-series equivalent for the Fleet Profile. The canvas is aria-hidden; the existing sr-only table covers device history, not backend time series. Slider stepping is not an overview. Fix: add a clipped, labeled table summarizing each backend lane over the visible window - layer, last, avg, min, max - and the cursor value when pinned. Suggested command: $impeccable audit web/src/routes/+page.svelte.

3. [P2] Live-session metadata is below the type floor. Phase and metric keys are 8px; age and est are 9px. Fix: bring every live-session label to the 10px floor; keep estimated values at 10px and measured TTFT at 12px, preserving the recent size reduction while restoring legibility. Suggested command: $impeccable typeset web/src/lib/LiveSessions.svelte.

4. [P2] Tier roles are numerals only. The UI says T0/T1 while King/Subject is the product's admission vocabulary. Fix: show KING/SUBJECT beside the tier in the rail and backend detail, with the number in the title. Suggested command: $impeccable clarify web/src/lib/FleetRail.svelte.

## Persona Red Flags

- Alex (Power User): controls are plentiful but no shortcuts are visible; L/Esc are specified but absent.
- Sam (Accessibility-Dependent): the signature chart has no backend-history equivalent; the backend table's row select is click-only (tr onclick).
- Casey (Distracted Mobile): the 390px page scrolls sideways; six range buttons compete with a one-handed glance.

## Minor Observations

FleetProfile does not implement the brief's L/Esc return-to-live. Backend-table row selection is not keyboard-operable. The stale banner holds truth but names no cause. Six history ranges and ten sortable headers exceed the four-choice guideline. Help is tooltip-only. The day sheet and the 1191px stacking preserve the world cleanly.

## Questions to Consider

- Keep all six ranges visible for expert speed, or reveal long ranges behind a secondary control?
- Should KV saturation add an explicit operator cue beyond the hot state?
