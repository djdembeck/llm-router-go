<script lang="ts">
  import { flip } from "svelte/animate";
  import { fade } from "svelte/transition";
  import { cubicOut } from "svelte/easing";
  import type { SessionsFeed } from "./api";
  import { DASH, calmMotion, fmtAge, fmtCompact, fmtInt, fmtMs, fmtPct } from "./format";

  let {
    feed,
    now,
    paused,
    stale,
    tall = false,
    singleBackend,
  }: {
    feed: SessionsFeed | null;
    now: number;
    paused: boolean;
    stale: boolean;
    tall?: boolean;
    singleBackend: string | null;
  } = $props();

  const live = $derived(feed?.live ?? []);
  const single = $derived(singleBackend != null);
  const flipDur = calmMotion ? 0 : 220;
</script>

<section class="sessions" class:tall aria-label="live sessions">
  <div class="head">
    <div class="row">
      <span class="label">LIVE SESSIONS</span>
      <span class="chip {live.length ? 'info' : 'muted'}">{live.length}</span>
    </div>
    <div class="row right">
      <span class="detail muted">{stale ? "last data" : paused ? "held" : "oldest first"}</span>
    </div>
  </div>

  <div class="list" role="list">
    {#each live as l (l.id)}
      {@const age = now - l.startMs}
      {@const firstByte = l.streamMs > 0 ? l.streamMs - l.startMs : null}
      {@const cachedTok = Math.max(0, l.ctxTok - l.newTok)}
      {@const hit = l.ctxTok > 0 ? (cachedTok / l.ctxTok) * 100 : null}
      {@const prefillRatio = l.estPrefillMs > 0 ? age / l.estPrefillMs : null}
      {@const pct = l.phase === "streaming" ? 100 : Math.min(100, (prefillRatio ?? 0) * 100)}
      {@const rowTitle = `${single ? `${l.backend} · ` : ""}${l.path}${l.sess ? ` · ${l.sess.slice(0, 12)}` : ""}`}
      <div
        class="req"
        role="listitem"
        animate:flip={{ duration: flipDur, easing: cubicOut }}
        title={rowTitle}
      >
        <div class="req-top">
          <span class="req-name mono">{single ? `REQUEST #${l.id}` : l.backend}</span>
          <span class="phase {l.phase}">
            <span class="dot"></span>{l.phase === "streaming" ? "stream" : "admitted"}
          </span>
        </div>
        <div class="metrics">
          <span
            class="metric"
            title="Context tokens estimated from request body bytes (~4 bytes/token)."
          >
            <span class="k">CTX</span><span class="v">~{fmtCompact(l.ctxTok)}</span>
          </span>
          <span
            class="metric"
            title={`Estimated cache reuse: (ctx − new) ÷ ctx from router token estimates. The engine's measured cache-hit rate is in ${single ? "Server" : "Backend"} Detail.`}
          >
            <span class="k">KV HIT</span>
            <span class="v" class:hit-low={hit != null && hit < 40}>
              {hit == null ? DASH : `~${fmtPct(hit)}`}
            </span>
          </span>
          <span
            class="metric"
            title="New/prefill tokens estimated from the request body."
          >
            <span class="k">NEW</span><span class="v">~{fmtCompact(l.newTok)}</span>
          </span>
          <span
            class="tag est"
            title="Router-side estimate, not an engine counter: tokens are body bytes ÷ 4; KV hit = (ctx − new) ÷ ctx."
          >est</span>
          <span class="metrics-sep" aria-hidden="true"></span>
          <span class="metric ttft">
            <span class="k">TTFT</span>
            <span class="v">{firstByte != null ? fmtMs(firstByte) : DASH}</span>
          </span>
        </div>
        <div class="req-bot">
          <span class="track" aria-hidden="true">
            <span class="fill" style:width={`${pct}%`}></span>
          </span>
          <span
            class="phase-detail mono"
            class:over={l.phase === "admitted" && prefillRatio != null && prefillRatio > 1.5}
          >
            {#if l.phase === "streaming"}
              {#if l.estPrefillMs > 0}
                <span title="Router estimate at PREFILL_TOKENS_PER_SEC; no queue or network time. TTFT is measured.">
                  est prefill {fmtMs(l.estPrefillMs)}
                </span>
              {:else if !l.stream}
                non-streaming
              {:else}
                first byte
              {/if}
            {:else if prefillRatio != null}
              {prefillRatio > 1.5
                ? `queued ~${prefillRatio.toFixed(1)}× est`
                : `prefill ${fmtPct(Math.min(100, prefillRatio * 100))} · est ${fmtMs(l.estPrefillMs)}`}
            {:else if !l.stream}
              non-streaming
            {:else}
              waiting for first byte
            {/if}
          </span>
          <span class="age mono">{fmtAge(age)}</span>
        </div>
      </div>
    {:else}
      <div class="empty detail muted" in:fade={{ duration: calmMotion ? 0 : 160 }}>
        {stale
          ? "feed stale — no new requests are arriving"
          : paused
            ? "paused — no new requests are being read"
            : "no requests in flight"}
      </div>
    {/each}
  </div>

  <div class="foot">
    <span class="detail muted">
      {live.length > 0
        ? `oldest ${fmtAge(now - live[0].startMs)} · ${fmtInt(live.length)} in flight`
        : "the stack is quiet"}
    </span>
    <span class="detail muted">est = router token estimate · TTFT measured</span>
  </div>
</section>

<style>
  .sessions {
    display: flex;
    flex-direction: column;
    min-width: 0;
    height: 100%;
    /* Clip FLIP/stale rows; a live burst must never paint into the next band. */
    overflow: hidden;
    contain: paint;
    border: 1px solid var(--rule);
    background: var(--panel);
    transition:
      background-color var(--dur) var(--ease-soft),
      border-color var(--dur) var(--ease-soft);
  }
  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s2);
    min-height: 40px;
    padding: 6px var(--s3);
    border-bottom: 1px solid var(--rule);
  }
  .list {
    display: flex;
    flex-direction: column;
    flex: 0 0 auto;
    height: 320px;
    overflow-y: auto;
  }
  /* Fixed height, not max-height: the stack must not resize the lower band as
     requests arrive and finish. The log toggle trades the two fixed heights. */
  .sessions.tall .list {
    height: 560px;
  }
  .req {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 7px var(--s3);
    border-bottom: 1px solid var(--rule);
  }
  .req {
    transition: background-color var(--dur-fast) var(--ease-out);
  }
  .req:hover {
    background: var(--panel-2);
  }
  .req-top {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }
  .req-name {
    font-size: 10px;
    font-weight: 400;
    color: var(--ink-2);
    white-space: nowrap;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .phase {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-family: var(--font-ui);
    font-size: 10px;
    line-height: 12px;
    font-weight: 500;
    color: var(--ink-3);
    text-transform: uppercase;
    letter-spacing: 0.08em;
    white-space: nowrap;
  }
  .phase .dot {
    width: 5px;
    height: 5px;
    background: var(--amber);
  }
  .phase.streaming .dot {
    background: var(--moss);
  }
  .metrics {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: 3px 8px;
    min-width: 0;
  }
  .metric {
    display: inline-flex;
    align-items: baseline;
    gap: 4px;
    min-width: 0;
  }
  .metric .k {
    font-family: var(--font-ui);
    font-size: 10px;
    line-height: 12px;
    font-weight: 500;
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: var(--ink-3);
    white-space: nowrap;
  }
  .metric .v {
    font-family: var(--font-mono);
    font-size: 10px;
    font-weight: 400;
    line-height: 1.1;
    color: var(--ink-2);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .metric.ttft .v {
    font-size: 12px;
    color: var(--ink);
  }
  .metric .v.hit-low {
    color: var(--amber);
  }
  .metrics-sep {
    width: 1px;
    align-self: stretch;
    min-height: 12px;
    background: var(--rule);
  }
  .req-bot {
    display: flex;
    align-items: center;
    gap: var(--s2);
  }
  .track {
    position: relative;
    flex: 1 1 auto;
    height: 3px;
    background: var(--panel-3);
    border: 1px solid var(--rule);
    overflow: hidden;
  }
  .fill {
    display: block;
    height: 100%;
    background: var(--amber);
    transition: width 1000ms linear;
  }

  .phase-detail {
    font-size: 10px;
    color: var(--ink-3);
    white-space: nowrap;
  }
  .phase-detail.over {
    color: var(--amber);
  }
  .age {
    margin-left: auto;
    font-size: 10px;
    color: var(--ink-3);
    white-space: nowrap;
  }
  .req .tag.est {
    font-size: 10px;
    font-weight: 500;
    letter-spacing: 0.04em;
    text-transform: none;
  }
  .empty {
    padding: var(--s5) var(--s3);
  }
  .foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s2);
    margin-top: auto;
    padding: 6px var(--s3);
    border-top: 1px solid var(--rule);
  }
  @media (max-width: 1199px) {
    .sessions {
      border: 1px solid var(--rule);
    }
    .list {
      height: 280px;
    }
    .sessions.tall .list {
      height: 420px;
    }
  }
  @media (max-width: 639px) {
    .sessions.tall .list {
      height: 360px;
    }
    .foot {
      flex-direction: column;
      align-items: flex-start;
      gap: 2px;
    }
  }
</style>
