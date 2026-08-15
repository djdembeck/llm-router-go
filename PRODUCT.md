# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Stack

SvelteKit + Tailwind, same convention as the sibling projects (annalist, bragibooks): static-build SvelteKit app served and embedded into the Go binary via `go:embed`, so the router stays a single deployable artifact. User-chosen; confirmed 2026-08-14.

## Users

ML-infrastructure operators running one or more OpenAI-compatible inference servers (vLLM, SGLang) on shared GPU hardware. Their job is keeping request latency predictable when multiple backends contend for the same GPUs — a single massive prefill can occupy a GPU and starve smaller requests. This is an operate-in-situ tool: the user is checking it while traffic is live, not adopting it.

## Product Purpose

A GPU-aware reverse proxy router that sits in front of vLLM and SGLang backends and routes requests using GPU utilization, concurrency, queue depth, and latency signals — instead of naive load balancing by request count. It exists so that when backends share GPU hardware, capacity is not wasted and large prefills do not starve smaller requests. Success is a fleet where the primary (King) instances stay responsive under contention while secondary capacity is used deliberately.

## Positioning

A scheduler for shared-GPU inference: tiered admission (King/Subject), weighted GPU budgets, bounded per-backend queues, and per-backend large-prefill caps — scheduling signals a request-count balancer cannot express. Single Go binary, standard library only, no external dependencies. A neighboring load balancer cannot truthfully copy the combination of tier semantics, GPU-budget accounting, and prefill-aware admission in a zero-dependency binary.

## Operating Context

- Deployed as a Docker container (listens on `:80`) or a Go binary, always behind a TLS/authenticating reverse proxy (Caddy, Nginx, Traefik) — the router itself has **no authentication**; it is never exposed to untrusted networks directly.
- Configured entirely through environment variables: a required `BACKENDS` JSON array plus scalar knobs (`MAX_QUEUE_TIMEOUT`, `MAX_GPU_BUDGET`, `MAX_BODY_BYTES`, `PREFILL_TOKENS_PER_SEC`). `.env.example` documents every variable.
- Management endpoints: `GET /health` (plain text), `GET /stats` (per-backend JSON: in-flight, queue depth, prefill queue state, EWMA duration; top-level `gpuUsed`/`gpuBudget`/`gpuWaiting`/`tier0Inflight`), `GET /v1/models` (aggregated model catalog). All other paths are proxied to a selected backend.
- Rejections are explicit: `429` with `Retry-After`, `X-Router-Reason`, `X-Router-Backend`; oversized bodies get `413` with `X-Router-Reason: body-too-large`.
- The planned web dashboard is a new surface; no UI exists yet. It will visualize the `/stats` data (backends, queues, GPU budget, latency) for live operation.

## Capabilities and Constraints

- Tier system: Tier 0 (**King**) backends are admitted with priority; Tier 1 (**Subject**) backends consume only the remaining `MAX_GPU_BUDGET` while a King is active, weighted by per-backend `gpuWeight`. `blockOnTier0` can fully block a secondary at a King in-flight threshold.
- Per-backend slot manager: `maxConcurrent` (0 = unlimited) plus `maxQueueDepth` (default 2); saturated queues wait a bounded time then return `429`.
- Large-prefill protection: request bodies are inspected to estimate new prefill tokens (multi-turn: only the last message counts, assuming prefix caching; ~4 bytes/token); `maxConcurrentLargePrefill` caps concurrent large prefills; slots bound by `PREFILL_TOKENS_PER_SEC` for non-streaming requests and release on the first response byte for streaming.
- EWMA per-backend duration tracking guides routing and avoids herd behavior.
- Architecture constraint (stated in README, binding): single-file Go (`main.go`), **standard library only**, no external runtime dependencies. The dashboard is served/embedded from this binary, not a separate service.
- Terminology is load-bearing: King (Tier 0) / Subject (Tier 1), GPU budget, large prefill, bounded queue.

## Brand Commitments

- Name: **llm-router-go**, module `github.com/djdembeck/llm-router-go`, MIT licensed.
- Docker image at `ghcr.io/djdembeck/llm-router-go`.
- No logo, formal brand system, or marketing commitments exist.
- Gitflow: `develop` integrates, `main` is production; Conventional Commits for PRs.

## Evidence on Hand

- README.md documenting install, configuration, API, and architecture — the authoritative source of product claims.
- Working single-file Go implementation (`main.go`, ~900 lines) with test coverage (`main_test.go`).
- `.env.example` documenting all environment variables; multi-stage Alpine `Dockerfile`; CI/release workflows in `.github/workflows`.
- No dashboard, no testimonials, no customers, no benchmarks, no press — future work must not fabricate any of them.

## Product Principles

1. **Signals over counts.** Scheduling decisions must reflect GPU and queue reality, not request volume; a naive count-based balancer is the anti-pattern this product exists to replace.
2. **Bounded failure.** Every saturation path ends in a bounded wait and an explicit `429` with a reason and `Retry-After`, never a silent hang or an unbounded queue.
3. **Zero dependencies, one artifact.** The router is one binary, standard library only; the dashboard must stay embedded and served by that same binary.
4. **Deliberate secondary capacity.** Subject backends are used only with explicit budget headroom — spillover is a policy, not an accident.
5. **Operate-in-situ truth.** What an operator sees (stats, dashboard) must be the router's actual in-flight state, not a stale or derived approximation.

## Accessibility & Inclusion

No product-specific requirement established. Standard operability expectations apply: the dashboard is used by an operator in a live-ops context, so scanability and fast state reading matter more than decorative polish.
