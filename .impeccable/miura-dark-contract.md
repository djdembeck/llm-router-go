# Direction Contract — "Vast Miura-Fold Sheet, dark mode"

Seed 5f8d7aae, mode operate, challenger `paper-folds-pleats-deployable-miura-orbit-sheet`,
user-pinned dark mode. This is the committed world; both build slices follow it.
It is the opening-comment contract (also embedded in the emitted markup).

## THESIS
The fleet is a sheet. Every backend is a fold-cell in one tessellated Miura grid,
and the grid undulates with live load. What the operator sees at a glance is a
single continuous surface — the whole fleet's requests, latency, and GPU
pressure as one field of linked creases — not a scatter of unrelated cards.
The category default this refuses: a row of equal dashboard cards or a dark
"near-black + one neon accent + glowing edges" ops board.

## OWN-WORLD (palette, material, type, composition)
- **Palette (dark mode — the committed material, not a toggle):**
  - Ground / sheet: near-black ink `#0A0A0B` (the dark sheet face; the tube/dark ground holds all).
  - Mountain crease (warm gray, primary structural lines + primary signal): `#A6A6A0` (dimmed) → `#E8E6E0` (lit).
  - Valley crease (steel blue, secondary signal + secondary structural): `#8FA9C0` (dimmed) → `#CFE0EF` (lit).
  - Gold-foil accent (THE live-signal line, active/selected, the packet face): `#C9A24B` (muted on dark; brighter `#E4C264` when active/lit). Gold is the only "hot" color; it carries the live trace and the active state, nothing decorative.
  - Sheet shadow / recessed crease: `#161513`.
  - Text: warm off-white `#EDEBE4` (primary), `#A6A29A` (secondary), `#6E6A62` (muted/labels).
  - Reserve: a single desaturated signal-red `#C0564B` ONLY for 429/saturated/rejected state — never for decoration.
- **Material:** matte dark sheet as ground; creases are thin scored lines that catch a
  faint gold or steel highlight on one edge (the "crease score" — a 1px lit edge along
  each fold). Subtle diagonal sheen across the sheet (very low contrast, ~3% light shift
  along the tessellation diagonal). No glassmorphism, no heavy shadows — depth is the
  fold geometry itself.
- **Type:** two faces only.
  - Display/headings: **"Archivo"** (700 italic, tight tracking, condensed feel) —
    the Miura card's headline face. Used for panel titles, the sheet masthead, big numbers.
  - Data/IDs/labels/body: **"JetBrains Mono"** (400/500) — the aerospace-spec mono for
    crease IDs (backend names), all numeric readouts, axis ticks, the request tape,
    status labels. Mono is the voice of the data.
- **Composition:** a parallelogram (rhombus) grid rules every block. The whole sheet sits
  on a subtle isometric skew (the Miura tessellation's diagonal seams); panel headers
  fold along the diagonal seams. Cell corners are sharp (the fold), not rounded. Section
  dividers are crease lines (thin, with a lit edge), not rules.

## FIRST VIEWPORT (the thesis, at scale)
Top: a masthead strip — `llm-router-go` in Archivo italic, a live clock, and a fleet
status line (total in-flight, gpu budget used/max with a tiny crease-gauge, tier0 inflight).
Immediately below: the **deployed sheet** — a tessellated grid of live fold-cells, one per
backend. Each cell's face is a **live small-multiple** (canvas): the last ~30s of a signal
drawn as a gold line over a faint crease-grid, with the current value big in JetBrains Mono
top-right and the signal name in a crease-ID label top-left. The sheet undulates: on load,
and whenever the global "deploy" state changes, the cells fold/unfold in a stepped diagonal
cascade (the Miura deploy). Below the grid: a **fleet strip** (aggregate req/s, ttft, tok-est/s
as three wide cells) and a **live request tape** (a horizontally scrolling mono line of the
most recent requests: `t backend path status ttft dur`, gold for 200, red for 429/5xx).
The very first thing the eye lands on is a LIVE gold line moving on the sheet.

## VISITOR PATH
1. Land → the sheet is already deployed and live (gold lines moving). One glance = whole
   fleet health: is any cell spiking, is the gpu budget cell red, is the tape flashing 429s.
2. Hover a cell → it lifts (the crease rises, a gold lit-edge sweeps the fold) and the
   signal name + current value + limit sharpen; a thin detail readout row appears under the
   cell (in-flight/limit, waiting, prefill, ewma, ttft samples).
3. Click a cell → it expands in place to a large cell (the fold opens wider): the small
   multiple becomes a full-height trace with a secondary overlay (e.g. ttft over req/s) and
   the full per-backend readout column. Click again (or Esc) re-folds it.
4. The masthead **deploy control** (`[ FOLD ]` / `[ DEPLOY ]`) folds the whole sheet back to a
   compact "packet" row (one thin live line per backend, gold) and deploys it back — the
   signature interaction, and the mobile/compact mode.

## SIGNATURE INTERACTION
One pull deploys the sheet: a single control (the gold-foil packet face, top-right of the
masthead) folds/deploys the entire tessellation in a stepped diagonal cascade. It is the
mobile/compact mode AND the moment that makes the surface memorable. Constraint-linked:
the same deploy state drives every cell's fold angle, so one control propagates across the
whole sheet (this is the Miura "one pull deploys a precise field" grammar, kept).

## CROSS-SURFACE REACH
The tessellation + crease-line + gold-signal grammar extends to any future surface:
a per-backend detail view is one opened fold; a settings/config view is the sheet's
"fold-line" side rail; an empty/backends-not-configured state is a **dark hollow packet**
(unfolded, no creases) — not a stock empty-state card.

## HONEST RISK
The isometric skew + per-cell live canvas is the perf and a11y risk: too many simultaneous
canvas traces will drop frames, and the skewed grid must not trap reading. Mitigations are
load-bearing: (a) each cell is ONE canvas, redrawn on the data tick (500ms), not a 60fps
loop; (b) the skew is a CSS transform on the container, not per-pixel distortion, so text
stays crisp and selectable; (c) the tessellation is always available as a flat,
non-skewed, accessible diagram (an ARIA-labelled table/grid of the same numbers) and the
`prefers-reduced-motion` path disables the cascade and afterglow.

## DISCIPLINES RAISED FROM THE DEALT CHALLENGERS (kept lines)
- From the phosphor terminal (declined as world, kept as discipline): **live trace with
  afterglow persistence** — the gold signal line carries a faint fading trail (persistence
  of vision), and the request tape is a continuous scrolling transcript, not a static list.
- From the console dashboard atmosphere (declined, kept as discipline): **selection glow +
  dark hollows for empty** — the opened/active cell carries a warm gold glow; an
  unconfigured or idle backend reads as a dark hollow in the grid, not a greyed card.
- From the one-bit desktop (declined, kept as discipline): **state inverts, disabled dims**
  — a saturated/429 cell inverts (gold/red on the ground), a disabled/absent limit reads as
  a dimmed 50%-density crease.
- The Miura sheet supplies everything else: the tessellated parallelogram grid, the
  crease-score lit edges, the deploy/re-fold cascade, the dark-ink + gold-foil material,
  and the one-control-propagates-across-the-sheet interaction.

## BOUNDARY
This is an Operate surface. The expression never obscures the task: the live numbers must
be legible at arm's length in a dark room, the fold motion must be reversible and must not
delay reading a value, and the skew must never put a number behind another. If a fold effect
makes a value hard to read, the value wins.
