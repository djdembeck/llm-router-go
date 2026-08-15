# Live Metrics Contract (frozen — A emits, B consumes)

Owner of final truth: slice A (Go). Both slices build against THIS frozen shape;
A must emit exactly these field names/types (adding fields is allowed, changing
or removing is not). B builds its client/types/stores to match, using a local
mock SSE dev server for independent development.

All endpoints live on the router's existing listener (same listener, same
absence-of-auth as /stats — the router has no auth and is always deployed
behind a TLS/auth reverse proxy). None of these may shadow the existing
`/health`, `/stats`, `/v1/models`, or any proxied `/v1/*` path.

Two new management endpoints. The client uses #1 (SSE) as its primary feed;
#2 backs the live request feed; the existing `/stats` remains as a
non-EventSource fallback.

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
      "reqRate": 2.5,
      "bytesRate": 88000,
      "tokEstRate": 4100,
      "reqTotal": 1043,
      "bytesInTotal": 2100000,
      "bytesOutTotal": 158000000,
      "tokEstTotal": 8812345
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
- `ttftMs` / `ttftSampleCount` — per-backend EWMA (milliseconds) of **time-to-first-byte**, measured ONLY for streaming requests (first response byte minus request start). `ttftSampleCount` is the number of streaming samples behind the EWMA; `0` means "no streaming samples yet" → client renders "—" not "0". NEW instrumentation (first-byte timestamp captured in the response tracker).
- `reqRate` / `bytesRate` / `tokEstRate` — per-backend rates over the streaming interval: requests/second, proxied bytes/second (in+out), and **estimated new prefill tokens/second** (see honest-boundary note). Computed server-side from counter deltas over the interval.
- `reqTotal` / `bytesInTotal` / `bytesOutTotal` / `tokEstTotal` — monotonically increasing counters since process start (bytesIn = request body bytes, bytesOut = response body bytes actually written, tokEst = sum of `estimateNewTokens` over accepted requests).
- `gpu.used` / `gpu.budget` / `gpu.waiting` — live GPU-budget weighted state. `gpu.active` is `true` when `tier0Inflight > 0`. When no GPU budget is configured, `gpu.budget` is `0` and `active` is `false`.
- `tier0Inflight` — total in-flight across all tier-0 backends.
- `totals.*` — sum across backends (inFlight, waiting) plus aggregate rates/counters.

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
