# Changelog

## Unreleased

### Bug Fixes

* **metrics:** gate the management listener with a Host allowlist (DNS-rebinding guard) — the whole management mux (all `/metrics/*` routes, `/stats`, and the web SPA `/`) rejects requests whose `Host` header is not allowlisted with `403`; loopback hosts are allowed by default, `METRICS_ALLOWED_HOSTS` (comma-separated) extends the list and fails startup on invalid values, like the `SESSION_*` knobs; correct the session-admission docs (the `admissionLease`/`admissionIdleSinceMs` fields live on `GET /metrics/sessions`, not `/metrics/stream`; keyed rows use the `sa:`-framed admission key as their session id) and the `.env.example` `SESSION_*` sample comment markers
* **runtime:** default native routing to loopback with explicit host opt-in, keep container routing reachable with loopback-published examples, run the image as UID/GID 10001 with writable history storage, bound incoming headers/body and idle connections without response deadlines, and drain both HTTP listeners on SIGTERM/SIGINT before a bounded forced close.
* **dashboard:** keep the mark and the browser chrome honest — the night wordmark becomes a mint-slate ink instead of near-white (it sat at 1.1:1 against chalk, so the lettering dissolved into body text), the tab icon uses a 16px optical drawing instead of the shrunken full symbol, the tab icon and `theme-color` follow the chosen sheet rather than the host OS, and the top bar stops demoting `LIVE` behind the range control at laptop widths; the health chip announces its own changes, and the unreferenced symbol/mono SVGs leave the embedded bundle (the symbol vector master moves to `assets/logo/`)
* **dashboard:** keep the phone header on the detail token — the feed line reads 12px at every width instead of dropping to 10px below 640px, at no height cost
* **dashboard:** set label leading to the documented 10/12 token — section heads and column heads rendered 10/16, so the readings strip loses 4px of borrowed air and the label/value step reads sharper
* **release:** never ship a release image from a foreign commit — the release build step verifies the version tag points at the run's commit (ancestor tag skips, foreign tag fails) instead of trusting image existence, and CI no longer cuts `type=semver` release tags on plain main pushes
* **release:** finalize drafts from the authenticated release listing — `GET releases/tags/{tag}` is a published-release lookup, so the draft create/reuse/publish cycle now matches the paginated authenticated listing by exact `tag_name` (duplicates fail closed, listing auth/server failures propagate without creating) and publishes with `--latest` computed from published non-prerelease semver records so a delayed lower release never rolls back GitHub Latest
* **release:** keep registry command output in CI errors — failing `docker buildx` invocations now surface their captured stdout and stderr in the CLI error while preserving the manifest-not-found vs auth/transport distinction
* **ci:** pin the CI build job to the canonical `Build` check-run name — main's required status checks match that exact name, so `Build (web + Go)` would strand every `develop → main` PR waiting for a context that never reports
* **release:** ops-only commits must never cut a release — `paths-ignore` gains `renovate.json5`, and Renovate's commit prefix is restored to `ci(deps): `
* **metrics:** retire live requests, release prefill slots, and record request outcomes even when a proxy stream aborts or the client disconnects; preserve HTTP abort behavior instead of leaving phantom sessions
* **dashboard:** read vLLM's group-aware KV token capacity from `cache_config_info`; show unknown usage as `—` independently of known capacity in live and historical pool readings
* **dashboard:** keep the first-viewport live band honest at >=1280px — the fold cap measures the band's distance from the document top instead of per-mode constants, so the STALE banner, history notice, or a wrapped server-identity row shrink the band in place instead of pushing the session list past the fold; the profile plot's 320px floor yields to a 96px minimum inside the capped range so the sheet never overflows its band
* **dashboard:** surface saturation in the first viewport — the fleet rail prints each backend's 5-minute 429 count (water, beside the KV bar) and the Fleet Profile feet each lane's live-edge spot height with a `429 n` figure
* **dashboard:** stop 200% text-zoom horizontal overflow — readings sublines, the profile summary line, and the top-bar feed line wrap instead of forcing the sheet sideways; the top bar clips and its controls row wraps
* **dashboard:** track `prefers-reduced-motion` changes mid-session so the canvas live-edge snap keeps mirroring the CSS 0.01ms kill; demote the profile readout's mirrored KV to data-detail type when a cursor is pinned, so the companion invariant no longer reads as a duplicated figure

### Features

* **admission:** TTL-bounded session admission — with `SESSION_ADMISSION=on`, each keyed conversation (backend-scoped lease id from `prompt_cache_key`, with optional `user`/`header:` fallback) holds a bounded admission slot per backend: the cap is `SESSION_MAX_ADMITTED` (or the backend's `maxConcurrent` when unset), queued admissions are strictly FIFO while bounded by `SESSION_MAX_WAITING` (or the backend's `maxQueueDepth`, default 2) and the existing `MAX_QUEUE_TIMEOUT` grace, and released leases retain their slot for `SESSION_IDLE_TTL` (default 5m) before newcomers may take it; the expiry timer — not traffic or dashboard polling — drains the queue, queue-full and timeouts answer `429` with `X-Router-Reason: session-admission-queue-full|session-admission-timeout`, cancellations mint nothing (499, recorded only), and unkeyed traffic plus the disabled default keep the legacy per-request behavior exactly; the bounded live feed now reports `admissionLease`/`admissionIdleSinceMs` per session from the lease map itself, so session rows never claim a lease the registry has evicted
* **dashboard:** add approved B3 Flow branding — theme-selected day/night header logo, matching favicons, and README identity, with identifier-only colors that preserve the survey palette and semantic states
* **dashboard:** automatically focus a one-backend deployment on its server: remove the fleet rail and duplicate aggregate chart lane, reclaim chart width, show compact server details, and omit repeated backend names; preserve fleet comparisons for multi-backend deployments and device-wide GPU telemetry in both modes
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
