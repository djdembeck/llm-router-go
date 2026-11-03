# Agents

## Routing and streaming changes

This Go reverse proxy serves OpenAI-compatible inference backends and an embedded SvelteKit dashboard. Read README "Session admission" and the lease manager in `main.go` before routing/capacity changes; read README "Configuration" for the timeout contract before SSE changes.

- Preserve the lease manager's admission authority and existing admission-off/keyless behavior.
- An admitted request owns its ticket through handler completion, including cancellation and downstream rejection; release it exactly once. Cancellation before grant must not mint a lease.
- Bound incoming reads and keep-alive without imposing a response write timeout on SSE/long generation; preserve graceful stream draining at shutdown.
