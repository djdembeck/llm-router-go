---
name: llm-router-go — Capacity Survey
description: A night survey sheet for a GPU-aware inference fleet — sand level lines, water KV, spot heights, and a live session stack.
colors:
  ink-ground: "#10130e"
  survey-panel: "#151a13"
  survey-panel-raised: "#1a2018"
  survey-panel-active: "#20261d"
  chalk: "#e9e5d8"
  chalk-secondary: "#b9b5a7"
  chalk-muted: "#8f8c80"
  survey-line: "#b9a684"
  survey-line-lit: "#d3c2a2"
  water: "#7fa3c4"
  water-lit: "#a7c6e2"
  moss: "#8fae74"
  moss-lit: "#b0cc96"
  amber: "#d6a445"
  amber-lit: "#e8c071"
  vermilion: "#e07a66"
  day-ground: "#eceadf"
  day-panel: "#f4f2e9"
  day-ink: "#171a13"
  day-survey-line: "#7d5f42"
  day-water: "#37627f"
  day-moss: "#476438"
  day-amber: "#8a5a12"
  day-vermilion: "#a33a28"
typography:
  display:
    fontFamily: "Overpass, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: "20px"
    letterSpacing: "-0.01em"
  body:
    fontFamily: "Overpass, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: "20px"
  detail:
    fontFamily: "Overpass, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: "16px"
  label:
    fontFamily: "Overpass, system-ui, sans-serif"
    fontSize: "10px"
    fontWeight: 600
    lineHeight: "12px"
    letterSpacing: "0.12em"
  data-value:
    fontFamily: "Overpass Mono, ui-monospace, monospace"
    fontSize: "24px"
    fontWeight: 500
    lineHeight: "28px"
    letterSpacing: "-0.01em"
  data-detail:
    fontFamily: "Overpass Mono, ui-monospace, monospace"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: "16px"
spacing:
  s1: "4px"
  s2: "8px"
  s3: "12px"
  s4: "16px"
  s5: "20px"
  s6: "24px"
  s8: "32px"
components:
  topbar:
    backgroundColor: "{colors.survey-panel}"
    textColor: "{colors.chalk}"
    height: "56px"
    padding: "8px 20px"
  button:
    backgroundColor: "transparent"
    textColor: "{colors.chalk-secondary}"
    typography: "{typography.label}"
    padding: "5px 9px"
  button-on:
    backgroundColor: "{colors.survey-panel-active}"
    textColor: "{colors.chalk}"
  reading-cell:
    backgroundColor: "{colors.survey-panel}"
    textColor: "{colors.chalk}"
    padding: "8px 12px 9px"
  profile-panel:
    backgroundColor: "{colors.survey-panel}"
    textColor: "{colors.chalk}"
    height: "456px"
  live-session-row:
    backgroundColor: "{colors.survey-panel}"
    textColor: "{colors.chalk-secondary}"
    padding: "8px 12px"
  data-table-header:
    backgroundColor: "{colors.survey-panel}"
    textColor: "{colors.chalk-muted}"
    typography: "{typography.label}"
  data-table-row:
    backgroundColor: "{colors.survey-panel}"
    textColor: "{colors.chalk-secondary}"
    padding: "6px 8px"
---

# Design System: llm-router-go — Capacity Survey

## Overview

**Creative North Star: "The Capacity Survey"**

The fleet is terrain. This is an operate-in-situ surface for the engineer who
keeps inference latency predictable while several backends contend for the
same GPUs. It borrows the national topographic sheet's discipline — level
lines, spot heights, grid references, a marginal scale — and applies it to
live telemetry. The result is a dark survey sheet that reads at arm's length
in a dim room, with a day sheet that inverts the same roles for daylight.

The world enters through four carriers only: type, palette, density, and one
signature move. The shell stays a standard web operations surface — a top
bar, a fleet rail, ruled tables, segmented controls, a range slider. The
signature is the **Fleet Profile**: one elevation lane per backend, its KV
level drawn as a sand ridge over a water fill, live requests riding below as
prefill/decode bars, spot heights at the live edge, and a survey cursor that
scrubs every reading. Lanes are categorical; the sheet never interpolates
between backends to invent a contour.

The honesty boundary is visible on every figure. `router` means measured at
the router (slots, queue, first-byte TTFT), `engine` means scraped from the
backend's Prometheus `/metrics`, `device` means an optional DCGM/nvidia
exporter, and `est` means the body-based ~4 bytes/token estimate. Stale data
freezes in place and says so; a missing engine is `—` and hatched, never a
guess.

