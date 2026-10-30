---
target: logo integration and placement on web UI
total_score: 16
max_score: 20
na_heuristics: 5,7,9,10
p0_count: 0
p1_count: 2
target_identity: "file:/home/djdembeck/orca/workspaces/llm-router-go/logo/web/src/lib/TopBar.svelte"
target_fingerprint: "sha256:b2a03bd43048ea945d15d767aca94e5ee3e84ef9b1a7d2888efd5b3dd4cf2905"
target_path: /home/djdembeck/orca/workspaces/llm-router-go/logo/web/src/lib/TopBar.svelte
timestamp: 2026-10-30T15-33-57Z
slug: web-src-lib-topbar-svelte
closed: true
---
# Critique — logo integration & placement (top bar)

Method: dual-agent (A: reviewer-design · B: task-light-gpt)

## Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3/4 | At ≤1100px LIVE/feed status drops to a second row below the range control (`order:3`), so health is read after six view options. |
| 2 | Match System / Real World | 4/4 | "GPU-aware inference routing" names the product's actual job; no generic analytics language. |
| 3 | User Control and Freedom | 4/4 | The theme button names its target and persists to `lrg-theme`. |
| 4 | Consistency and Standards | 3/4 | Favicon and `theme-color` follow the OS, not the sheet the operator chose; `alt="LLM Router"` is a third name for `llm-router-go`. |
| 5 | Error Prevention | n/a | No input or failure-prone action in the mark; the asset is static. |
| 6 | Recognition Rather Than Recall | 3/4 | The descriptor that names the product's job is `display:none` below 640px; the phone header says "LLM ROUTER" alone. |
| 7 | Flexibility and Efficiency | n/a | The logo surface has no workflow. |
| 8 | Aesthetic and Minimalist Design | 3/4 | Night wordmark is 1.11:1 against chalk and dissolves into body ink; `.mark` reserves 45px it never fills; the 16px favicon is unreadable. |
| 9 | Error Recovery | n/a | Nothing recoverable in the mark. |
| 10 | Help and Documentation | n/a | Out of scope for this surface. |
| **Total** | | **16/20** | **Good (80%)** |

## Design Specificity Verdict

**Authored for this product in the artwork; category-standard in the integration — which is the right call for Operate.**

The mark is genuine: custom path artwork, not a font or an icon set; the arrow-through-a-server-blade motif and the custom `LLM ROUTER` lettering are unique; `GPU-aware inference routing` is the actual product job (tiered admission for shared GPUs), not a slogan. The identifier-only colour rule is a decision most products never make.

The placement is the standard left mark + subtitle + status + right-side controls. On an operate-in-situ surface that is correct: the operator's muscle memory for brand top-left, state top-middle, controls top-right is worth more than novelty. What keeps it from being generic is that the bar belongs to the survey sheet — 1px rule, panel ground, no shadow, 10px tracked labels — and the mark sits on the same ground as the readings.

**Deterministic scan:** CLI `detect --json web/src` → exit 0, 0 findings (the scanner could not resolve `/fonts.css`, so colour and custom-property rules were skipped; treat that zero as low signal). Browser overlay on the live page: **200 findings — 196 `undersized-ui-text`, 4 `tiny-text`**. Top-bar instances: `.subtitle`, `.chip.ok`, `.chip.warn`, the six `.seg .btn` range labels, PAUSE, DAY. Every one is the 10px `--fs-label` token DESIGN.md codifies, so the rule is firing on a deliberate system. It is still the honest signal: the mark's own descriptor is the smallest text in the bar.

**Visual overlays:** Assessment B injected `detect.js` into its own tab and read the console; nothing was left in the user's browser. The dev server at :5173 is still running.

## Overall Impression

The mark belongs to the sheet. Custom lettering, no gradient, flush on the ruled ground, clean theme swap, and it never pretends to be a status light. Two things keep it from finished craft: at night the wordmark evaporates into chalk, and the mark does not survive its two harshest contexts — a 16px favicon and a 1024px laptop header.

## What's Working

1. **Colour separation is real, not just documented.** Mint #5EEAD4 (hue ~174°) vs moss LIVE #8fae74 (~84°), amber (~41°), vermilion (~9°). Measured at four widths in both sheets: the symbol cannot be misread as a health state. The identifier-only rule survives contact with the palette.
2. **Asset discipline.** Exact intrinsic dimensions (`992×192`), CSS `height:30px` + `width:auto`, alt text, per-asset `<title>`, day/night pair swapped from state. Rendered box is exactly 155×30 in 8 of 8 captures — no layout shift, no overflow at 390px.
3. **Day sheet contrast.** Wordmark #18343A on #f4f2e9 = 11.77:1; petrol symbol = 4.88:1 (clears the 3:1 graphical minimum). The day mark reads at a glance.

## Priority Issues

