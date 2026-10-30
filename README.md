# llm-router-go

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="web/static/logo-dark.svg">
    <img src="web/static/logo.svg" alt="LLM Router logo" width="320">
  </picture>
</p>

GPU-aware reverse proxy router for vLLM and SGlang LLM inference backends.

llm-router-go sits in front of one or more OpenAI-compatible inference servers and routes requests using GPU utilization, concurrency, queue depth, and latency signals. It is a single Go binary with no external dependencies.

Use it when several inference backends share GPU hardware and naive load balancing either wastes capacity or lets a large prefill starve smaller requests. Tiered admission, weighted GPU budgets, bounded per-backend queues, and prefill limits keep primary instances responsive while secondary capacity is used deliberately.

## Table of Contents

- [Security](#security)
- [Background](#background)
- [Install/Running](#installrunning)
  - [Docker image](#docker-image)
  - [Binary install](#binary-install)
- [Usage](#usage)
  - [Day-1 quickstart](#day-1-quickstart)
  - [Use an environment file](#use-an-environment-file)
- [Configuration](#configuration)
  - [Backends](#backends)
  - [Scalar environment variables](#scalar-environment-variables)
- [API](#api)
  - [`/health`](#health)
  - [`/stats`](#stats)
  - [`/v1/models`](#v1models)
  - [Proxied requests](#proxied-requests)
- [Architecture](#architecture)
  - [Tier system](#tier-system)
  - [GPU budget and slot managers](#gpu-budget-and-slot-managers)
  - [Prefill protection and load balancing](#prefill-protection-and-load-balancing)
- [Building](#building)
  - [Build the binary](#build-the-binary)
  - [Build the Docker image](#build-the-docker-image)
- [Contributing](#contributing)
- [License](#license)

## Security

> [!WARNING]
> This proxy has **no built-in authentication or authorization**. It blindly forwards any request it receives to the configured backends.
>
> **Always deploy behind a reverse proxy** (for example, Caddy, Nginx, or Traefik) that handles:
>
> - Authentication (API keys, tokens, and similar credentials)
> - TLS termination
> - Request validation and rate limiting
>
> Do not expose `llm-router-go` directly to untrusted networks.

## Background

When multiple inference backends share GPU resources, request count alone is a poor scheduling signal. A single massive prompt prefill can occupy a GPU and starve smaller requests. Tier 0 (King) backends therefore take priority; Tier 1 (Subject) backends use only the remaining configured GPU budget while a King is active.

The router's scheduling controls are deliberately narrow:

- **GPU-weighted admission:** `gpuWeight` models the share of a shared GPU consumed by a backend instead of treating every request as equal.
- **Bounded queues:** each backend has its own concurrency limit and queue. Saturated queues return `429 Too Many Requests` with `Retry-After`.
- **Large-prefill limits:** request bodies are inspected to estimate new prefill tokens and cap concurrent large prefills per backend.
- **EWMA durations:** exponentially weighted moving averages guide load balancing without sending a herd of requests to the same backend.
- **Unified model catalog:** `/v1/models` aggregates model metadata from all configured backends.

## Install/Running

Docker is the fastest path to a running router. The container listens on port `80`.

### Docker image

Pull the pre-built image from GHCR and provide the required `BACKENDS` JSON array:

```bash
docker run -d --name llm-router-go -p 80:80 -p 127.0.0.1:9100:9100 \
  -e BACKENDS='[{"name":"primary","url":"http://vllm-1:8000","maxConcurrent":4,"tier":0}]' \
  -e METRICS_LISTEN=0.0.0.0:9100 \
  ghcr.io/djdembeck/llm-router-go
```

The backend URL must be reachable from the container. Adjust the published host port when port `80` is already in use. The dashboard and `/metrics/*` live on a separate management listener that binds `127.0.0.1:9100` inside the container by default — unreachable from the host. To serve the dashboard, set `METRICS_LISTEN=0.0.0.0:9100` (or the interface you want it on) and publish the port (`-p 127.0.0.1:9100:9100`): the container-side bind stays `0.0.0.0`, the host-side bind is loopback, so the port reaches the host at `http://127.0.0.1:9100` only and never on the network. Because it carries in-flight and per-conversation session data, if you ever proxy it beyond localhost, put it behind your TLS/auth reverse proxy, the same boundary as the rest of the management surface.

### Binary install

Install the Go command from the module, then provide the same environment variables when you run it:

```bash
go install github.com/djdembeck/llm-router-go@latest
```

The resulting `llm-router-go` binary is placed in the Go binary directory (`$GOBIN`, or `$GOPATH/bin` when `GOBIN` is unset).

## Usage

### Day-1 quickstart

Start the pre-built container with one vLLM backend:

```bash
docker run -d --name llm-router-go -p 80:80 -p 127.0.0.1:9100:9100 \
  -e BACKENDS='[{"name":"primary","url":"http://vllm-1:8000","maxConcurrent":4,"tier":0}]' \
  -e METRICS_LISTEN=0.0.0.0:9100 \
  ghcr.io/djdembeck/llm-router-go
```

The dashboard is on the management listener: `http://127.0.0.1:9100/`.

Check queue, concurrency, latency, and GPU-budget state:

```bash
curl http://127.0.0.1:80/stats
```

Send OpenAI-compatible API requests to the router's port. The router selects a backend for paths other than its management endpoints and forwards the request.

### Use an environment file

`.env.example` documents every supported environment variable. Copy it, set the backend URL, and source it before starting the binary:

```bash
cp .env.example .env
# Edit .env and set BACKENDS for the inference servers reachable by this process.
set -a
. ./.env
set +a
llm-router-go
```

The `.env.example` file contains a minimal backend entry and the default scalar settings. For multiple backends or tiered scheduling, use the full schema below.

## Configuration

The router is configured entirely through environment variables. `BACKENDS` is required; the scalar variables have the defaults shown here.

### Backends

`BACKENDS` is a JSON array. Each object describes one inference backend. This example includes every supported field:

```bash
BACKENDS='[
  {
    "name": "primary",
    "url": "http://vllm-1:8000",
    "maxConcurrent": 4,
    "tier": 0,
    "gpuWeight": 1,
    "blockOnTier0": 0,
    "maxQueueDepth": 2,
    "maxConcurrentLargePrefill": 1,
    "largePrefillThresholdTokens": 8192
  }
]'
```

| Field | Description |
| :--- | :--- |
| `name` | Required unique backend identifier. |
| `url` | Required backend endpoint URL. |
| `maxConcurrent` | Maximum concurrent requests. `0` means unlimited. |
| `tier` | Routing priority: `0` is a King and `1` is a Subject. Tier 0 backends take precedence. |
| `gpuWeight` | GPU cost attributed to this backend while Tier 0 is active. `0` has no GPU-budget cost. |
| `blockOnTier0` | For a secondary backend, block requests when the number of in-flight Tier 0 requests reaches this threshold. `0` disables the check. |
| `maxQueueDepth` | Maximum pending requests beyond `maxConcurrent`; defaults to `2`. |
| `maxConcurrentLargePrefill` | Maximum concurrent large-prefill requests. `0` disables large-prefill limiting. |
| `largePrefillThresholdTokens` | Estimated prompt-token count (whole conversation) that triggers large-prefill logic; defaults to `8192`. |

### Scalar environment variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BACKENDS` | Required | JSON array of backend server configurations. |
| `MAX_QUEUE_TIMEOUT` | `30s` | Maximum time a request waits in a queue before returning `429 Too Many Requests`. |
| `MAX_GPU_BUDGET` | `4` | Maximum weighted GPU usage while any Tier 0 (King) backend is active. |
| `MAX_BODY_BYTES` | `16777216` (16 MiB) | Maximum request body size in bytes. Larger requests receive HTTP `413` with `X-Router-Reason: body-too-large`. |
| `PREFILL_TOKENS_PER_SEC` | `10000` | Estimated prefill throughput used to bound how long a non-streaming request holds a large-prefill slot. The slot is released after the estimated prefill duration or the first response byte, whichever comes first. This bounds slot hold time but weakens head-of-line protection for very large non-streaming prefills. |
| `HISTORY_PATH` | *(empty)* | Optional file for the dashboard's history rings. Atomic snapshots every minute; empty means memory-only (history resets on restart). |
| `GPU_METRICS_URL` | *(empty)* | Optional Prometheus text endpoint for device GPU utilization/VRAM/temperature/power (DCGM exporter or nvidia_gpu_exporter). Empty disables device telemetry; engine KV/memory gauges remain available. |
| `METRICS_LISTEN` | `127.0.0.1:9100` | Address for the management listener that serves the dashboard and the `/metrics/*` endpoints (including in-flight and conversation session data, which is never exposed on the external listener). Point it at a LAN interface to expose the dashboard to that network. |

## API

The router listens on `:80` by default.

### `/health`

`GET /health` returns the plain-text health response `vLLM router OK`.

### `/stats`

`GET /stats` returns JSON status for every backend, including in-flight requests, queue depth, prefill queue state, and EWMA duration. The top-level response also reports `gpuUsed`, `gpuBudget`, `gpuWaiting`, and `tier0Inflight`.

### `/v1/models`

`GET /v1/models` queries all configured backends and returns one aggregated model catalog. If a backend cannot provide model metadata, the router returns a fallback entry named for that backend.

### Proxied requests

All other paths are proxied to a selected backend. OpenAI-compatible inference paths, including chat and completion requests, are forwarded after the router applies tier, GPU, concurrency, queue, and large-prefill limits.

Requests rejected because a queue, GPU budget, or prefill limit is full receive `429` with `Retry-After`, `X-Router-Reason`, and `X-Router-Backend` headers. Requests exceeding `MAX_BODY_BYTES` receive `413` with `X-Router-Reason: body-too-large`.


## Dashboard

The router embeds an operations dashboard (SvelteKit, served at `/` by the
same binary). It is built for the live question first: **what is running, how
hard are the GPUs working, how fast are prefill/decode/TTFT, and how full is
the KV cache** — then the same signals over the last 5 minutes to 30 days.

The first viewport is a ruled readings strip (running, waiting, TTFT, prefill,
decode, KV cache; a device row appears when GPU telemetry is configured), the
**Fleet Profile** — one elevation lane per backend with its KV level, live
request bars (prefill/decode), failure ticks, and a survey cursor — and the
**live session stack** beside it. Below them: backend detail (router slots vs
engine running/waiting, KV, KV pool, engine memory, TTFT, prefill, decode, hit
rate), the recent request feed with CSV export, and the history ranges
(`5m`, `1h`, `6h`, `24h`, `7d`, `30d`). With exactly one configured backend,
the dashboard automatically becomes a **server survey**: no fleet rail or
duplicate aggregate lane, one server identity, a wider profile, compact
server details, and no repeated backend column in the request log. Device
telemetry stays device-wide, including every reported GPU. Two or more
backends retain the fleet navigation and comparison tables. Pause, stale
feeds, and engine scrape failures do not change the layout mode. Five-minute
deltas use that server's own history, not a retained fleet aggregate; an
unknown reference stays `—`. If the feed falls back to `/stats`, engine and
GPU telemetry are labelled unavailable, not disabled or unconfigured.

Every figure carries its data class:

| Tag | Meaning |
| :--- | :--- |
| `router` | measured by the router: slots, queue, streaming first-byte TTFT, request/byte rates |
| `engine` | scraped from the backend's Prometheus `/metrics`: running/waiting, KV, prefill/decode tokens, engine TTFT |
| `device` | scraped from an optional DCGM / nvidia exporter: device utilization, VRAM, temperature, power |
| `est` | body-based token estimate (~4 bytes/token), never measured decode |

The dashboard and the metrics endpoints share one dedicated management
listener — `METRICS_LISTEN` (default `127.0.0.1:9100`) — which is separate
from the external API listener. Because `/metrics/sessions` exposes
in-flight and per-conversation request data, that surface binds to the
loopback interface by default; point `METRICS_LISTEN` at a LAN interface
only when the deployment boundary (TLS/auth reverse proxy) covers it. The
external listener keeps `/health`, `/stats`, and `/v1/*` only. It reads four
endpoints:

| Endpoint | Description |
| :--- | :--- |
| `GET /metrics/stream` | SSE feed of full live snapshots every `?interval=` (default `500ms`, clamped `200ms`–`10s`). Per-backend slots, EWMA duration, streaming TTFT (EWMA + latest first-byte), **30s trailing arrival rates**, cumulative counters, engine metrics, GPU budget state, optional `gpuDevices`, and fleet `totals` (including engine-aggregated throughput and KV peak). |
| `GET /metrics/history` | Server-side time series. `?range=5m\|1h\|6h\|24h\|7d\|30d` (default `1h`), optional repeated `?backend=`. Dense grid-aligned arrays (`t[i] = from + i*stepMs`); a bucket with no samples is `null`, never forward-filled. Includes per-backend series, the fleet aggregate, device series, and router TTFT p50/p95 with the actual sample count and coverage. |
| `GET /metrics/burst` | The most recent completed requests (bounded ring, capacity `2000`, `?limit=` default `500`), newest last, including router-originated rejections. |
| `GET /metrics/sessions` | The in-flight request stack plus conversation-grouped sessions (polling endpoint). |

The frontend connects to `/metrics/stream` via `EventSource` and falls back
to polling `/stats` if the stream is unavailable; `/stats` is served on both
listeners so the fallback resolves from the dashboard's origin. In
`bun run dev` with no router
reachable, a dev-only synthetic feed is used and the top bar marks it
`DEV MOCK`; the production build never loads it. Rebuilt on every code change
with `cd web && bun run build` and embedded with the `webui` build tag (the
Docker image does this automatically; the plain `go build` ships a 404 stub
for the dashboard until `web/build` exists).

### History

A single sampler goroutine records one coherent fleet snapshot per second,
independent of dashboard clients, into three rings per series:

| Ring | Step | Retention |
| :--- | :--- | :--- |
| raw | 1s | 1 hour |
| minute rollup | 1m | 7 days |
| hour rollup | 1h | 30 days |

Rollups store averages for gauges/rates and maxima for KV/TTFT pressure. Set
`HISTORY_PATH=/data/history.gob` (on a mounted volume) to keep the rings
across restarts; writes are atomic snapshots every minute. The file is
ignored if it is unreadable or from a different version. Without it, history
is memory-only and resets when the process restarts — the live view is never
affected.

### Engine metrics (v2)

Each backend's engine block is the backend's **own** execution truth scraped
from its Prometheus `/metrics` at 1s. It carries running/waiting, KV pressure,
prefill (compute and cache-hit separately), decode throughput, interval
latency means, lifetime counters, finished-reason breakdown (vLLM), pool
statistics (SGLang), memory, cache hit rate, and the context/SLO caps. Fields
an engine does not report render as `null` (`—`). vLLM's
`cache_config_info{kv_cache_size_tokens="..."}` supplies group-aware KV token
capacity, summed across data-parallel engines; a missing/invalid capacity
label leaves it unknown. The pool reads used/capacity independently (for
example `—/992k`), never inferring used tokens from KV percentage or multiplying
block count by block size for hybrid models. vLLM does not expose the
SGLang-specific weight/KV memory gauges; device VRAM and utilization require
`GPU_METRICS_URL`. Prefill rates can legitimately be zero between prompt
bursts, even while requests decode. The full field table lives in
`.impeccable/metrics-contract.md`.

Failure handling is explicit: a 404/405 endpoint is `off` and re-probed every
30s (fixed, since the route only reappears when the engine restarts with
metrics enabled); other failures (`401`, `403`, `5xx`, unreachable) are `err`
with the HTTP status and message and re-probed on the **next 1s tick** — no
backoff, because they can recover at any moment — while a successful scrape
clears any backoff. On any failed
scrape the interval means and rates are zeroed (with their sample counts
cleared) and every counter/histogram baseline is reset, so the first
successful scrape after an outage is baseline-only rather than a delta across
the gap. Gauges keep their last known values for backend detail, but the
dashboard treats a non-`ok` engine as unavailable — `—` in the live cells and
hatched in the profile — never as current load.

### Sessions

`GET /metrics/sessions` returns the router's **in-flight request stack**
(`live`, oldest on top) plus **conversation-grouped sessions** (`sessions`).
A session is identified by the SHA-1 of the conversation prefix — each
message's role and content, length-framed, for every message except the last,
truncated to 12 hex chars. This makes formatting changes harmless and keeps
different role partitions distinct; identical prefixes from independent
clients still share a session (there is no client identity in the request).
A new conversation forks at its second turn. Non-chat requests have no
session id but still appear in `live` and the request feed.

Live entries and admission state are finalized even when an upstream stream
is truncated or a client disconnects. The request history records `502` for
an aborted proxy copy or `499` for a client-cancelled request (unless an
explicit failure status was already recorded); an already-started HTTP
response is not rewritten. A router restart clears live entries leaked by
older builds; live requests are not restored from persisted history.

Each session shows its request count, first/last activity, active flag,
in-flight count, the latest request's context/new/cached token estimates,
mean streaming TTFT, total duration, total output tokens, and last status;
recent sessions expand to their last 32 requests. All token figures here are
**estimates** (body-based, ~4 bytes/token) — see the measurement boundary
below — never measured decode tokens.

The live stack carries two prefill-progress fields per in-flight request:
`estPrefillMs` (estimated prefill duration — `newTok` at
`PREFILL_TOKENS_PER_SEC`, a router estimate, `0` for non-streaming requests
and zero `newTok`) and `streamMs` (UnixMilli of the first response body byte,
`0` while still admitted). The dashboard shows an amber prefill bar that
fills against `estPrefillMs` while the request is admitted and flips to the
measured first-byte age on first byte.

### Measurement boundary

- **`router` — measured at admission.** Requests, bytes, queue/concurrency/GPU
  budget state, total-duration EWMA, and streaming first-byte TTFT. `reqRate`,
  `bytesRate`, and `tokEstRate` are **trailing 30s arrival averages** (300 ×
  100ms buckets, evaluated every 500ms), not completion deltas; a request
  contributes from the moment it is admitted and leaves the window after 30s.
- **`engine` — measured by the engine.** Running/waiting, KV pressure, real
  prefill/decode token rates, interval latency means, cache hit rate, and
  pool/memory gauges. `prefillTokS` is **compute-only** for both families;
  `prefillCacheTokS` is the cache-hit share and `prefillTotalTokS` is their
  sum. Engine histogram means carry an observation count (`ttftSamples`, ...);
  zero samples means no data, rendered `—`.
- **`est` — body-based estimates.** `tokEst*`, session token figures, and
  `estPrefillMs` are ~4 bytes/token estimates of NEW prefill tokens; they are
  never presented as measured decode throughput.
- **`device` — optional and separate.** `GPU_METRICS_URL` points at a
  Prometheus text endpoint (DCGM exporter or nvidia_gpu_exporter). The router
  normalizes utilization to 0–100%, memory to MiB, temperature to °C, and
  power to W, and identifies devices by UUID (or hostname + GPU index). These
  are device-wide figures and are never attributed to a backend. Without the
  variable, device panels show an explicit empty state; the router never
  derives device utilization from engine counters.
- **GPU budget is not GPU utilization.** `gpuUsed`/`gpuBudget` are the
  router's weighted admission-budget units. Device utilization comes only
  from the `device` source.

## Architecture

The application is a single-file Go binary (`main.go`) using only the standard library. It builds without external runtime dependencies.

### Tier system

Backends are assigned as **King (Tier 0)** or **Subject (Tier 1)**. Tier 0 backends are always admitted with priority. When a Tier 0 request is in flight, Tier 1 admission consumes the remaining `MAX_GPU_BUDGET` according to each backend's `gpuWeight`. When no King is active, the GPU-budget check is not applied.

### GPU budget and slot managers

A weighted GPU semaphore models shared GPU capacity. Each backend also has a slot manager with an optional concurrency limit and bounded queue. This keeps one saturated backend from consuming all request capacity and gives callers a bounded wait followed by a useful `429` response.

### Prefill protection and load balancing

The router estimates new prefill tokens from request bodies. Backends can cap concurrent large prefills to prevent a massive prompt from blocking smaller requests. Large-prefill slots are bounded using `PREFILL_TOKENS_PER_SEC` for non-streaming requests and release on the first response byte for streaming requests.

Per-backend request durations are tracked with an exponentially weighted moving average (EWMA). Routing uses these durations with current load to reduce latency and avoid herd behavior.

## Building

### Build the binary

The project requires Go 1.25 or newer and uses only the standard library for
the router itself:

```bash
go build -o llm-router-go .          # dashboard is a 404 stub
cd web && bun install && bun run build
go build -tags webui -o llm-router-go .   # embedded dashboard
```

Run the resulting binary with `BACKENDS` and any optional environment variables from [Configuration](#configuration). Without `-tags webui` the dashboard path returns 404 (`web/build` is absent); the Docker image and release builds always embed it.

### Build the Docker image

The repository includes a multi-stage Alpine Dockerfile. Build the image locally with:

```bash
docker build -t llm-router-go .
```

Run the local image by replacing `ghcr.io/djdembeck/llm-router-go` in the quickstart command with `llm-router-go`.

## Contributing

Use the repository's Gitflow model: `develop` is the integration branch and `main` is the production branch. Use Conventional Commits for pull requests.

Keep the single-file, standard-library architecture intact unless the change requires otherwise. Before opening a pull request, run the repository's formatting, vetting, and test checks (`gofmt`, `go vet`, and `go test`).

## License

This project is licensed under the [MIT License](LICENSE) (SPDX: `MIT`).
