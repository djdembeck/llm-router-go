---
name: llm-router-go — Fleet Sheet
description: The Vast Miura-Fold Sheet in dark mode — a tessellated grid of live fold-cells, gold-foil signal on dark ink, one pull deploys the whole fleet.
colors:
  ink-ground: "#0a0a0b"
  recess: "#161513"
  cell-face: "#0e0e10"
  cell-face-hi: "#141416"
  mountain-dim: "#a6a6a0"
  mountain-lit: "#e8e6e0"
  valley-dim: "#8fa9c0"
  valley-lit: "#cfe0ef"
  gold: "#c9a24b"
  gold-lit: "#e4c264"
  text-primary: "#edebe4"
  text-secondary: "#a6a29a"
  text-muted: "#8c867c"
  signal-red: "#d46a5e"
typography:
  display:
    fontFamily: "Archivo, 'JetBrains Mono', sans-serif"
    fontSize: "30px"
    fontWeight: 700
    fontStyle: "italic"
    lineHeight: 1
    letterSpacing: "-0.015em"
  body:
    fontFamily: "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace"
    fontSize: "13px"
    fontWeight: 400
    lineHeight: 1.45
  label:
    fontFamily: "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace"
    fontSize: "10px"
    fontWeight: 500
    letterSpacing: "0.14em"
  data-value:
    fontFamily: "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace"
    fontSize: "30px"
    fontWeight: 500
    lineHeight: 1
    fontFeature: "tnum"
spacing:
  crease-gap: "1px"
  section: "16px"
  page-inline: "28px"
  page-bottom: "48px"
components:
  packet-btn:
    textColor: "{colors.gold}"
    typography: "{typography.label}"
    padding: "8px 14px"
  packet-btn-hover:
    textColor: "{colors.gold-lit}"
  fold-cell:
    backgroundColor: "{colors.cell-face}"
    height: "168px"
    padding: "10px 14px 8px"
  fold-cell-open:
    backgroundColor: "{colors.cell-face-hi}"
    height: "308px"
  fold-cell-pkt:
    height: "46px"
  fleet-cell:
    backgroundColor: "{colors.cell-face}"
    height: "118px"
    padding: "10px 16px 8px"
  pool-bar:
    backgroundColor: "rgba(143, 169, 192, 0.1)"
    height: "8px"
    fill: "{colors.valley-dim}"
  pool-bar-hot:
    fill: "{colors.signal-red}"
    hotThreshold: "90"
  gpu-seg:
    backgroundColor: "rgba(143, 169, 192, 0.1)"
    width: "12px"
    height: "9px"
  gpu-seg-on:
    backgroundColor: "{colors.gold}"
  switcher-btn:
    textColor: "{colors.text-muted}"
    typography: "{typography.label}"
    padding: "4px 9px"
  switcher-btn-on:
    textColor: "{colors.gold-lit}"
---

# Design System: llm-router-go — Fleet Sheet

## Overview

**Creative North Star: "The Miura Fleet Sheet"**

The fleet is a sheet. Every backend is a fold-cell in one tessellated Miura grid, and the
grid undulates with live load — what an operator sees at a glance is a single continuous
surface, the whole fleet's requests, latency, and GPU pressure as one field of linked
creases, not a scatter of unrelated cards. The world is dark-ink: a matte near-black sheet
(#0a0a0b) scored with warm-gray mountain creases and steel-blue valley creases, carrying a
single gold-foil live signal. The first thing the eye lands on is a live gold line moving
on the sheet.

This is an operate-in-situ surface: an ML-infrastructure operator checks it while traffic
is live, at arm's length, in a dark room. Density is high but disciplined — the data is the
decoration. Every number is the router's actual in-flight state (never a stale or derived
approximation), and the honesty boundary is visual: token figures are estimated prefill and
always wear a gold `est` label; measured figures say so. The committed refusals, carried
from the world contract: a row of equal dashboard cards, and the dark "near-black + one
neon accent + glowing edges" ops board — here gold is a signal line, not a glow, and the
sheet is one field, not a card grid.

**Key Characteristics:**

