# Live Metrics Contract (frozen — A emits, B consumes)

Owner of final truth: slice A (Go). Both slices build against THIS frozen shape;
A must emit exactly these field names/types (adding fields is allowed, changing
or removing is not). B builds its client/types/stores to match, using a local
mock SSE dev server for independent development.

All endpoints live on the router's existing listener (same listener, same
absence-of-auth as /stats — the router has no auth and is always deployed
behind a TLS/auth reverse proxy). None of these may shadow the existing
`/health`, `/stats`, `/v1/models`, or any proxied `/v1/*` path.

Three new management endpoints. The client uses #1 (SSE) as its primary feed;
#2 backs the live request feed; #3 backs the live sessions view (in-flight
request stack + conversation-grouped sessions); the existing `/stats` remains
as a non-EventSource fallback.

## 1. GET /metrics/stream  (SSE)

Server pushes one JSON object per `data:` frame, every `?interval=500ms`
(clamp to 200ms..10s, default 500ms). The connection stays open; the client
reconnects on drop. Each frame is a full live snapshot (client is stateless
per-frame). `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
`Connection: keep-alive`, `X-Accel-Buffering: no`.

Frame schema (all numbers are `int`/`float64` as Go serializes them):

```json
{
  "t": 1721000000123,
  "backends": [
    {
      "name": "primary",
      "url": "http://vllm-1:8000",
      "tier": 0,
      "inFlight": 3,
      "maxConcurrent": 4,
      "waiting": 0,
      "maxQueueDepth": 2,
      "prefillInFlight": 1,
      "prefillWaiting": 0,
      "prefillMax": 1,
      "avgDurationS": 12.4,
      "ewmaMs": 12400,
      "ttftMs": 320,
      "ttftSampleCount": 120,
      "ttftMsNow": 315,
      "reqRate": 2.5,
      "bytesRate": 88000,
      "tokEstRate": 4100,
      "reqTotal": 1043,
      "bytesInTotal": 2100000,
      "bytesOutTotal": 158000000,
      "tokEstTotal": 8812345,
      "engine": {
        "status": "ok",
        "engine": "vllm",
        "running": 3,
        "waiting": 1,
        "kvPct": 42.5,
        "hitRate": 0.81,
        "prefillTokS": 3800,
        "prefillCacheTokS": 450,
        "decodeTokS": 320,
        "ttftMs": 290,
        "itlMs": 41,
        "e2eMs": 11800,
        "queueMs": 12,
        "prefillMs": 210,
        "decodeMs": 9400,
        "perTokMs": 40,
        "meanPromptTok": 1850,
        "meanGenTok": 240,
        "waitCap": 1,
        "waitDefer": null,
        "retracted": null,
        "retractedTokS": null,
        "fullPct": null,
        "swaPct": null,
        "mambaPct": null,
        "kvUsedTok": null,
        "kvCapTok": null,
        "kvFreeTok": null,
        "kvEvictTok": null,
        "mambaUsedTok": null,
        "mambaCapTok": null,
        "hicacheHostUsedTok": null,
        "hicacheHostCapTok": null,
        "ttftStreamMs": null,
        "ttftNonStreamMs": null,
        "kvMemGB": null,
        "weightMemGB": null,
        "sloCap": null,
        "ctxLen": null,
        "preemptedTotal": 2,
        "abortedTotal": null,
        "streamDoneTotal": null,
        "nonStreamDoneTotal": null,
        "promptTokTotal": 412000,
        "genTokTotal": 96000,
        "hitQueriesTotal": 5100,
        "hitHitsTotal": 4130,
        "mmQueriesTotal": 88,
        "mmHitsTotal": 71,
        "reqDoneTotal": 1043,
        "finReasons": {"stop": 990, "length": 41, "abort": 12}
      }
    }
  ],
  "gpu": {
    "used": 3,
    "budget": 4,
    "waiting": 0,
    "active": true
  },
  "tier0Inflight": 3,
  "totals": {
    "inFlight": 5,
    "waiting": 1,
    "reqRate": 4.2,
    "bytesRate": 210000,
    "tokEstRate": 9000,
    "reqTotal": 1043,
    "bytesInTotal": 2100000,
    "bytesOutTotal": 158000000,
    "tokEstTotal": 8812345
  }
}
```

Field semantics (A implements, B displays):
- `t` — server clock, epoch milliseconds.
- `inFlight` / `waiting` / `maxConcurrent` / `maxQueueDepth` — live slot-manager state (in-flight, queued, limits). `maxConcurrent`/`maxQueueDepth` are `0` when unlimited/absent.
- `prefillInFlight` / `prefillWaiting` / `prefillMax` — live large-prefill slot state; all `0` when the backend has no large-prefill limit.
- `avgDurationS` / `ewmaMs` — per-backend EWMA of TOTAL request duration (seconds / milliseconds). Same value, two units. Existing behavior.
- `ttftMs` / `ttftSampleCount` — per-backend EWMA (milliseconds) of **time-to-first-byte**, measured ONLY for streaming requests (first response byte minus request start). `ttftSampleCount` is the number of streaming samples behind the EWMA; `0` means "no streaming samples yet" → client renders "—" not "0". The EWMA updates at first response byte (while the request is still live), not at completion. `ttftMsNow` is the most recent single first-byte sample (ms) — the live readout value, no smoothing; `0` until the first streaming first-byte is seen.
- `reqRate` / `bytesRate` / `tokEstRate` — per-backend **live arrival rates**, requests/second, admitted request-bytes/second, and **estimated new prefill tokens/second** (see honest-boundary note). Computed server-side from a 30s decaying pending window: every admitted request adds its demand (1 req, its body bytes, its est tokens) to the window the instant it is accepted, the window is divided by the tick interval for the rate, then decays by (interval / 30s) each tick. The rate therefore tracks ARRIVAL, not completion — a long decode contributes its true admission rate from the moment it is accepted, and the tail lingers ~30s after traffic stops instead of spiking to 2×-per-tick and collapsing to zero. Router-originated rejections (429/413) never enter the window — they are not backend demand.
- `reqTotal` / `bytesInTotal` / `bytesOutTotal` / `tokEstTotal` — monotonically increasing counters since process start (bytesIn = request body bytes, bytesOut = response body bytes actually written, tokEst = sum of `estimateNewTokens` over accepted requests).
- `gpu.used` / `gpu.budget` / `gpu.waiting` — live GPU-budget weighted state. `gpu.active` is `true` when `tier0Inflight > 0`. When no GPU budget is configured, `gpu.budget` is `0` and `active` is `false`.
- `tier0Inflight` — total in-flight across all tier-0 backends.
- `totals.*` — sum across backends (inFlight, waiting) plus aggregate rates/counters.
- `engine` (v2) — the backend's OWN execution truth, scraped from its Prometheus `/metrics` endpoint at 1s (vLLM: always on; SGLang: only when started with `--enable-metrics`). Full field table below.

### Engine v2 field table

Aggregation: vLLM request gauges/counter sums are SUMmed across data-parallel engines; vLLM `kv_cache_usage_perc` is MAX (per-pool). SGLang scheduler gauges are replicated per tp/pp rank → MAX; SGLang counters & histograms are unioned across ranks → SUM (per-rank counter deltas sum; histograms sum per label set then mean).

| json | type | class | vllm | sglang | source metric | aggregation |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| `status` | string | state | both | both | — | `"ok"` / `"off"` (404) / `"err"` |
| `engine` | string | state | both | both | family detection | `"vllm"` / `"sglang"` / `""` |
| `running` | number | gauge | both | both | `num_requests_running` / `num_running_reqs` | vllm SUM, sglang MAX |
| `waiting` | number | gauge | both | both | `num_requests_waiting` / `num_queue_reqs` | vllm SUM, sglang MAX |
| `kvPct` | number | gauge | both | both | `kv_cache_usage_perc` / `token_usage` | MAX × 100, 0..100 |
| `hitRate` | number | gauge | both | both | `prefix_cache_hits_total/queries` (lifetime) / `cache_hit_rate` | vllm = hits/queries (0 if q=0); sglang gauge (value>1 ⇒ ÷100) |
| `prefillTokS` | number | rate | both | both | vllm `prompt_tokens_by_source_total{local_compute}` (fallback `prompt_tokens_total`) / sglang `realtime_tokens_total{prefill_compute}` | SUM Δ/Δt, compute-only |
| `prefillCacheTokS` | number | rate | both | both | vllm `{local_cache_hit}` / sglang `{prefill_cache}` | SUM Δ/Δt; 0 when no by-source |
| `decodeTokS` | number | rate | both | both | `generation_tokens_total` / `{decode}` | SUM Δ/Δt |
| `ttftMs` | number | mean | both | both | `time_to_first_token_seconds` histogram | Δsum/Δcount × 1000 (blended) |
| `itlMs` | number | mean | both | both | `inter_token_latency_seconds` | hist mean × 1000 |
| `e2eMs` | number | mean | both | both | `e2e_request_latency_seconds` | hist mean × 1000 |
| `queueMs` | number | mean | both | both | `request_queue_time_seconds` / `queue_time_seconds` | hist mean × 1000 |
| `prefillMs` | number? | mean | vllm | — | `request_prefill_time_seconds` | hist mean × 1000, nil for sglang |
| `decodeMs` | number? | mean | vllm | — | `request_decode_time_seconds` | hist mean × 1000, nil for sglang |
| `perTokMs` | number? | mean | vllm | — | `request_time_per_output_token_seconds` | hist mean × 1000, nil for sglang |
| `meanPromptTok` | number | mean | both | both | `request_prompt_tokens` / `prompt_tokens_histogram` | hist mean in TOKENS (no ×1000) |
| `meanGenTok` | number | mean | both | both | `request_generation_tokens` / `generation_tokens_histogram` | hist mean in TOKENS |
| `waitCap` / `waitDefer` | number? | gauge | vllm | — | `num_requests_waiting_by_reason{reason=capacity|deferred}` | SUM, nil when absent |
| `retracted` | number? | gauge | — | sglang | `num_retracted_reqs` | MAX, nil when absent |
| `retractedTokS` | number? | rate | — | sglang | `num_retracted_input_tokens_total` | SUM Δ/Δt, nil until baselined |
| `fullPct` / `swaPct` / `mambaPct` | number? | gauge | — | sglang | `full_token_usage` / `swa_token_usage` / `mamba_usage` | MAX × 100; Mamba nil unless hybrid model |
| `kvUsedTok` / `kvCapTok` / `kvFreeTok` / `kvEvictTok` | number? | gauge | — | sglang | `num_used_tokens` / `max_total_num_tokens` / `kv_available_tokens` / `kv_evictable_tokens` | MAX each |
| `mambaUsedTok` / `mambaCapTok` | number? | gauge | — | sglang | `mamba_used_tokens` / (used+available+evictable) | MAX each then sum; nil unless hybrid |
| `hicacheHostUsedTok` / `hicacheHostCapTok` | number? | gauge | — | sglang | `hicache_host_used_tokens` / `hicache_host_total_tokens` | MAX each; nil when absent |
| `ttftStreamMs` / `ttftNonStreamMs` | number? | mean | — | sglang | `time_to_first_token_seconds` split by `is_streaming` label | per-label hist mean × 1000; nil for vllm |
| `kvMemGB` / `weightMemGB` | number? | gauge | — | sglang | `kv_cache_memory_usage_gb` / `weight_memory_usage_gb` | MAX |
| `sloCap` | number? | gauge | — | sglang | `max_running_requests_under_SLO` | MAX, nil when absent |
| `ctxLen` | number? | gauge | — | sglang | `context_len` | MAX, nil when absent |
| `preemptedTotal` | int? | lifetime | vllm | — | `num_preemptions_total` | latest value, nil for sglang |
| `abortedTotal` | int? | lifetime | — | sglang | `num_aborted_requests_total` | latest, nil for vllm |
| `streamDoneTotal` / `nonStreamDoneTotal` | int? | lifetime | — | sglang | `num_requests_total{is_streaming=true|false}` | latest per label |
| `promptTokTotal` / `genTokTotal` | int? | lifetime | both | both | `prompt_tokens_total` / `generation_tokens_total` | latest raw SUM (sglang sums both is_streaming) |
| `hitQueriesTotal` / `hitHitsTotal` | int? | lifetime | vllm | — | `prefix_cache_queries_total` / `hits_total` | latest |
| `mmQueriesTotal` / `mmHitsTotal` | int? | lifetime | vllm | — | `mm_cache_queries_total` / `hits_total` | latest |
| `reqDoneTotal` | int | lifetime | both | both | vllm Σ `request_success_total` / sglang Σ `num_requests_total` | latest |
| `finReasons` | map[string]int | lifetime | vllm | — | `request_success_total{finished_reason}` | latest per reason; nil for sglang |

Field classes: **gauge** = live now; **mean** = histogram mean over the last scrape interval; **rate** = counter Δ/Δt over the last interval; **lifetime** = engine-side counters since the engine's own start (assigned the latest raw value each ok scrape — an engine restart legitimately re-zeros them).

On a non-ok scrape (`"off"`/`"err"`): the **rate** and **mean** fields are zeroed (stale deltas would mislead) while **gauge** and **lifetime** fields keep their last known values. Zero + status ≠ `"ok"` = "no data" → render "—". The router's own req/bytes/tok rates and TTFT stay authoritative for the router's admission view; the engine block is the execution view.

## 2. GET /metrics/burst

A single JSON object: the most recent completed requests in a bounded ring
(server default capacity 500, configurable via `?limit=` up to 1000). Newest
last. Backs the live request-feed tape.

```json
{
  "requests": [
    {
      "t": 1721000000123,
      "backend": "primary",
      "path": "/v1/chat/completions",
      "status": 200,
      "stream": true,
      "durMs": 12400,
      "ttftMs": 320,
      "newTokensEst": 512,
      "bytesIn": 2048,
      "bytesOut": 158000
    }
  ]
}
```

- `status` is the upstream status as proxied (or the 429/413/503/502 the router itself returned — the router's own rejections are recorded too, so the feed shows capacity rejections live).
- `ttftMs` is `0` for non-streaming and for router-originated rejections.
- `durMs` for a router-originated 429 is the queue-wait time before rejection.

## 3. GET /metrics/sessions

A single JSON object: the router's in-flight request stack plus
conversation-grouped session aggregates. `Content-Type: application/json`.
No query params. The client polls it (it is not an SSE feed).

```json
{
  "t": 1721000000123,
  "live": [
    {
      "id": 1,
      "sess": "a1b2c3d4e5f6",
      "backend": "vllm-king-0",
      "path": "/v1/chat/completions",
      "stream": true,
      "phase": "admitted",
      "startMs": 1721000000000,
      "ctxTok": 123,
      "newTok": 45
    }
  ],
  "sessions": [
    {
      "id": "a1b2c3d4e5f6",
      "backend": "vllm-king-0",
      "n": 5,
      "firstMs": 1720999900000,
      "lastMs": 1720999990000,
      "active": true,
      "liveN": 1,
      "ctxTok": 123,
      "newTok": 45,
      "cachedTok": 78,
      "avgTTFTMs": 123.4,
      "totalDurS": 12.3,
      "tokOutTotal": 987,
      "lastStatus": 200,
      "reqs": [
        {
          "tMs": 1720999990000,
          "status": 200,
          "stream": true,
          "durMs": 1230.5,
          "ttftMs": 90.2,
          "ctxTok": 123,
          "newTok": 45,
          "cachedTok": 78,
          "tokOut": 45,
          "tokS": 3.4
        }
      ]
    }
  ]
}
```

Semantics:

**Session identity.** For chat requests (`messages` array non-empty), the
session id is the sha1 of the raw JSON of every message **except the last**,
joined by `"\n"` (a single-message conversation uses that message's content),
hex-encoded, truncated to **12 chars**. Same conversation prefix → same
session; a conversation **forks a new session at its second turn** (the
prefix grows). Non-chat paths (`/v1/completions` with a `prompt` string, or
unparseable bodies) get `sess: ""` — the request still appears in `live`
but is excluded from `sessions`.

**`live[]`** — in-flight requests only, registered at admission (phase
`"admitted"`), flipping to `"streaming"` on the first response body byte,
removed on finish. `id` is a process-wide unique counter. Sorted `startMs`
**ascending** (oldest on top — the stack). Feed cap **256** (oldest evicted
beyond that); internal live map cap **1024** (evict oldest on insert).
`ctxTok`/`newTok` are router estimates (see honest boundary).

**`sessions[]`** — per conversation:
- `n` — completed requests in the session (incl. rejections); `firstMs`/`lastMs` — first/last request ms.
- `active` — `liveN > 0` OR `(now − lastMs) < 120s`.
- `liveN` — in-flight count for this session.
- `ctxTok`/`newTok`/`cachedTok` — from the **latest** completed (or live) request; `cachedTok = max(0, ctxTok − newTok)`.
- `avgTTFTMs` — mean TTFT over completed **streaming** requests with `ttftMs > 0` (rejections/non-stream excluded).
- `totalDurS` — Σ durMs over all requests.
- `tokOutTotal` — Σ tokOut (est, bytesOut/4).
- `lastStatus` — latest request status (rejections count).
- `reqs` — last **32** completed requests, **oldest first**, included ONLY for sessions that are active (liveN>0) OR have `lastMs` within **60s**, AND within the **first 20** sessions after sorting; otherwise `null`.
- Sort: **active first, then `lastMs` descending**. Feed cap **100** (least-recent evicted); internal store cap **512** (LRU by `lastMs`).

**Rejections (429/413)** never enter `live` — they bypass it and are folded
directly into the session ring/aggregates (`stream: false`, `ttftMs: 0`,
`durMs` = queue-wait time before rejection). Mid-stream client cancellations
go through the normal finish path with the already-started response status.

**Per-request (`reqs[]`) fields:** `tMs` (start), `status`, `stream`,
`durMs` (admission→finish), `ttftMs` (0 non-stream/rejection), `ctxTok`/
`newTok`/`cachedTok` (est), `tokOut` (est = bytesOut/4), `tokS` (est =
tokOut ÷ seconds, 0 when durMs=0).

**Est boundary:** ALL token figures in this endpoint are router-side
**body-based ESTIMATES** (~4 bytes/token): `ctxTok` = Σ message content
bytes (or prompt bytes) ÷ 4; `newTok` = `estimateNewTokens` (last message
only for multi-turn); `tokOut` = response body bytes ÷ 4. They are NOT
measured — render with the "est" marker, as in the burst feed.

## Honest data boundary (binding on both slices)

The router counts **requests, bytes, queue/concurrency/GPU state, total
duration EWMA, and now first-byte (streaming) TTFT** — all real, measured.
It does NOT count output tokens or prefill compute: it streams bytes through
and does not parse upstream SSE usage, and vLLM/SGLang expose per-request token
usage inconsistently. Therefore:
- `tokEstRate` / `tokEstTotal` / `newTokensEst` are the router's existing
  **estimated** NEW prefill tokens (request-body based, ~4 bytes/token), NOT
  measured decode tokens. B must render these with an "est" marker and never
  present them as measured token throughput.
- `/metrics/sessions` token figures (`ctxTok`, `newTok`, `cachedTok`,
  `tokOut`, `tokS`, `tokOutTotal`) are the SAME body-based estimates
  (~4 bytes/token) — they too must carry the "est" marker. The engine block
  in `/metrics/stream` is the only place token figures are **measured**.
- Throughput is shown as **measured** requests/sec + bytes/sec (real).
- No panel may display a "decode tokens/sec" line as if measured. If a token
  rate is shown it is the *prefill estimate* and is labeled as such.
This is a measurement boundary, not a data-quality bug; the handoff states it.

## Dev mock (slice B)
B ships a SvelteKit dev-only route (`src/routes/dev-metrics/+server.ts`) that
emits the SAME SSE frame shape + burst shape from a small synthesizer, so the
dashboard runs with `bun run dev` and no router running. The Vite proxy in
`vite.config.ts` forwards `/stats`, `/metrics/*`, and `/v1/models` to the real
router (default `http://localhost:80`) when one is up; the store prefers
`/metrics/stream`, falls back to `/dev-metrics/stream` in dev, then to
`/stats` polling. Never ship the mock in the production build (it is a
`+server.ts` route that the static `adapter-static` build prerservers/omits;
the production store only ever hits `/metrics/stream` and `/stats`).
