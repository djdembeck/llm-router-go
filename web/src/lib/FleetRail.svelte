<script lang="ts">
  import type { Frame, HistoryResponse } from "./api";
  import type { FeedInfo } from "./live.svelte";
  import TweenNumber from "./TweenNumber.svelte";
  import { DASH, fmtClock, fmtGB, fmtInt, fmtPct, fmtSince } from "./format";

  let {
    frame,
    metricFrame,
    history,
    selected,
    onSelect,
    feed,
    lastFrameAt,
    now,
  }: {
    frame: Frame | null;
    metricFrame?: Frame | null;
    history: HistoryResponse | null;
    selected: string | null;
    onSelect: (name: string | null) => void;
    feed: FeedInfo | null;
    lastFrameAt: number | null;
    now: number;
  } = $props();

  const m = $derived(metricFrame ?? frame);
  const backends = $derived(m?.backends ?? []);
  const devices = $derived(
    m?.gpuFeed.status === "ok" ? (m.gpuDevices ?? []) : [],
  );
  const engineOk = $derived(frame?.totals.engineOk ?? 0);

  function kvClass(pct: number): string {
    if (pct >= 90) return "hot";
    if (pct >= 75) return "warm";
    return "";
  }
</script>

<aside class="rail" aria-label="fleet">
  <div class="rail-head">
    <span class="label">BACKENDS</span>
    <span class="detail mono muted">{backends.length}</span>
  </div>

  <div class="fleet-line mono">
    <span class="label">IN FLIGHT</span>
    <span class="num-value">{fmtInt(frame?.totals.inFlight)}</span>
    <span class="label">WAITING</span>
    <span class="num-value">{fmtInt(frame?.totals.waiting)}</span>
    <span class="label">ENGINES</span>
    <span class="num-value">{engineOk}</span>
  </div>

  <div class="rows">
    {#each backends as b (b.name)}
      {@const ok = b.engine.status === "ok"}
      {@const kv = ok ? b.engine.kvPct : null}
      <button
        class="backend"
        class:selected={selected === b.name}
        aria-pressed={selected === b.name}
        onclick={() => onSelect(selected === b.name ? null : b.name)}
        title={ok ? "engine /metrics ok" : `engine /metrics ${b.engine.status}${b.engine.lastError ? `: ${b.engine.lastError}` : ""}`}
      >
        <span class="line1">
          <span class="dot {ok ? 'ok' : b.engine.status === 'off' ? 'muted' : 'err'}"></span>
          <span class="name">{b.name}</span>
          <span class="tier mono">T{b.tier}</span>
          <span class="slots mono">{b.inFlight}/{b.maxConcurrent || "∞"}</span>
        </span>
        <span class="line2 mono">
          <span class="bar" aria-hidden="true">
            <span
              class="fill {kvClass(kv ?? 0)}"
              style:width={`${Math.min(100, kv ?? 0)}%`}
            ></span>
          </span>
          <span class="kv"><TweenNumber value={kv} format={fmtPct} duration={1600} /></span>
          <span class="queue">q {b.waiting}</span>
        </span>
      </button>
    {:else}
      <div class="empty detail muted">no backends configured</div>
    {/each}
  </div>

  {#if devices.length}
    <div class="rail-head devices-head">
      <span class="label">DEVICES</span>
      <span class="tag device">device</span>
    </div>
    <div class="devices">
      {#each devices as d (d.key)}
        <div class="device">
          <span class="line1 mono">
            <span class="name">{d.name || `GPU ${d.index}`}</span>
            <span class="slots"><TweenNumber value={d.utilPct} format={fmtInt} />%</span>
          </span>
          <span class="line2 mono">
            <span class="bar" aria-hidden="true">
              <span class="fill" style:width={`${Math.min(100, d.utilPct)}%`}></span>
            </span>
            <span class="kv">{fmtGB(d.memUsedMB)}/{fmtGB(d.memTotalMB)}G</span>
          </span>
        </div>
      {/each}
    </div>
  {/if}

  <div class="rail-foot">
    <div class="foot-row">
      <span class="label">FEED</span>
      <span class="mono">{feed ? feed.url : DASH}</span>
    </div>
    <div class="foot-row">
      <span class="label">LAST</span>
      <span class="mono">
        {#if lastFrameAt}
          {fmtClock(lastFrameAt)} · {fmtSince(now - lastFrameAt)} ago
        {:else}
          {DASH}
        {/if}
      </span>
    </div>
    {#if frame?.gpuFeed.status === "err"}
      <div class="foot-row">
        <span class="label">DEVICE</span>
        <span class="mono err" title={frame.gpuFeed.lastError ?? ""}>exporter err</span>
      </div>
    {/if}
    <div class="foot-row">
      <span class="label">HISTORY</span>
      <span class="mono">
        {#if history}
          {history.range} · {history.backends.length} series
        {:else}
          waiting
        {/if}
      </span>
    </div>
  </div>
</aside>

<style>
  .rail {
    display: flex;
    flex-direction: column;
    min-width: 0;
    border-right: 1px solid var(--rule);
    background: var(--panel);
    transition:
      background-color var(--dur) var(--ease-soft),
      border-color var(--dur) var(--ease-soft);
  }
  .rail-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s2);
    min-height: 32px;
    padding: 0 var(--s3);
    border-bottom: 1px solid var(--rule);
  }
  .fleet-line {
    display: grid;
    grid-template-columns: auto 1fr auto 1fr auto auto;
    align-items: baseline;
    gap: var(--s2);
    padding: 8px var(--s3);
    border-bottom: 1px solid var(--rule);
  }
  .fleet-line .label {
    font-size: 9px;
    letter-spacing: 0.08em;
  }
  .num-value {
    font-size: var(--fs-body);
    color: var(--ink);
    text-align: right;
  }
  .rows {
    display: flex;
    flex-direction: column;
  }
  .backend {
    appearance: none;
    display: flex;
    flex-direction: column;
    gap: 5px;
    width: 100%;
    padding: 7px var(--s3);
    background: transparent;
    border: 0;
    border-bottom: 1px solid var(--rule);
    border-radius: 0;
    color: inherit;
    font: inherit;
    text-align: left;
    cursor: pointer;
    transition: background-color var(--dur-fast) var(--ease-out);
  }
  .backend:hover {
    background: var(--panel-2);
  }
  .backend.selected {
    background: var(--panel-3);
    box-shadow: inset 0 0 0 1px var(--sand);
  }
  .line1,
  .line2 {
    display: flex;
    align-items: center;
    gap: 6px;
    min-width: 0;
  }
  .dot {
    width: 5px;
    height: 5px;
    flex: none;
    background: var(--ink-3);
  }
  .dot.ok {
    background: var(--moss);
  }
  .dot.muted {
    background: var(--ink-3);
  }
  .dot.err {
    background: var(--verm);
  }
  .name {
    font-size: var(--fs-detail);
    font-weight: 600;
    color: var(--ink);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .tier {
    font-size: 9px;
    color: var(--sand);
    border: 1px solid var(--rule-2);
    padding: 0 3px;
    line-height: 12px;
  }
  .slots {
    margin-left: auto;
    font-size: var(--fs-label);
    color: var(--ink-2);
  }
  .line2 {
    font-size: var(--fs-label);
    color: var(--ink-3);
  }
  .bar {
    position: relative;
    flex: 1 1 auto;
    height: 5px;
    background: var(--panel-3);
    border: 1px solid var(--rule);
    overflow: hidden;
  }
  .fill {
    display: block;
    height: 100%;
    background: var(--water);
    transition: width 420ms var(--ease-out);
  }
  .fill.warm {
    background: var(--amber);
  }
  .fill.hot {
    background: var(--verm);
  }
  .kv {
    min-width: 34px;
    text-align: right;
    color: var(--ink-2);
  }
  .queue {
    min-width: 28px;
    text-align: right;
  }
  .devices-head {
    border-top: 1px solid var(--rule);
  }
  .devices {
    display: flex;
    flex-direction: column;
  }
  .device {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 6px var(--s3);
    border-bottom: 1px solid var(--rule);
  }
  .rail-foot {
    display: flex;
    flex-direction: column;
    gap: 3px;
    padding: 8px var(--s3);
    border-top: 1px solid var(--rule);
  }
  .foot-row {
    display: flex;
    justify-content: space-between;
    gap: var(--s2);
    font-size: 9px;
    line-height: 14px;
    color: var(--ink-3);
    white-space: nowrap;
  }
  .foot-row .label {
    font-size: 9px;
    letter-spacing: 0.08em;
  }
  .empty {
    padding: var(--s3);
  }
  @media (max-width: 959px) {
    .rail {
      border-right: 0;
      border-bottom: 1px solid var(--rule);
    }
    .rows {
      flex-direction: row;
      overflow-x: auto;
    }
    .backend {
      min-width: 210px;
      border-right: 1px solid var(--rule);
      border-bottom: 0;
    }
    .rail-foot {
      margin-top: 0;
    }
  }
  @media (max-width: 639px) {
    .rows {
      flex-direction: column;
      overflow-x: visible;
    }
    .backend {
      min-width: 0;
      border-right: 0;
      border-bottom: 1px solid var(--rule);
    }
    .fleet-line {
      grid-template-columns: auto 1fr auto 1fr auto auto;
      gap: 4px;
    }
  }
</style>