- One tessellated field of sharp-cornered parallelogram fold-cells under a shared isometric skew (-6deg)
- Gold-foil (#c9a24b) as the only hot color: the live trace, the active state, nothing decorative
- Depth by fold geometry, not shadows: recessed 1px seams, 1px crease-scored lit edges, a ~3% diagonal sheen
- Archivo 700 italic for the masthead; JetBrains Mono for every number, ID, label, and line of the tape
- One pull deploys the whole field: the `[DEPLOY]`/`[FOLD]` packet control cascades every cell in a stepped diagonal (55ms per cell)
- The same sheet folds to a thin packet row — the mobile/compact mode is the fold, not a separate layout

## Colors

A dark-ink palette of warm near-blacks, two crease families (warm gray and steel blue) in
observed dim/lit pairs, one gold-foil signal color, and a single reserved signal-red.

### Primary
- **Gold Foil** (#c9a24b, lit #e4c264): the live signal. Every live trace is drawn in gold
  with its newest third lit; the active/selected states (hover sweep, opened fold glow,
  switcher `.on`, focus outline, selection background) are gold. The gold-foil packet
  control carries it. Gold never decorates — if a gold pixel isn't a signal or an active
  state, it doesn't belong.
- **Signal Red** (#d46a5e): reserved for 5xx / saturated / offline — the
  failure conditions. A saturated fold-cell inverts to red-on-ground (red top
  crease + red inset + faint red halo); `st-err` on the tape, the waiting GPU
  segment's red top border, `health[data-h="offline"]`, and the STALE feed-loss
  banner. A 429 on the tape is NOT red — it is valley blue (`st-throttle`): a
  capacity rejection with `Retry-After` is the policy working as designed, not
  a failure. The contrast floor is 4.5:1 on every dark surface (verified
  against #0a0a0b, #161513, #0e0e10, #141416).

### Secondary
- **Valley Crease — Steel Blue** (#8fa9c0 dim, #cfe0ef lit): the secondary signal and
  secondary structure. The TTFT overlay line, the dashed limit line in traces, T1 tier
  chips, the GPU gauge's empty segments, the diagonal seams inside each trace canvas.

### Tertiary
- **Mountain Crease — Warm Gray** (#a6a6a0 dim, #e8e6e0 lit): the primary structural
  lines. Crease lines, sheet/fleet/tape borders (at reduced opacity), cell top-crease lit
  edges, the crease-ID labels, the big values (lit warm gray is the "hot" data color when
  it isn't saturated).

### Neutral
- **Ink Ground** (#0a0a0b): the dark sheet face; page and hollow state.
- **Recess** (#161513): the folded-under ground; shows through the 1px grid gaps as the seams between cells.
- **Cell Face** (#0e0e10): a deployed fold-cell's face; fleet cells.
- **Cell Face Hi** (#141416): the raised face — hover and opened folds.
- **Text Primary** (#edebe4): primary text, readout values, tape backend names.
- **Text Secondary** (#a6a29a): status lines, paths, secondary numerals.
- **Text Muted** (#8c867c): labels, axis-adjacent text, idle states, `.k` keys.

### Named Rules

**The One Hot Color Rule.** Gold is the live signal and nothing else. Its rarity — thin
lines, small lit edges, one button — is the point. A screen with a gold surface, a gold
block, or gold used for emphasis of a non-signal value has left the world.

**The Red Reservation Rule.** Red appears only on 5xx / saturated / offline —
the failure conditions, plus the STALE feed-loss banner. A 429 on the tape is
valley blue, not red: it is a capacity rejection, the bounded-failure policy
working as designed. If a red pixel isn't a failure condition, delete it.

## Typography

**Display Font:** Archivo (700 italic, with JetBrains Mono fallback) — loaded from Google Fonts, the only italic face in the system.
**Body Font:** JetBrains Mono (400/500) — the aerospace-spec mono; the voice of the data.

**Character:** Two faces, strict division of labor. Archivo 700 italic exists only for the
masthead wordmark (`llm-router-go`, 30px, tight at -0.015em) — it names the sheet.
Everything an operator reads is mono: crease IDs, every numeral, the tape, the readout.
Numbers are always tabular (`font-variant-numeric: tabular-nums`) so live values don't
jitter as they tick.

### Hierarchy
- **Display** (Archivo 700 italic, 30px, line-height 1, -0.015em): the masthead title
  only; 24px under 720px. The `-go` suffix is gold, not italic-extra — it's the foil stamp.
- **Data Value** (Mono 500, 30px, line-height 1, tabular): the fold-cell's current
  in-flight count, big top-right of the cell face.
- **Fleet Value** (Mono, 26px, line-height 1, tabular): the fleet strip's aggregate numbers.
- **Clock** (Mono, 21px, tabular, 0.02em): the live wall clock in the masthead right.
- **Body** (Mono 400, 13px, line-height 1.45): readout rows, fleet foot, feed notes.
- **Label** (Mono 500, 10px, 0.14em, uppercase): panel labels, fleet-cell labels, tape
  head, health chip.
- **Crease ID** (Mono 500, 11px, 0.08em, lit mountain): the backend name top-left of each
  cell, with the tier chip.
- **Detail** (Mono, 10px, 0.04em): the hover detail row under the trace.

### Named Rules

**The Est Label Rule.** Token figures are estimated new-prefill tokens, never measured
decode throughput. Anywhere a token number appears, a gold `est` (9px, 0.1em) sits beside
it: the fleet strip label, the readout rows, the tape's `~<n>` column. A token figure
without `est` is a lie about the data.

**The Tabular Rule.** Every live number uses tabular numerals. A ticking value that shifts
horizontal weight has broken the sheet's calm.

## Layout

A centered 1320px frame (`max-width: 1320px`, padding 18px 28px 48px; 14px inline / 36px
bottom under 720px). The page is a single vertical stack: masthead → feed row → the sheet
→ fleet strip → engine pools band → request tape → sessions sheet → (sr-only flat table). The sheet
and every block below it
live on a shared isometric skew: `skewX(-6deg)` with `transform-origin: 50% 0`, applied to
the *containers* (`.sheet`, `.fleet-strip`, `.tape-wrap`, `.hollow`, `.fleet-foot`) — never
to individual text, which is counter-skewed (`.unskew`, `skewX(6deg)`) so it stays crisp,
horizontal, and selectable. That pair — skewed container, unskewed text — is the load-
bearing mitigation for the skew.

The sheet is a CSS grid: `repeat(auto-fit, minmax(252px, 1fr))` with a **1px gap**, and the
grid background is the recess color — so the seams between cells are 1px recessed creases,
not borders. The fleet strip is `repeat(4, 1fr)`, same 1px seam treatment. An opened fold
cell spans the full row (`grid-column: 1 / -1`). The opened cell is a two-column grid
(trace column + 272px readout column, 18px gap), collapsing to one column under 720px.

Responsive (single breakpoint, 720px): the masthead stacks (title left, status and clock/
control row below), the fleet strip goes single-column, the opened fold's readout drops
under the trace, and — in JS, not CSS — the sheet auto-folds to the packet row at narrow
width: narrow viewport means folded state, the compact mode is the fold itself.

Cell heights are a fixed scale: deployed 168px, opened 308px, packet 46px. Section
spacing between the sheet/fleet/tape is 16px (margins), the masthead's internal gaps are
12–20px, cell-face padding is 10px 14px 8px.

## Elevation & Depth

**No shadows at rest.** Depth is the fold geometry itself, never ambient blur. A deployed
cell sits flush on the face color; what makes the field read as a sheet is (a) the 1px
recessed seams between cells, (b) each cell's 1px top crease — a lit warm-gray
`border-top: 1px solid rgba(232,230,224,0.1)` plus a 1px `box-shadow: 0 1px 0
rgba(232,230,224,0.05)` score under it — and (c) a ~3% diagonal sheen across the whole
sheet (`linear-gradient(115deg, transparent 34%, rgba(232,230,224,0.03) 48%, transparent
62%)`). Crease divider lines are the same idea in 1px: a `rgba(166,166,160,0.28)` scored
line with a 1px lit edge (`rgba(232,230,224,0.1)`) along its lower side.

Shadow and glow appear **only in response to state**, and only in the exact values the
system uses (see sidecar `shadows`):

- **Hover lift:** the crease rises — `translateY(-3px)`, `0 10px 26px rgba(0,0,0,0.55)`,
  an `inset 0 1px 0 rgba(228,194,100,0.55)` gold lit-edge, plus a 2px gold lit-edge sweep
  across the top (background-position travel, 0.8s).
- **Opened fold (selection):** warm gold — a 1px `rgba(228,194,100,0.4)` ring, a 48px
  `rgba(201,162,75,0.16)` halo, and the black drop.
- **Saturated (429):** red-on-ground inversion — `inset 0 1px 0 rgba(192,86,75,0.5)` and a
  22px `rgba(192,86,75,0.1)` halo.

### Named Rules

**The Fold-Is-Depth Rule.** If a surface isn't reacting to hover, selection, or failure,
it carries no shadow and no glow. Ambient depth is geometry — seams, crease scores, the
sheen — not blur. Glassmorphism, heavy drop shadows, and decorative glows are outside
this world.

**The Value-Wins Rule.** A fold effect that makes a value hard to read loses to the value.
The skew must never put a number behind another number; the cascade must not delay reading
a value; text inside the skew is always counter-skewed.

## Shapes

Sharp corners, everywhere, always: **zero border-radius in the entire system** — the fold
is sharp, and rounding a cell corner would un-fold it. The recurring silhouette is the
**parallelogram**: cells are rectangles rendered as parallelograms by the container's
`skewX(-6deg)`, and the GPU gauge segments use a steeper individual skew (`skewX(-18deg)`,
12×9px) as miniature fold pieces. Section dividers are **crease lines** — 1px scored folds
with a lit edge — not rules. Inside every trace canvas, the Miura tessellation is drawn as
two diagonal seam lines (`rgba(143,169,192,0.09)`) plus vertical time ticks every 10s
(`rgba(166,166,160,0.12)`), so the small multiple reads as a scope on the sheet, not a
widget. The hollow (empty) state is an *unfolded tessellation*: the skewed ground plane
with faint 115°/65° repeating seam lines and no cells — an empty state that is still the
sheet.

## Components

### Masthead
**Character:** the sheet's header strip — who, when, and how full the fleet is, in one
line's worth of glances. `llm-router` in Archivo 700 italic 30px lit mountain with a gold
`-go`; a 10px uppercase subtitle (0.14em, muted) underneath. Center: a 11px status line
(0.05em, secondary) — in-flight / waiting / tier0 counts in lit mountain 500, dot-
separated, plus the GPU crease-gauge. Right: the live clock (21px tabular) and the packet
control. The masthead is *not* skewed — it's the flat margin of the sheet; everything
below it deploys under the skew.

### The Packet Control — `[DEPLOY]` / `[FOLD]`
**Character:** the signature interaction. One gold-foil button that folds or deploys the
entire tessellation in a stepped diagonal cascade; the same control is the mobile/compact
toggle.
- **Shape:** sharp rectangle (0 radius), 11px 500 uppercase 0.14em, padding 8px 14px.
- **Rest:** gold text (#c9a24b) on a vertical gold-foil gradient
  (`rgba(201,162,75,0.16) → rgba(201,162,75,0.04)`), 1px gold border
  (`rgba(201,162,75,0.55)`) with a brighter top edge (`rgba(228,194,100,0.9)`) — the crease score.
- **Hover:** text goes gold-lit, `0 0 18px rgba(201,162,75,0.28)` glow +
  `inset 0 1px 0 rgba(228,194,100,0.4)`; transitions 0.25s.
- **Behavior:** `aria-pressed` reflects deployed state; label reads `[FOLD]` while
  deployed, `[DEPLOY]` while folded.

### Fold Cell
**Character:** one backend, one fold — the core unit of the sheet. A live small multiple
with its crease ID and current value; hover lifts the crease; click opens the fold in
place.
- **Rest:** face #0e0e10, 168px min-height, 1px lit top crease, `10px 14px 8px` padding.
  Top row: crease-ID (11px 500, 0.08em, lit mountain, with tier chip — 9px, steel-blue
  border; King chips are gold-bordered) and the value (30px 500 tabular, lit mountain,
  `n/max` with the max at 13px muted). Below: the trace canvas (30s window — the
  operator's glance unit). Bottom: a hidden 10px
  detail row (inf/wait/prefill/kv/ewma/ttft) that fades in on hover and `:focus-visible`.
  When the cell is saturated a persistent 10px story line carries the state's meaning:
  `queue full · 429 + retry-after — bounded, clears on its own` (the 429 in valley blue —
  the inversion is status, not alarm).
- **Hover:** translateY(-3px), face-hi background, deep shadow + gold inset top, and the
  2px gold lit-edge sweep across the fold (0.3s opacity, 0.8s travel).
- **Saturated:** `.is-saturated` — red top crease (`rgba(192,86,75,0.85)`), red inset +
  halo, value in red. State inverts, it doesn't just tint. And the inversion carries its
  story: a persistent 10px line — `queue full · 429 + retry-after — bounded, clears on
  its own` (the `429` in valley blue, never red) — so the loudest state on screen says
  *policy working*, not *broken*.
- **Opened:** spans the row, **fixed at 308px** (`min-height = max-height`: the readout
  scrolls inside, the sheet never grows, and the opened scope keeps the same glance
  scale as the collapsed cell), face-hi, gold ring + 48px halo. The face is replaced
  by a full-height trace (1px `rgba(166,166,160,0.18)` border) with a signal switcher
  above in two crease-labeled groups — `router` (in-flight / req/s / ttft / tok-est /
  bytes/s) and `engine · /metrics` (engine run / engine prefill / engine decode, shown
  only when the backend's /metrics feed is live) — and the `fold ✕` control at the
  row's right end, visually distinct from a signal option. The history window (30s /
  60s / 5m, default 60s, over a 5-minute buffer) is a small 3-button group at the
  legend row's right end — it scales what you are looking at, so it sits with the
  legend, not with the signal choice. A legend below (gold swatch for the primary
  signal, steel-blue for the ttft overlay), and a 272px readout column that leads with a
  **live-state block** (in-flight / queue / prefill / ewma / ttft), then a **rates**
  group, then the **engine** block (scraped from the backend's own /metrics — the
  grouped engine readout: requests · real / tokens · real / cache / latency · real /
  capacity; see the Engine Readout entry below), then a **lifetime totals** group
  collapsed behind a `＋` toggle — progressive disclosure, so a 15-second glance
  sees state first. The engine block is long, so the readout column **scrolls**
  (thin scrollbar, no glow) against the opened fold's fixed 308px height —
  scroll is depth's price, the value still wins: nothing overlaps, every number
  is tabular and horizontal. The `fold ✕` button is at the
  switcher row's right end, visually distinct from a signal option. `role="button"`,
  `tabindex=0`, Enter/Space toggle, Esc refolds and returns focus to the cell (a
  keyboard user is never stranded at `<body>`).
- **Packet:** when the sheet is folded (or the viewport is narrow), the face is replaced
  by a 46px single column: 86px name, a 26px bare trace (30s window, no crease grid),
  value 15px right. Tapping a packet row expands it to 72px with an inline readout
  (inf / wait / prefill) — on mobile the packet row is data, not a dead tap. The sheet
  becomes `grid-template-columns: 1fr` — one thin live line per backend.

### Engine Readout
**Character:** the opened fold's engine block — the backend's own /metrics truth,
grouped under small crease sub-headers (9px uppercase muted, 1px crease above each):
- **top rows (no header):** running, waiting (+ `cap N / defer M` split when vLLM
  reports it), retracted (SGLang).
- **requests · real:** lifetime completed requests, the finished-reason breakdown
  inline on one row (stop / length / abort / error — the error number is the only
  thing that may go red, and only when it is > 0), aborted (SGLang), preempted
  (vLLM), streaming / batch completed split (SGLang).
- **tokens · real:** compute-only prefill tok/s, cache-hit prefill tok/s, decode
  tok/s, lifetime prompt and generation token totals (compact 2.4M / 132k), and the
  gen:prompt ratio. Every figure here is *measured* — none carry `est`.
- **cache:** kv cache pressure % (with the full / SWA / mamba pool split for
  SGLang), pool used / capacity in tokens (+ free · evictable), mamba pool
  used / cap, prefix-cache hit rate (+ lifetime hits / queries for vLLM), mm-cache
  hit rate, hicache host offload, and memory (KV GB · weights GB).
- **latency · real:** TTFT (with the stream / batch split for SGLang), ITL, e2e,
  queue, prefill / decode stage means (vLLM), per-token ms, and the average
  request shape (mean prompt in / mean gen out).
- **capacity:** SLO running-request cap and context length (SGLang).

Fields the engine does not report render as `—` or the row is absent — family
differences (vLLM vs SGLang) are silent, never faked. When the endpoint is
absent the whole block is two honest lines (`status: off — …`, `source: …/metrics`).

### Trace Scope
**Character:** a scrolling oscilloscope, not a stepped chart — the gold line glides
because a requestAnimationFrame loop advances the virtual clock at frame rate while data
arrives on 500ms server ticks, with samples plotted at their *server* timestamps (skew-
free). Every visible segment is one uniform color — gold (#c9a24b, α 0.9, 1.5px) for the
primary signal, steel blue (#8fa9c0, α 0.45) for the ttft overlay — so a signal stays
recognizable across the full history; there is no afterglow fade, which made long lines
unreadable and made each segment's position in the window guessable by brightness alone.
The newest sample is a lit head (2px dot + 5px α 0.35 halo in the signal's lit shade).
The y-axis eases toward its target scale (lerp 0.15/frame) instead of snapping. A limit,
when set, is a dashed 4/4 steel-blue line with a 9px mono value label. A secondary
overlay (ttft) draws in steel blue at 0.55× alpha on the same time axis. Canvas is
devicePixelRatio-aware (capped at 2×); the whole engine is `aria-hidden` — the numbers
are read from the table, not the scope.
**Motion honesty:** under `prefers-reduced-motion: reduce` the rAF scroll is disabled and
the trace redraws only on data ticks, anchored to the newest sample.

### Fleet Strip Cell
**Character:** six wide aggregate cells (req/s, ttft, tok-est/s, decode-tok/s · real,
cache hit · real, kv peak · real) on
the same skewed grid as the sheet — 118px min-height, `10px 16px 8px` padding, face
#0e0e10, 1px seams. Label 10px uppercase muted (with gold `est` on the prefill cell,
steel-blue `real` on the four measured cells), value 26px tabular lit mountain (the kv
peak value goes red at ≥90 — the engine's own saturation threshold), and a live
trace under each (gold for req/s and tok-est; steel-blue for ttft, the real decode
throughput, cache hit, and kv peak — engine truth is never gold). The est/real pairing is the honest data boundary made visible: the body-
based prefill estimate and the engine's measured token throughput sit side by side. The
10px muted footnote below the strip (also skewed) states what is estimated, what is
scraped from the engines' /metrics, and what is measured.

### Engine Pools Band (kv / mamba cache)
**Character:** the backends' memory pools, read from the engines' own /metrics — the
engine's side of the est/real boundary. A skewed band on the sheet (same 1px seams,
zero radius) between the fleet strip and the request tape: `ENGINE POOLS · KV / MAMBA
CACHE` head with a `real` tag and the source note ("scraped from the backends' own
/metrics — the engines' gauges, not the router's estimate"), then one cell per backend
(min 252px auto-fit grid, 118px min-height, `10px 16px 8px` padding) carrying:
- **crease-gauge pool bars** — kv, and for SGLang the full / SWA / mamba split: an 8px
  valley-blue bar (`scaleX` fill, 0.5s eased; no rest glow) in a 44px-label / bar /
  64px-value grid, tabular % on the right. Mamba carries `used/cap` in the value column
  when the engine reports token counts, % otherwise. Family differences are silent — a
  vLLM backend simply has no swa/mamba rows.
- **foot row** — `hit 32%` (prefix-cache hit rate), `pool 1.2M/2.5M`, `evict 102.2k`
  (only when nonzero), 10px muted keys / tx-2 tabular values.
- **a 5-minute kv-pressure scope** under the foot — the same `Trace` scope as the fleet
  strip, steel blue, `yMax=100`, so the bar's current value has a history.
**Color law:** this band is valley-only. The engines' gauges are *measured* engine
truth, not the router's live signal — gold never appears in this band (gold is the
router's line, not the engine's). Red is the saturation reservation: a pool at ≥90%
goes red (bar fill + value) — the engine's own retraction/eviction threshold, the same
red the saturated fold-cell uses. Absence is never red: an engine without /metrics
shows one muted `no /metrics` line in its cell; with no engines reporting at all, the
band keeps its shell and one muted `no engine pools` line.
**Accessibility:** every bar is `role="img"` with a `kv cache N% used` aria-label —
the gauges are real state, not decoration.

### Request Tape Entry
**Character:** the continuous transcript — the last requests as one horizontally scrolling
mono line, edge-masked (`transparent → black at 3%/97%`), duplicated half for a seamless
loop, paused on hover **and on touch** (touch-down pauses, touch-up gives a 1.5s grace
before resuming — there is no hover on a phone). One entry: `t backend path status ttft dur ~tokens /` at 11px
tabular, 8px gaps, 18px entry padding. Colors by field: time muted, backend text-primary,
path text-secondary, status 500-weight and semantic — gold for 200 (`st-ok`), valley blue
for 429 (`st-throttle`, a capacity rejection — not red), red for 5xx/4xx errors (`st-err`)
— numerals secondary, separator muted at 0.5. The header row (10px uppercase muted)
carries the field legend **above the tape** (`t · backend · path · status · ttft · dur · tok(est)`),
a `429s+ only` toggle that filters the transcript to rejections, and a `spikes n/5s`
counter that goes red when nonzero and carries a 60s `peak` so the evidence does not
decay away before it is read. When the feed drops, the tape freezes in place (the
transcript is still the last truth). Idle state: one muted line — "the sheet is quiet."

### Sessions Sheet
**Character:** the router's conversation memory, under the tape — the in-flight
request stack and the live sessions, as one more skewed band on the sheet (same
`skewX(-6deg)` container, counter-skewed content, 1px creases, zero radius). The
header row reads `sessions · in flight · N` (the count is gold when N > 0 — the
live signal) with the est note `token figures · est` at the right end.
- **In-flight stack:** one row per live request, oldest on top (the operator reads
  the stack like a queue: whatever is on top has been waiting longest). Each row:
  ticking elapsed time (1s clock, `m:ss` after a minute), backend, path (truncated),
  the session's 12-hex prefix id (muted; `—` when the request is not a chat
  conversation), a phase chip — `admitted` in valley blue (queued, not yet
  streaming) and `streaming` in gold (the live signal) — then `ctx N est` and
  `new N est` (body-based estimates, est-labeled per the rule).
- **Session rows:** one conversation per row, sorted active-first then
  most-recent. Identity is the conversation's **prefix hash**: sha1 of the message
  history up to (excluding) the last turn, first 12 hex chars — a new conversation
  forked off an old one gets its own id at its second turn. Active sessions (a
  live request, or a turn in the last 2 minutes) carry a 2px **gold left edge** —
  the only gold edge at rest, because it *is* the live signal — and a `live` gold
  chip instead of the age (`m:ss`). Then: `{n} reqs`, `ctx N est`, `cached N est`,
  average ttft, lifetime `N tok est`, and the last status chip: 2xx gold, **429
  valley blue** (capacity rejection, the policy working), 5xx red (the only red
  allowed).
- **Expansion:** clicking a row opens its per-request table in place (one open at
  a time): `t (HH:MM:SS) · st · dur · ttft · ctx est · new est · cached est ·
  out est · tok/s est` at 11px tabular rows under 1px creases, 9px uppercase
  muted header. Request detail is only carried while the session is fresh —
  older sessions say so in one muted line, they do not fake history.
- **Honesty:** every token figure here is a body-based estimate (~4 bytes/token)
  and wears its gold `est`; durations and statuses are measured. When the feed is
  absent (router build without the sessions endpoint), the band shows one muted
  line — `no session feed` — absence is not failure, so it is never red.

### Stale Banner (feed loss)
**Character:** the moment the truth stops arriving is the moment the page must be
loudest. When no frame has arrived in >6s, the sheet keeps its last numbers (a frozen
truth is more honest than an empty frame) and a red banner takes the place between the
feed row and the sheet: `stale — last data HH:MM:SS · Ns ago` on the left, the failing
endpoint on the right (`re-arming /metrics/stream`). The banner is the only red that is
not a per-backend failure — it is the offline reservation applied to the feed itself.
The feed row also carries a persistent `last data HH:MM:SS` anchor so the age of the
numbers is always readable, and the flat table's caption notes the data is stale. The
health chip states are `live` (gold), `degraded` (on a fallback feed — not "paused",
which reads like a user-controlled state), and `offline` (red).

### Hollow Packet
**Character:** the empty state is an *unfolded tessellation*, not an empty-state card. A
skewed 240px-min plane on the ink ground, framed by a 1px `rgba(166,166,160,0.22)` crease,
with faint 115° warm-gray and 65° steel-blue repeating seam lines (1px every 38px, α ~0.05)
— the sheet with no cells deployed. The message (12px, 0.12em, muted, counter-skewed)
states the honest reason: no backends configured, or the feed is re-arming.

### The Flat Table (accessibility diagram)
The tessellation is always available as a flat, non-skewed, accessible diagram: an
`sr-only` `<table>` carrying the same numbers per backend (tier, in-flight, limit,
waiting, prefill, ewma, ttft, req/s, tok/s est) with a caption naming it. It is
`display:block; width:1px` so it can never produce horizontal overflow while remaining a
real table for assistive tech. It is not optional decoration — it is the a11y half of the
honest-data boundary and ships with the sheet.

## Do's and Don'ts

### Do:
- **Do** use gold (#c9a24b / #e4c264) only for live signals and active states: the trace line and its lit head, hover/selection edges, the packet control, focus outlines, and the `est` label. Engine-scraped data (engine pools band, `real` fleet cells) is valley blue — the engines' gauges are not the router's live signal.
- **Do** counter-skew (`.unskew`) every text element inside a skewed container, so numbers stay horizontal, crisp, and selectable.
- **Do** keep a gold `est` label on every token figure (fleet strip, readout, tape column) — token numbers are estimated prefill, not decode.
- **Do** use tabular numerals on every value that ticks.
- **Do** keep the sr-only flat table next to the tessellation; the sheet must always have its non-skewed accessible twin.
- **Do** draw traces as a scrolling scope (server timestamps, eased y-scale, lit newest 30%, afterglow fade) on 500ms data ticks.
- **Do** honor `prefers-reduced-motion: reduce` by disabling the deploy cascade, the undulation, the hover sweep, and the rAF scroll — the trace then redraws on ticks only.

### Don't:
- **Don't** use gold for decoration, emphasis, or any surface — one hot color, live signal only.
- **Don't** use red (#d46a5e) for anything except 5xx / saturated / offline state and the STALE feed-loss banner; a 429 stays valley blue on the tape.
- **Don't** round a corner. Zero radius is the fold; the parallelogram comes from skew, not border-radius.
- **Don't** let the skew obscure a value — no number behind another number; if a fold effect delays reading a value, the value wins.
- **Don't** add ambient shadows, glassmorphism, or glows at rest; shadow/glow is a state response (hover lift, opened-fold ring, saturated inversion) in the exact documented values.
- **Don't** present token throughput as measured; measured figures are req/s, bytes/s, durations. A 429 is a rejection, not an error — it stays blue.
- **Don't** run a per-cell 60fps loop — one rAF-driven scope per trace instance, redrawn on data ticks otherwise.
- **Don't** build the compact/mobile view as a separate layout — it is the folded sheet (packet row), driven by the same deploy state.