**[P1] The night wordmark is chalk, so the brand block loses its second half.**
`brand-wordmark-night` #E5F3F1 against `chalk` #e9e5d8 = **1.11:1**. At 30px, "LLM ROUTER" is optically identical to the 10px chalk labels and 12px mono values on the same ground; only the mint symbol carries identity at night — the primary sheet, in the dim room this product is designed for. The identifier-only rule still holds on paper but produces no observable difference, which is how a brand rule quietly rots.
**Fix:** either (a) recommit the night wordmark to a value that separates from chalk while staying clear of moss/amber/vermilion (a pale teal-slate at ~6–8:1), or (b) accept symbol-led identity at night, say so in DESIGN.md, and reduce the wordmark's optical weight so it stops competing with the labels. Option (a) is a brand-owner decision; do not ship it as an unannounced recolour.

**[P1] The mark does not survive 16px.**
The symbol rendered at 16/32/64 on both grounds: at 32px the arrow reads, at 16px the notch collapses to ~1.5px and the two 2px gaps nearly close, so the favicon is a teal blob. The favicon is the only place the product appears outside the app boundary. It compounds with the asset's padding: the artwork spans 32–224 in a 256 box, so a 16px icon carries a 12px mark.
**Fix:** ship a dedicated small-size asset — tightened viewBox with no padding, notch widened, strokes ≥2px at 16px — or a hand-drawn 16px variant. Keep the current art for 32/64.

**[P2] On a 1024px laptop the header demotes health and grows 30px.**
`@media (max-width:1100px)` sets `.status{order:3;flex-basis:100%}` and `.controls{order:2}`. Measured at 1024×768: `.mark` on row 1 (200px), `.controls` right on row 1, `.status` at y=68 across the full width; top bar 95px vs 65px at 1440. The operator whose job is reading the fleet sees six history ranges and two buttons before LIVE/feed health, and the fold-capped live band loses 30px of profile canvas. Separately, `.mark` reserves `min-width:200px` while the logo renders 155px — 45px of dead paper inside the brand block at every width above 639px.
**Fix:** `.mark{min-width:0}` (already applied at ≤639), subtitle line-height 12px per the label token (saves 4px), keep `.status` on row 1 down to ~900px, and where it must wrap put `.status` before `.controls`.

**[P2] The browser chrome follows the OS, not the sheet.**
`app.html` picks the favicon and both `theme-color` metas with `prefers-color-scheme`, while the app picks its sheet from `lrg-theme`. Day sheet on a dark OS gives a mint favicon and olive chrome over a cream page; night sheet on a light OS gives the reverse. README uses the same coupling for its logo pair.
**Fix:** set `link[rel=icon]` href and both `theme-color` metas from the resolved `data-theme` in the bootstrap script and on toggle, before first paint.

**[P3] Dead brand assets and a third product name.**
`web/static/logo-mono.svg` and `logo-symbol-mono.svg` ship in the embedded SPA with zero references anywhere in the repo (README uses `logo.svg` / `logo-dark.svg`); PRODUCT.md establishes no monochrome context. And `alt="LLM Router"` is a name that appears nowhere else: repo, image, README and `<title>` all say `llm-router-go`.
**Fix:** delete the two mono files (restore when a real single-colour context exists) and set `alt="llm-router-go"`.

## Persona Red Flags

**Alex (power user, 13" laptop at 1024):** opens the dashboard and gets brand + six ranges + pause + theme on row 1, LIVE 30px lower. He does not need the range control flanking the logo; he resizes windows and notices the 45px of unused brand reservation.

**Jordan (first-timer):** on a phone the descriptor is hidden, so the header says "LLM ROUTER" above a survey sheet with no explanation of what it routes. The document title says "fleet survey", which is the metaphor, not the job.

**On-call GPU operator (PRODUCT.md):** switching to the night sheet at 02:00 — the page goes dark, the tab icon and browser chrome stay cream/petrol, and the wordmark loses contrast against the panel exactly when she has the least attention to spare.

## Minor Observations

- The wordmark sits 8.75px above the bottom of its 30px box (viewBox overhang Latin caps never use) while the symbol runs to the box edge. With the 2px flex gap, the subtitle optically attaches to the symbol's baseline, not the wordmark's: ~10.75px below the letters vs 2px below the glyph. Tighten the viewBox height to the lettering's bounds, or shift the wordmark down inside the asset.
- `.subtitle` inherits the 16px `--lh-tight` line box; DESIGN.md's label token is 10/12. Applying the token shrinks the brand block to 44px and the single-row header by 4px.
- `--ink-3` at 10px/0.1em measures 5.24:1 night and 5.80:1 day — passes AA, but it is the smallest type in the bar and the only line that explains the product.
- The 45px `.mark` slack reads as intentional breathing room at 1440 and is pure waste in the wrap budget at 1100; it does not cause the wrap (the media query forces that) but it removes any chance of fitting.
- The favicon pair is correct per OS preference and wrong per app choice; there is no third state for "app says night, OS says day".

## Questions to Consider

- Is the night lockup deliberately symbol-led, or should the lettering carry identity in both sheets? That answer decides the P1.
- Should the favicon be its own optical-size drawing rather than the logo symbol scaled down, as mature brands do?
- At 1024, does the operator need the range control beside the mark, or should health own row 1 and the controls fold under it?
