# llm-router-go

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
docker run -d --name llm-router-go -p 80:80 \
  -e BACKENDS='[{"name":"primary","url":"http://vllm-1:8000","maxConcurrent":4,"tier":0}]' \
  ghcr.io/djdembeck/llm-router-go
```

The backend URL must be reachable from the container. Adjust the published host port when port `80` is already in use.

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
docker run -d --name llm-router-go -p 80:80 \
  -e BACKENDS='[{"name":"primary","url":"http://vllm-1:8000","maxConcurrent":4,"tier":0}]' \
  ghcr.io/djdembeck/llm-router-go
```

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
| `largePrefillThresholdTokens` | Estimated new-token count that triggers large-prefill logic; defaults to `8192`. |

### Scalar environment variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BACKENDS` | Required | JSON array of backend server configurations. |
| `MAX_QUEUE_TIMEOUT` | `30s` | Maximum time a request waits in a queue before returning `429 Too Many Requests`. |
| `MAX_GPU_BUDGET` | `4` | Maximum weighted GPU usage while any Tier 0 (King) backend is active. |
| `MAX_BODY_BYTES` | `16777216` (16 MiB) | Maximum request body size in bytes. Larger requests receive HTTP `413` with `X-Router-Reason: body-too-large`. |
| `PREFILL_TOKENS_PER_SEC` | `10000` | Estimated prefill throughput used to bound how long a non-streaming request holds a large-prefill slot. The slot is released after the estimated prefill duration or the first response byte, whichever comes first. This bounds slot hold time but weakens head-of-line protection for very large non-streaming prefills. |

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

The router embeds a live operations dashboard (SvelteKit, served at `/` by the
same binary). One tessellated "fold sheet" shows every backend as a live
cell: current in-flight vs its limit, queue depth, large-prefill state, EWMA
duration, streaming time-to-first-byte (TTFT), live request/byte/token rates,
and the GPU budget gauge while a King is active. Opening a fold gives a 30s /
60s / 5m trace (5-minute history buffer) with signal switcher — including the
backend's **own** running/queue/KV-cache/prefill/decode state scraped from its
Prometheus `/metrics` — plus full readout. A fleet strip shows aggregate
requests/sec, TTFT, estimated prefill token rate (`est`), and **real decode
token throughput** (`real`, from the engines); a live request tape streams the
most recent requests including router rejections (429/413).

The dashboard is served by the same listener as the management endpoints (so
it inherits the same deployment boundary: put it behind your TLS/auth reverse
proxy). It reads three endpoints:

| Endpoint | Description |
| :--- | :--- |
| `GET /metrics/stream` | SSE feed of full live snapshots every `?interval=` (default `500ms`, clamped `200ms`–`10s`). One JSON frame per `data:` line: per-backend slots, EWMA duration, streaming TTFT (EWMA + latest first-byte sample), live arrival rates (30s window), cumulative counters, engine metrics (v2 — see below), GPU budget state, tier-0 in-flight, and fleet totals. |
| `GET /metrics/burst` | The most recent completed requests (bounded ring, default capacity `500`, `?limit=` up to `1000`), newest last, including router-originated rejections. |
| `GET /metrics/sessions` | The in-flight request stack plus conversation-grouped sessions (polling endpoint, see below). |

The frontend connects to `/metrics/stream` via `EventSource` and falls back to
polling `/stats` if the stream is unavailable. Rebuilt on every code change
with `cd web && bun run build` and embedded with the `webui` build tag (the
Docker image does this automatically; the plain `go build` ships a 404 stub
for the dashboard until `web/build` exists).

### Engine metrics (v2)

Each backend's fold now exposes the backend's **own** execution truth scraped
from its Prometheus `/metrics` at 1s, as a full v2 `engine` block. Beyond the
original running / waiting / KV% / prefill / decode / TTFT, the sheet now
shows:

- **Requests** — lifetime completed requests (`reqDoneTotal`) with a
  **finished-reason breakdown** (`finReasons`: stop / length / abort / error /
  repetition; vLLM), plus streaming vs non-streaming splits and aborted /
  preempted / retracted counts.
- **Token totals + rates** — lifetime prompt/generation token totals, real
  prefill throughput split into **compute-only** (`prefillTokS`) and
  **cache-hit** (`prefillCacheTokS`) prefill, decode throughput, and retracted
  input-token rate. Prefix-cache and multimodal-cache hit rates with raw
  query/hit totals.
- **Memory pools** — full / SWA / Mamba pool usage % and pool token stats
  (used / capacity / free / evictable, Mamba used / capacity), host-tier
  (HiCache) token usage, KV and weight memory (GB).
- **Per-stage latency** — prefill / decode / queue / inter-token / end-to-end
  means (ms), plus the streaming vs non-streaming TTFT split and per-request
  mean prompt / generation token length.
- **Capacity** — SLO-constrained running-request cap and context length
  (SGLang); waiting-by-reason (capacity / deferred) for vLLM.

Engine fields the engine doesn't report render as `null` (the sheet shows
`—`). The field table lives in `.impeccable/metrics-contract.md`.

Availability / capability flags:

| Condition | Effect |
| :--- | :--- |
| **SGLang started without `--enable-metrics`** (default) | `/metrics` returns 404 → engine `status: "off"`, sheet renders `—`. Start SGLang with `--enable-metrics` to populate it. |
| **vLLM** | Always serves `/metrics`; no flag needed. |
| **SGLang Mamba pool** (`mambaPct`, `mambaUsedTok`, `mambaCapTok`) | Appears only with a hybrid / Mamba model; `null` otherwise. |
| **vLLM finished-reason breakdown** (`finReasons`) | Always available — requires no extra flag. |
| **SGLang HiCache** (`hicacheHostUsedTok` / `hicacheHostCapTok`) | Appears only when host-tier caching is enabled. |

### Sessions

`GET /metrics/sessions` returns the router's **in-flight request stack**
(`live`, oldest on top) plus **conversation-grouped sessions** (`sessions`).
A session is identified by the SHA-1 of the conversation prefix — the raw
JSON of every message except the last, truncated to 12 hex chars — so the same
conversation groups together and a new conversation forks at its second turn.
Non-chat requests (completions with a `prompt`) have no session.

Each session shows its request count, first/last activity, active flag,
in-flight count, the latest request's context/new/cached token estimates,
mean streaming TTFT, total duration, total output tokens, and last status;
recent sessions expand to their last 32 requests. All token figures here are
**estimates** (body-based, ~4 bytes/token) — see the measurement boundary
below — never measured decode tokens.

### Measurement boundary

Two token figures coexist, labeled differently:

- **`est` — estimated prefill tokens.** The router measures requests, bytes,
  queue/concurrency/GPU state, total-duration EWMA, and streaming first-byte
  TTFT directly. It does **not** count output tokens: it streams response
  bytes through without parsing upstream usage. `tokEst*` figures are the
  router's estimate of **new prefill tokens** from the request body (~4
  bytes/token, last-message-only for multi-turn). Requests/sec and bytes/sec
  are measured.
- **`real` — engine token throughput.** The router scrapes each backend's own
  Prometheus `/metrics` endpoint at 1s and derives real prompt/prefill and
  completion/decode token rates from the engine's counters, plus in-engine
  running/waiting gauges and KV-cache pressure. vLLM serves `/metrics`
  always; SGLang only when started with `--enable-metrics` — when the endpoint
  is absent or unreachable the dashboard says `off`/`err` and renders `—`
  rather than guessing. This is additive truth, not a replacement for the
  router's admission view.


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