**Key Characteristics:**

- A dark olive-ink survey sheet with a paired cream day sheet
- Sand level lines, water KV, moss live, amber estimate, vermilion failure
- Overpass + Overpass Mono; every figure tabular, every type size on an integer 2px step
- Sharp corners, 1px rules, no ambient shadow, no glow
- The Fleet Profile is the history chart; no second chart style competes with it
- Live sessions sit beside the profile in the first viewport
- Frozen truth on feed loss, explicitly labelled `stale`
- Scroll panels clip their own rows; a live burst never paints into the next band
- Measured values ease to the next sample; the live edge glides; rows keep identity through reflow

## Colors

A full palette with named jobs, not an accent over neutral. Night is the
primary sheet; day inverts ground and ink while keeping each role.

### Primary

- **Survey Line — Sand** (#b9a684, lit #d3c2a2): the structural level line.
  The KV ridge, the TTFT ridge, the fleet lane label, the live-edge square,
  the range slider thumb, and the `device` data-class tag. Sand is a survey
  stroke, never a decorative highlight.
- **Water — KV / Engine** (#7fa3c4, lit #a7c6e2): the KV fill and the
  `engine` data-class. The fill is water at 16% over the panel; the level
  line above it is sand. `prefillCache` and KV pools use the same family.
- **Moss — Live / Router** (#8fae74, lit #b0cc96): the live/healthy chip,
  the streaming phase, the `router` data-class, and successful request
  status.
- **Amber — Attention / Estimate** (#d6a445, lit #e8c071): the `est` tag,
  the prefill bar, queued/waiting emphasis, and the `PAUSED`/`DEV MOCK`
  chips.

### Failure

- **Vermilion** (#e07a66 night, #a33a28 day): 5xx, saturated pools (≥90%),
  offline, the STALE banner's square, and failure ticks in the profile. A
  `429` is **not** vermilion; it is a capacity rejection, so it stays water.

### Neutral

- **Ink Ground** (#10130e): the page.
- **Survey Panel** (#151a13): bars, sheets, tables.
- **Raised** (#1a2018): hover, selected rows, the paused readout.
- **Active** (#20261d): pressed/selected cells and table selection.
- **Chalk** (#e9e5d8), **Secondary** (#b9b5a7), **Muted** (#8f8c80): text
  hierarchy. Muted is the floor at 5.2:1 on the panel; never go lighter.
- **Rule** (rgba(233,229,216,0.10)) and **Rule Strong** (0.18/0.30): the
  1px survey rulings.

### Named Rules

**The Source Rule.** Every figure carries its source: `router`, `engine`,
`device`, or `est`. A number without a source is a lie about the data.

**The One Failure Rule.** Vermilion is failure only. A 429 stays water; a
stale feed is the one feed-level failure; a saturated KV pool is the one
capacity failure.

**The Day-Sheet Rule.** The day sheet is not a separate design. It swaps
ground, panel, ink, and the four role colors; it never changes layout,
density, type, or the profile's grammar.

## Typography

**Display Font:** Overpass (400–700) — a workhorse grotesk with
transit-signage DNA; the wordmark and all labels.
**Data Font:** Overpass Mono (400–600) — every number, unit, identifier,
axis label, and table cell.

**Character:** The two faces divide labor strictly. Overpass names things and
carries the small tracked labels; Overpass Mono carries measurement. There is
no italic and no display face. Numerals are always tabular so a ticking value
does not shift its neighbours.

### Hierarchy

- **Display** (Overpass 600, 16/20, -0.01em): the wordmark only; `-go` in sand.
- **Data Value** (Mono 500, 24/28, -0.01em, tabular): readings strip values
  and the largest number in a panel.
- **Panel Value** (Mono 500, 16/20): backend names, table values, session ages.
- **Body** (Overpass 400, 14/20): the reading sublines and explanatory copy.
- **Detail** (Overpass 400, 12/16): data notes, feed metadata, table footers.
- **Label** (Overpass 600, 10/12, 0.12em, uppercase): section heads, column
  heads, data-class tags, controls.
- **Data Detail** (Mono 400, 12/16, tabular): axis labels, token figures,
  secondary measurements.

### Named Rules

**The Integer-Step Rule.** Type sizes use only the 10/12/14/16/24/28 steps.
Line heights use 12/16/20/28/32. No fractional sizes or line heights.

**The Tabular Rule.** Every measured figure is mono and tabular. A value that
changes width while ticking has broken the sheet.

**The No-Serif Rule.** Overpass and Overpass Mono only. A serif, a display
face, or an italic would pull the sheet back toward the editorial cliché.

## Layout

A centered 1760px frame; the top bar spans it, the body is a 232px fleet rail
plus a fluid main column. Main order: ruled readings strip → live band
(Fleet Profile 1fr + live sessions 400px) → backend detail sheet → request
feed sheet. The page has 12px gaps between sheets and 12–20px inline padding.

The readings strip is six equal cells separated by 1px rules at ≥1200px,
3×2 at 960–1199, and 2×3 below 640. The device readings form a second three-
cell row only when device telemetry is present. Each cell is a button when
it corresponds to a profile layer; the pressed state is a 2px inset sand edge
along the bottom.

The profile is a canvas 320px tall (280px at 640–959, 240px below 640). Its
left gutter is 150px (122px below 520px): a 46px lane-label column, a 38px
scale column, then the plot. The time axis carries 3–6 labels; the lane
baselines are 1px rules; the scale labels are 10px mono.

The live sessions panel is 400px wide beside the profile at ≥1280px; below
that it stacks beneath the profile, above the detail sheets. It keeps 6–8
rows visible and scrolls.

The backend detail and request feed are stacked below 1600px; at ≥1600px
they sit side by side (1.4fr / 1fr). Tables use
fixed column widths on desktop and drop the secondary columns below 640px
(KV pool, engine memory, decode, hit rate) while the request feed drops the
path column.

Responsive composition is explicit:

- ≥1280: rail 232 + main; profile and sessions side by side.
- 960–1279: rail 200; live band stacks.
- 640–959: rail becomes a horizontal fleet strip above main; readings 3×2;
  profile 280px.
- <640: top bar wraps; readings 2×3; rail rows stack; profile 240px; session
  rows keep age/phase/backend/id (path hidden); tables drop secondary columns; every
  control has a ≥44px hit area; no horizontal page overflow.

## Elevation & Depth

The sheet is flat. There is no ambient shadow, no glow, no glass, and no
gradient. Depth comes from three things only: the 1px rule, the water fill
under a level line, and the paper ground showing through the 12px gaps
between sheets. Hover and selection are tonal (raised/active panel colors),
not lifted. The one inset mark is the 2px sand bottom edge on the selected
reading.

### Named Rules

**The Flat-Sheet Rule.** If a surface is not reacting to state, it carries
no shadow. A zero-offset glow is decoration and never belongs here.

## Shapes

Zero border-radius everywhere. The sheet's geometry is the ruled rectangle:
1px borders, 4px square markers for status/deltas, 8px swatches, 2px bars for
requests and the prefill track, 5–8px round dots only as canvas marks at the
live edge and cursor. Hatching is a 45° 1px line at 14px pitch for missing
data. There are no pills, no skews, no fold angles, and no ornamental
parallelograms. The profile's lane is the only recurring custom silhouette:
a horizontal band bounded by a baseline and a level line.

## Components

### Motion

The focal moment is the Fleet Profile's continuous live edge: the sheet
slides under a fixed right edge (1s linear, canvas painted at ~30fps, stopped
when scrolled out of view). Measured readings tween to their next sample over
480ms/1.6s with an ease-in-out cubic; the exact value stays in the screen-reader
text and no metric overshoots. TTFT and engine throughput read a 15s rolling mean
(refreshed every 5s) and tween over 1.6s; the KV gauge is the latest sample
held 5s and also tweens over 1.6s. Counts stay live and tween at 480ms. The
window is printed on the cell (`15s` / `5s`), so the number is never mistaken
for a per-scrape reading. Live session rows appear without an entrance and FLIP over 220ms when
the stack reflows; the request table updates instantly with no row fades or
FLIPs, so a burst never flashes. Controls change color/border in 160ms; the
night/day sheet cross-fades in 240ms. There is no page-load choreography or
mount dim. A range change dips the canvas for 240ms. Under
`prefers-reduced-motion`, the edge and values snap, JS row transitions run at
zero duration, and CSS transitions/animations collapse to 0.01ms.

### Top Bar

The sheet's header. Left: the wordmark (`llm-router-go`, `-go` sand) with a
10px subtitle. Center-left: health chip (`LIVE` moss, `DEGRADED` amber,
`OFFLINE` vermilion), optional `PAUSED` and `DEV MOCK` chips, feed source and
last-data time. Right: wall clock (12px mono), a six-button range segmented
control, pause/play, and a theme button. Height 56px; 1px rule bottom;
`survey-panel` ground.

### Readings Strip

One ruled row, no cards. Six cells: Running, Waiting, TTFT, Prefill, Decode,
KV Cache. Each: 10px label, 24px mono value, unit, a data-class tag, and a
10px subline with the 5-minute delta (the delta is colored by what a rise
means for that signal, not by direction). A second row adds GPU utilization,
VRAM used/total, and temperature when device telemetry is available. Cells
that map to a profile layer are buttons with a 2px inset sand bottom edge
when selected.

### Fleet Profile (signature)

A canvas elevation sheet. Time on x; one lane per backend plus a fleet lane;
the selected layer is one of KV, router TTFT, engine TTFT, compute prefill,
decode, GPU utilization, or VRAM. Backend lanes use the metric's measured
source; device layers use device lanes only and are never attributed to a
backend.

- **Ridges:** 1.5px sand or role-colored line over a 16% fill; no smoothing
  beyond the measured points; nulls break the line.
- **Scale:** fixed 0–100% with 25/50/75/100 labels for KV/util/VRAM; auto
  max with four labelled rules for TTFT and throughput.
- **Requests:** in the 5m range, completed requests ride at each lane's base
  as amber prefill → moss decode segments (2.5px); router rejections are 2px
  vermilion ticks; live admitted requests are a dashed queue lead with an
  amber bar to now, streaming requests moss.
- **Live edge:** a 1px vertical rule, a 4px sand square, and a spot height in
  10px mono at every lane. The header readout mirrors `T HH:MM:SS / KV 78%`.
- **Cursor:** click or pointer sets a survey cursor; the keyboard uses a
  labelled range slider; arrow keys walk it; `Return to live`, `L`, or `Esc`
  clears it. The cursor is a 1px chalk rule with a 3px ring at the selected
  lane.
- **Accessible series:** the canvas is `aria-hidden`; a clipped table
  summarises every backend lane over the visible window (last, average,
  minimum, maximum, and the cursor value when pinned). A separate clipped
  table carries device history. Both are wrapped so their table layout cannot
  create page overflow.
- **No data:** a 45° hatch at 16% with a 9px `NO DATA` label; never
  interpolated.
- **Live tail:** the fetched history is merged with one frame per second;
  the tail starts after the last real history point, not after the grid end.
- **Header:** layer label, source tag, source note, the readout, and a
  return-to-live button when pinned.
- **Footer:** last/avg/min/max of the visible buckets plus router TTFT p50/p95
  with its actual sample count; legend swatches for the layer, prefill,
  decode, failure, and no data.
- **Ruler:** a 1px track with an 8×16px sand thumb, `HISTORY` and `LIVE` end
  labels, and an `aria-valuetext` readout.

### Live Sessions

A 400px panel (full width when stacked) headed `LIVE SESSIONS`, a count chip,
and `oldest first`. Each row leads with what can slow a pool, not metadata:
backend plus a 10px phase dot (`queued` amber, `stream` moss), then a metric
line `CTX`, `KV HIT`, `NEW`, a single gold `est` tag, a rule, and measured
`TTFT`. The estimates are 10px and prefixed `~`; `TTFT` is the row's one
12px measured value. `KV HIT` is the router's estimated cache reuse `(ctx - new) / ctx`
from body-byte token estimates, amber below 40%; the engine's measured
cache-hit rate remains in Backend Detail. A 3px amber track shows queued
prefill progress or a complete prefill; its note gives the estimated prefill
time (gold boundary) and flags a queue over 1.5× that estimate. The ticking
age is a 10px footnote at the right; path and session prefix are only in the
row title. The list has a fixed height, never a content max: 560px while the request
log is collapsed (420px under 1200, 360px on phones) and 320px when it is
open (280px under 1200). Request arrival/completion never resizes the sheet;
only the log toggle trades the two fixed heights. Token figures wear the row `est` tag; TTFT is measured. Rows
preserve identity and move in place; nothing flashes. Empty: `no requests in
flight`; paused: `paused — no new requests are being read`; stale: `feed
stale — no new requests are arriving`.

### Backend Detail Table

A full-width ruled table: backend + `KING`/`SUBJECT` role, router slots, engine run/wait, KV%,
KV pool, engine memory (KV + weights in GiB), router TTFT, engine prefill,
engine decode, cache hit. Column heads carry a source dot (moss router, water
engine). Engine cells are `—` when the engine is not `ok`; the row title
carries the last error. Sortable headers; the first column is sticky; numeric
cells are right-aligned. At a survey cursor the header reads `at HH:MM:SS`
and values come from the history bucket; stale reads `last data HH:MM:SS`.
Below 640px the pool, memory, decode, and hit columns are dropped.

### Request Feed

A collapsed ruled header by default: `REQUESTS`, the last-5s spike and 60s
peak, a water chip with the recent 429+ count, and `SHOW`. The filter, CSV,
table, and footer appear only when opened; the open state persists in
`localStorage`. The table is the recent ring: completion time, backend, path,
status, TTFT, duration, new-token estimate. Status is moss for 200, water for
429, amber for other 4xx, vermilion for 5xx. Path is dropped below 640px. The
log stays available without occupying the band on every visit; its `open`
state is bound to the page so the live stack claims the released height when
it is collapsed.

### Buttons, Segmented Control, Chips

Square, 1px rule border, transparent ground, 10px label type, 5×9px padding.
Hover raises the border to sand and the text to chalk; pressed uses the active
panel with a 2px inset sand bottom edge. The segmented range control is a
single 1px frame with internal 1px dividers. Chips are 18px tall with a 4px
square marker and 1px current-color border; no fill. Icons are 12px drawn SVG
stroke 1.5.

### Stale Banner & Notices

A 34px band with a 1px vermilion bottom border, an 8px vermilion square,
`STALE — last data HH:MM:SS · Ns ago`, and `re-arming <feed>` at the right.
It never fills the row with red. A one-line notice below reports a missing
history feed without hiding the last series.

### Tables and Browser Surfaces

Tables are the accessible numeric twin of the profile. Thin scrollbars are
themed from the rule color; selection is sand at 30%; focus is a 2px chalk
outline with 2px offset; the caret and native checkbox follow the ink. Text
selection uses sand at 30%.

## Do's and Don'ts

### Do:

- **Do** lead live-session rows with TTFT as the one large measured value;
  keep estimated context/reuse/load smaller, muted, and `~`-prefixed. Time,
  endpoint, id, and phase are secondary.
- **Do** keep the request log collapsed by default; the header still carries
  its spike and 429+ counts.
- **Do** put live sessions beside the profile in the first viewport; the
  operator's first question is what is running now.
- **Do** label every figure `router`, `engine`, `device`, or `est`.
- **Do** keep the Fleet Profile as the only history chart; the readings strip
  is not a second chart.
- **Do** hatch missing data and break lines at nulls; never interpolate.
- **Do** freeze the last truth on feed loss and label it `stale`; the STALE
  banner is the only feed-level failure.
- **Do** use tabular mono for every measured figure and integer 2px type steps.
- **Do** keep 429 water (a capacity rejection) and vermilion for 5xx/saturation.
- **Do** keep device lanes separate; never attribute VRAM to a backend.
- **Do** move TTFT/prefill/decode/KV with a 1.6s ease-in-out tween and counts
  with 480ms; the exact value remains in screen-reader text and a metric never
  overshoots.
- **Do** glide the live edge continuously across each 1s interval; stop the
  loop when the profile is offscreen and under reduced motion.
- **Do** give TTFT, prefill, decode, and KV a 15s mean or 5s hold and a 1.6s
  tween; motion is for transitions, not for chasing every scrape. Print the
  window on the cell.
- **Do** let rows keep identity through reflow with FLIP; skip entrance
  flashes on load and on bursts.
- **Do** honor `prefers-reduced-motion`: snap the edge and numbers, run JS
  row transitions at zero duration, and keep color/state feedback legible.

### Don't:

- **Don't** use sand as decoration or a generic accent; it is the survey level
  line.
- **Don't** add shadows, glows, gradients, glass, or rounded corners.
- **Don't** use serif or italic type, or a display face.
- **Don't** re-introduce equal KPI cards, sparkline tiles, or a second chart
  style beside the profile.
- **Don't** present the router's GPU budget as device utilization; it is an
  admission weight.
- **Don't** show a stale engine's retained gauges as current; use `—` and
  hatch the lane.
- **Don't** forward-fill history or claim a percentile the sample ring does
  not cover; state `n` and coverage.
- **Don't** let the compact/mobile layout become a separate design; it is the
  same sheet with a narrower gutter and fewer table columns.
- **Don't** use bounce or elastic easing on measured numbers; motion may
  smooth a transition, never imply a value that was not sampled.
- **Don't** add page-load choreography, mount fades, scroll reveals, or a
  60fps canvas loop for a 1.6px/s edge.
- **Don't** fade or fly in live rows and request rows; a panel that flashes on
  load reads as jank, not responsiveness.
