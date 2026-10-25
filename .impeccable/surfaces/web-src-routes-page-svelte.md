---
version: 1
slug: "web-src-routes-page-svelte"
primary_target: "web/src/routes/+page.svelte"
related_targets: []
---

# Surface: llm-router-go dashboard (/)

Mode: Operate. Single-page live fleet dashboard for an ML-infrastructure operator running vLLM/SGLang backends behind the router. Audience: the operator on call, checking the fleet while traffic is live. Job: see which sessions are running, where GPU/KV pressure is building, how fast prefill/decode/TTFT are moving, and what the last hour/day/month looked like; act on a saturated backend or a stalled feed. Action/task: read state in one glance, scrub any instant for exact values, spot rejections and engine gaps. Proof/content: the router's real /metrics/stream + /metrics/sessions + /metrics/burst, engine /metrics scrapes, optional device exporter, server history rings. Constraints: no auth, embedded static SPA, single binary, honest estimator/measurement boundary (est/router/engine/device), no fabricated device telemetry.

## Direction contract

THESIS: Capacity is terrain. The surface is a survey sheet of the fleet: level lines, spot heights, grid references, marginal scale. It refuses the equal-card grid and the dark-neon ops board.

OWN-WORLD: Night survey sheet — ground #10130e/#151910, sand #b9a684, chalk #e9e5d8, water #7fa3c4, moss #8fae74, amber #d6a445, vermilion #cf5b43; Overpass + Overpass Mono; 1 px rules at 8–14% alpha, 8 px baseline, sharp corners, no shadows; day sheet inverts to cool paper with the same roles.

STORY: At a glance the operator sees where pressure is building, which lane is saturated, which requests are still prefilling, and how the last month looked; then scrubs any instant and reads exact values without losing the live edge.

FIRST VIEWPORT: Top bar (mark, health, clock, range, pause, theme); 232 px fleet rail (readings + backend rows with KV level bars); ruled readings strip (Running, Waiting, TTFT, Prefill, Decode, KV — each selects the profile layer); two-column live band — Fleet Profile (left, ~340 px) and Live Sessions stack (right, 400 px, 6–8 rows). Live sessions visible without scrolling. Below: backend detail and request feed. With exactly one configured backend, replace the rail with a compact server identity/status row, use the full width for Server Profile and sessions, omit the duplicate aggregate lane, and replace the comparison table with compact Server Detail. Keep all device lanes, history ranges, and source boundaries; the request log omits repeated backend names but CSV retains them. Mode follows configured count, not healthy-engine count; pause and stale feeds preserve it.

FORM: Standard web ops shell; the world enters only through type, palette, density, and the Fleet Profile. seed:fa1d89619c16ec0d

SIGNATURE: The Fleet Profile — time on x, one lane per backend on y; the KV level is a sand ridge over a water fill, 25/50/75/100% reference rules, live request bars (amber prefill → moss decode, dashed queue lead), vermilion failure ticks, spot heights at the live edge and a survey cursor with a `T‑04:21 · KV 78%` grid reference. The cursor scrubs every reading; arrow keys walk it; `L`/Esc returns to live; nulls are hatched, never interpolated. Device lanes are separate and never attributed to backends.

RAISES: whole-cell type lock (integer 2 px type steps, tabular figures in fixed cells, no fractional line-heights) from Emigre; mark-plus-hue state (filled/hollow/hatched/2 px rule in addition to colour) from the Celestial atlas; margin-column heads; routine state changes ease out over 160–240 ms instead of stepped frames (the brief asks for flow, not bounce).

## Scope notes

The information architecture is fixed across the decision round. The live sessions stack and the profile share the first viewport. History ranges: 5m, 1h, 6h, 24h, 7d, 30d from the server rings. A non-ok engine scrape renders `—` live and hatched in the profile; last-known values only in backend detail with their scrape time. Device telemetry (DCGM/nvidia exporter) is optional and appears only when configured.
