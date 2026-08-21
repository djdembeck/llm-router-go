<script lang="ts">
  import type { LiveReq } from "./metrics.js";

  // Large-cold-prefill progress: the router estimates new-prefill tokens at
  // admission (~4 bytes/token, body-based) and the engines prefill at a
  // bounded rate, so an admitted request's prefill ETA is pure estimate —
  // gold + est, never blue. A request that has not produced its first byte
  // yet is "still prefilling": elapsed vs estPrefillMs gives the bar. On
  // first byte it flips to streaming (streamMs set) and renders muted for
  // one cycle before leaving the admitted phase.

  interface Props {
    live: LiveReq[];
    /** 1s wall-clock tick from the parent. */
    now: number;
    /** newTok floor for "large"; matches the default LargePrefillThresholdTokens. */
    threshold?: number;
  }

  let { live, now, threshold = 8192 }: Props = $props();

  interface Row {
    r: LiveReq;
    pct: number;
    etaS: number;
    remS: number;
  }

  const rows = $derived.by(
    () =>
      live
        .filter(
          (r) =>
            r.newTok >= threshold &&
            r.estPrefillMs > 0 &&
            (r.phase === "admitted" || r.streamMs > 0),
        )
        .map((r) => {
          const ageMs = now - r.startMs;
          const frac = Math.min(1, ageMs / r.estPrefillMs);
          return {
            r,
            pct: Math.max(0, Math.round(frac * 100)),
            etaS: Math.round(r.estPrefillMs / 1000),
            remS: Math.max(0, Math.ceil((r.estPrefillMs - ageMs) / 1000)),
          };
        }),
  );
</script>

{#if rows.length > 0}
  <div class="pf-card" aria-label="large cold prefills in progress">
    <div class="pf-subhead">large cold prefill · <em>est</em></div>
    {#each rows as { r, pct, etaS, remS } (r.id)}
      {#if r.streamMs > 0}
        <div class="pf-row pf-done">
          <span class="pf-be">{r.backend}</span>
          <span class="pf-tok">{r.newTok} tok <em class="est">est</em></span>
          <span class="pf-note">
            streaming — prefilled in {((r.streamMs - r.startMs) / 1000).toFixed(1)}s est
          </span>
        </div>
      {:else}
        <div class="pf-row">
          <span class="pf-be">{r.backend}</span>
          <span class="pf-tok">{r.newTok} tok <em class="est">est</em></span>
          <span
            class="pf-bar"
            role="img"
            aria-label="prefill ~{pct}% est"
          >
            <span class="pf-fill" style="width: {pct}%"></span>
          </span>
          <span class="pf-pct">{pct}%</span>
          <span class="pf-eta">~{etaS}s est</span>
          <span class="pf-rem">{remS === 0 ? "due" : `~${remS}s`}</span>
        </div>
      {/if}
    {/each}
  </div>
{/if}

<style>
  .pf-card {
    display: flex;
    flex-direction: column;
  }
  .pf-subhead {
    padding: 4px 12px 2px;
    font-size: 9px;
    letter-spacing: 0.1em;
    text-transform: uppercase;
    color: var(--tx-3);
  }
  .pf-subhead em {
    color: var(--gold);
    font-style: normal;
  }
  .pf-row {
    display: flex;
    align-items: baseline;
    gap: 12px;
    padding: 5px 12px;
    border-bottom: 1px solid rgba(166, 166, 160, 0.12);
    font-size: 11px;
    font-variant-numeric: tabular-nums;
    min-width: 0;
  }
  .pf-be {
    color: var(--tx-1);
    min-width: 90px;
  }
  .pf-tok {
    color: var(--gold);
    white-space: nowrap;
  }
  .pf-tok .est {
    color: var(--gold);
    font-style: normal;
    font-size: 9px;
    letter-spacing: 0.1em;
  }
  .pf-bar {
    position: relative;
    flex: 1;
    height: 6px;
    min-width: 80px;
    background: rgba(201, 162, 75, 0.14);
    border: 1px solid rgba(201, 162, 75, 0.45);
  }
  .pf-fill {
    position: absolute;
    inset: 0 auto 0 0;
    background: var(--gold);
  }
  .pf-pct {
    color: var(--tx-1);
    min-width: 36px;
    text-align: right;
  }
  .pf-eta,
  .pf-rem {
    color: var(--tx-3);
    white-space: nowrap;
  }
  .pf-row.pf-done .pf-be {
    color: var(--tx-3);
  }
  .pf-note {
    color: var(--tx-3);
    letter-spacing: 0.02em;
  }
</style>
