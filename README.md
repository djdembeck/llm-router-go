# llm-router-go

GPU-aware reverse proxy router for vLLM and SGlang LLM inference backends.

## Security

This proxy has **no built-in authentication or authorization**. It blindly forwards any request it receives to the configured backends.

**Always deploy behind a reverse proxy** (e.g., Caddy, Nginx, Traefik) that handles:
- Authentication (API keys, tokens, etc.)
- TLS termination
- Request validation and rate limiting

Do not expose `llm-router-go` directly to untrusted networks.

## Quick Start

Build and run via Docker:

```bash
docker build -t llm-router-go .
docker run -d -p 80:80 \
  -e BACKENDS='[{"name":"primary","url":"http://vllm-1:8000","maxConcurrent":4,"tier":0}]' \
  llm-router-go
```

## Configuration

The router is configured entirely via environment variables.

### Environment Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `BACKENDS` | Required | JSON array of backend server configurations. |
| `MAX_QUEUE_TIMEOUT` | `30s` | Maximum time a request waits in queue before returning `429 Too Many Requests`. |
| `MAX_GPU_BUDGET` | `4` | Max weighted GPU usage when any Tier 0 (King) backend is active. |

### Backends JSON Schema

Each object in the `BACKENDS` array supports:

- `name` (string, required): Unique identifier for the backend.
- `url` (string, required): Backend endpoint URL.
- `maxConcurrent` (int): Max concurrent requests. `0` for unlimited.
- `tier` (int): Routing priority. `0` for King, `1` for Subject.
- `gpuWeight` (int): GPU cost attributed to this backend when Tier 0 is active.
- `blockOnTier0` (int): Request block threshold when King backends are saturated. `0` to disable.
- `maxQueueDepth` (int): Max pending requests. Defaults to `2`.
- `maxConcurrentLargePrefill` (int): Max concurrent large prefill requests. `0` to disable.
- `largePrefillThresholdTokens` (int): Token count to trigger large prefill logic. Defaults to `8192`.

## Endpoints

The router listens on port `:80`.

- `/stats`: Returns JSON status of all backends, including queue depths, EWMA durations, and active concurrency.
- `/v1/models`: Aggregates model information from all configured backends.
- `/*`: All other paths are proxied to the selected backend based on load balancing logic.

## Architecture

`llm-router-go` implements a specialized scheduling layer to optimize GPU utilization across multiple inference servers.

### Tier System
Backends are assigned as either **King (Tier 0)** or **Subject (Tier 1)**. Tier 0 backends take precedence; Tier 1 backends are utilized based on available `MAX_GPU_BUDGET` to prevent resource starvation of primary instances.

### GPU Budget & Slot Manager
The router tracks weighted GPU usage. When a Tier 0 backend is active, the router enforces a global GPU budget, limiting the concurrency of Tier 1 backends based on their `gpuWeight`.

### Queue & Load Balancing
Each backend maintains its own request queue. Requests are routed using an Exponentially Weighted Moving Average (EWMA) of request durations to minimize latency and avoid "herd" behavior.

### Large Prefill Detection
To prevent "head-of-line" blocking caused by massive prompt prefills, the router tracks prefill sizes and limits the number of concurrent large prefill operations per backend.

## Building from Source

The project is written in Go and uses only the standard library.

```bash
go build -o llm-router-go .
```

## License

MIT SPDX-License-Identifier: MIT
