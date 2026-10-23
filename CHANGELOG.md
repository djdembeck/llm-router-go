# Changelog

## Unreleased

### Bug Fixes

* **metrics:** retire live requests, release prefill slots, and record request outcomes even when a proxy stream aborts or the client disconnects; preserve HTTP abort behavior instead of leaving phantom sessions
* **dashboard:** read vLLM's group-aware KV token capacity from `cache_config_info`; show unknown usage as `—` independently of known capacity in live and historical pool readings

### Features

* **dashboard:** replace the fold-sheet UI with the Capacity Survey — readings strip, Fleet Profile (KV ridges, live request bars, survey cursor), live session stack, backend detail, request feed, night/day sheets
* **dashboard:** smooth the sheet — TTFT/prefill/decode read a 15s mean refreshed every 5s and tween over 1.6s, the KV gauge holds 5s, a continuously gliding live edge, FLIP session rows with no entrance flashes, a stable request table, eased controls, and a night/day cross-fade; reduced-motion paths snap values and zero JS row durations
* **dashboard:** live rows lead with measured TTFT; estimated context, KV reuse, and new-token load are smaller, muted, and `~`-prefixed, with a prefill/queue note; path, session id, and age recede. The request log is collapsed by default, persisted, and keeps its spike/429 counts in the header; the live stack grows into the released height while it is collapsed
* **dashboard:** audit and polish — mobile hit targets meet 44px, selected backend rows announce `aria-current`, the range has a 44px hit area, and the Fleet Profile fills its sheet instead of leaving dead paper
* **metrics:** correct 30s arrival rates and session liveness, fix engine scrape baselines and the SGLang compute/cache prefill split, add server-side `/metrics/history` (5m–30d) and optional device telemetry via `GPU_METRICS_URL`

## [1.0.1](https://github.com/djdembeck/llm-router-go/compare/v1.0.0...v1.0.1) (2026-07-25)


### Bug Fixes

* **ci:** update stale llm-release-action SHA pin ([2ac4cbc](https://github.com/djdembeck/llm-router-go/commit/2ac4cbcc41762097089efa3bdd64256dc45b8809))
* **ci:** update stale llm-release-action SHA pin ([c0b22dc](https://github.com/djdembeck/llm-router-go/commit/c0b22dcd396552ac1e745a71c108f8462455d3ac))

## 1.0.0 (2026-07-25)


### Features

* initial main branch with LICENSE ([524fced](https://github.com/djdembeck/llm-router-go/commit/524fceded1850326bbbf4043f98103e1439f710c))
* initial release of GPU-aware LLM inference reverse proxy ([#4](https://github.com/djdembeck/llm-router-go/issues/4)) ([cffafdf](https://github.com/djdembeck/llm-router-go/commit/cffafdfc34f6436091cf09c8a60e69d034bfe3f2))


### Bug Fixes

* **ci:** revert invalid releases permission scope ([9c798cc](https://github.com/djdembeck/llm-router-go/commit/9c798cc33463d810c8f660228e884ac85196c343))
* resolve Mira PR [#5](https://github.com/djdembeck/llm-router-go/issues/5) review findings ([9cca478](https://github.com/djdembeck/llm-router-go/commit/9cca4788b2353e3ce6c866b68703dcb97d796802))
