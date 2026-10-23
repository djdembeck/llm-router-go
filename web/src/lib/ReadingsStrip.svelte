<script lang="ts">
  import type { Frame, HistoryResponse, HistoryField, MetricAverages } from "./api";
  import TweenNumber from "./TweenNumber.svelte";
  import type { LayerId } from "./profile";
  import {
    fmtCompact,
    fmtDelta,
    fmtFloat,
    fmtGB,
    fmtInt,
    fmtMs,
    fmtPct,
    nearestIndex,
  } from "./format";

  let {
    frame,
    metricFrame,
    averages,
    history,
    layerId,
    onLayer,
  }: {
    frame: Frame | null;
    metricFrame?: Frame | null;
    averages?: MetricAverages | null;
    history: HistoryResponse | null;
    layerId: LayerId;
    onLayer: (id: LayerId) => void;
  } = $props();

  function prevValue(field: HistoryField): number | null {
    if (!history) return null;
    const t = history.fleet?.t ?? [];
    const arr = history.fleet?.series?.[field] ?? [];
    if (!t.length) return null;
    const target = history.now - 300_000;
    let i = nearestIndex(t, target);
    if (i < 0) return null;
    for (let k = i; k < t.length; k++) {
      if (arr[k] == null) continue;
      // The 5m delta must be resolvable at five minutes: a coarse range
      // (7d at 15m, 30d at 1h) cannot answer it, so render "—" instead of
      // relabelling a 15–60 minute change as 5m.
      if (Math.abs(t[k] - target) > 60_000) return null;
      return arr[k] ?? null;
    }
    return null;
  }

  const liveTotals = $derived(frame?.totals ?? null);
  const m = $derived(metricFrame ?? frame);
  const totals = $derived(m?.totals ?? null);
  const engineOk = $derived((totals?.engineOk ?? 0) > 0);
  // TTFT and engine rates are 15s means refreshed every 5s; the KV gauge is
  // the latest held sample. Both read as current without twitching per scrape.
  const ttftNow = $derived(
    liveTotals && liveTotals.ttftSampleCount > 0
      ? (averages?.ttftMs ?? totals?.ttftMs ?? null)
      : null,
  );
  const prefillNow = $derived(
    engineOk ? (averages?.prefillTokS ?? totals?.prefillTokS ?? null) : null,
  );
  const decodeNow = $derived(
    engineOk ? (averages?.decodeTokS ?? totals?.decodeTokS ?? null) : null,
  );
  const kvNow = $derived(engineOk ? (totals?.kvPeak ?? null) : null);
  const poolUsed = $derived(
    m?.backends.every((b) => b.engine.status !== "ok" || b.engine.kvUsedTok != null)
      ? totals?.kvUsedTok
      : null,
  );
  const devices = $derived(
    m?.gpuFeed.status === "ok" ? (m.gpuDevices ?? []) : [],
  );

  const gpuMax = $derived(
    devices.length ? Math.max(...devices.map((d) => d.utilPct)) : null,
  );
  const gpuAvg = $derived(
    devices.length
      ? devices.reduce((a, d) => a + d.utilPct, 0) / devices.length
      : null,
  );
  const vramUsed = $derived(devices.reduce((a, d) => a + d.memUsedMB, 0));
  const vramTotal = $derived(devices.reduce((a, d) => a + d.memTotalMB, 0));
  const vramPct = $derived(vramTotal > 0 ? (vramUsed / vramTotal) * 100 : null);
  const tempMax = $derived.by(() => {
    const temps = devices
      .map((d) => d.tempC)
      .filter((t): t is number => t != null);
    return temps.length ? Math.max(...temps) : null;
  });

  const runDelta = $derived(fmtDelta(liveTotals?.inFlight, prevValue("inFlight")));
  const waitDelta = $derived(fmtDelta(liveTotals?.waiting, prevValue("waiting")));
  const ttftDelta = $derived(fmtDelta(ttftNow, prevValue("ttftMs"), 0));
  const prefillDelta = $derived(fmtDelta(prefillNow, prevValue("prefillTokS")));
  const decodeDelta = $derived(fmtDelta(decodeNow, prevValue("decodeTokS")));
  const kvDelta = $derived(fmtDelta(kvNow, prevValue("kvPct"), 0));
</script>

{#if frame}
  <div class="readings" role="group" aria-label="fleet readings">
    <div class="cell static">
      <div class="top"><span class="label">RUNNING</span><span class="tag router">router</span></div>
      <div class="val">
        <TweenNumber class="value" value={liveTotals?.inFlight} format={fmtInt} />
        <span class="unit">req</span>
      </div>
      <div class="sub mono">
        {#if (liveTotals?.engineOk ?? 0) > 0}
          engine {fmtInt(liveTotals?.engineRunning)}
        {:else}
          no engine feed
        {/if}
        <span class="delta {runDelta.dir > 0 ? 'neutral' : runDelta.dir < 0 ? 'good' : ''}">
          5m {runDelta.text}
        </span>
      </div>
    </div>

    <div class="cell static">
      <div class="top"><span class="label">WAITING</span><span class="tag router">router</span></div>
      <div class="val">
        <TweenNumber class="value" value={liveTotals?.waiting} format={fmtInt} />
        <span class="unit">queued</span>
      </div>
      <div class="sub mono">
        engine {fmtInt(liveTotals?.engineWaiting)}
        <span class="delta {waitDelta.dir > 0 ? 'bad' : waitDelta.dir < 0 ? 'good' : ''}">
          5m {waitDelta.text}
        </span>
      </div>
    </div>

    <button
      class="cell"
      class:on={layerId === "ttft"}
      aria-pressed={layerId === "ttft"}
      onclick={() => onLayer("ttft")}
      title="First response body byte, streaming requests. Click for the history profile."
    >
      <div class="top">
        <span class="label">TTFT</span>
        <span class="tags"><span class="tag router">router</span><span class="tag window">15s</span></span>
      </div>
      <div class="val">
        <TweenNumber class="value" value={ttftNow} format={fmtMs} duration={1600} />
      </div>
      <div class="sub mono">
        {#if history?.ttft && history.ttft.n > 0}
          p50 {fmtMs(history.ttft.p50)} · p95 {fmtMs(history.ttft.p95)} · n {history.ttft.n}
        {:else}
          no streaming samples yet
        {/if}
        <span class="delta {ttftDelta.dir > 0 ? 'bad' : ttftDelta.dir < 0 ? 'good' : ''}">
          5m {ttftDelta.text}
        </span>
      </div>
    </button>

    <button
      class="cell"
      class:on={layerId === "prefill"}
      aria-pressed={layerId === "prefill"}
      onclick={() => onLayer("prefill")}
      title="Measured compute prefill tokens/s. Cached prefill is shown separately."
    >
      <div class="top">
        <span class="label">PREFILL</span>
        <span class="tags"><span class="tag engine">engine</span><span class="tag window">15s</span></span>
      </div>
      <div class="val">
        <TweenNumber class="value" value={prefillNow} format={fmtInt} duration={1600} />
        <span class="unit">tok/s</span>
      </div>
      <div class="sub mono">
        cache {fmtInt(totals?.prefillCacheTokS)}
        <span class="delta {prefillDelta.dir > 0 ? 'good' : prefillDelta.dir < 0 ? 'bad' : ''}">
          5m {prefillDelta.text}
        </span>
      </div>
    </button>

    <button
      class="cell"
      class:on={layerId === "decode"}
      aria-pressed={layerId === "decode"}
      onclick={() => onLayer("decode")}
      title="Measured generation tokens/s from the engines."
    >
      <div class="top">
        <span class="label">DECODE</span>
        <span class="tags"><span class="tag engine">engine</span><span class="tag window">15s</span></span>
      </div>
      <div class="val">
        <TweenNumber class="value" value={decodeNow} format={fmtInt} duration={1600} />
        <span class="unit">tok/s</span>
      </div>
      <div class="sub mono">
        {(totals?.engineOk ?? 0) > 0 ? `${totals?.engineOk} engines` : "no engine feed"}
        <span class="delta {decodeDelta.dir > 0 ? 'good' : decodeDelta.dir < 0 ? 'bad' : ''}">
          5m {decodeDelta.text}
        </span>
      </div>
    </button>

    <button
      class="cell"
      class:on={layerId === "kv"}
      aria-pressed={layerId === "kv"}
      onclick={() => onLayer("kv")}
      title="Hottest KV cache pool across the engines; 90%+ is hot, 75–89% warm. Click for the history profile."
    >
      <div class="top">
        <span class="label">KV CACHE</span>
        <span class="tags"><span class="tag engine">engine</span><span class="tag window">5s</span></span>
      </div>
      <div class="val">
        <TweenNumber class="value" value={kvNow} format={fmtPct} duration={1600} />
      </div>
      <div class="sub mono">
        {#if (totals?.kvCapTok ?? 0) > 0}
          pool {fmtCompact(poolUsed)}/{fmtCompact(totals?.kvCapTok)}
        {:else}
          {(totals?.engineOk ?? 0) > 0 ? "peak across engines" : "no engine feed"}
        {/if}
        <span class="delta {kvDelta.dir > 0 ? 'bad' : kvDelta.dir < 0 ? 'good' : ''}">
          5m {kvDelta.text}
        </span>
      </div>
    </button>
  </div>

  {#if devices.length}
    <div class="readings devices" role="group" aria-label="device readings">
      <button
        class="cell"
        class:on={layerId === "util"}
        aria-pressed={layerId === "util"}
        onclick={() => onLayer("util")}
        title="Device-wide GPU utilization from the configured exporter."
      >
        <div class="top"><span class="label">GPU UTIL</span><span class="tag device">device</span></div>
        <div class="val"><TweenNumber class="value" value={gpuMax} format={fmtPct} /></div>
        <div class="sub mono">
          {devices.length} dev · avg {fmtPct(gpuAvg)}
        </div>
      </button>
      <button
        class="cell"
        class:on={layerId === "vram"}
        aria-pressed={layerId === "vram"}
        onclick={() => onLayer("vram")}
        title="Device memory used across every reported GPU."
      >
        <div class="top"><span class="label">VRAM</span><span class="tag device">device</span></div>
        <div class="val"><TweenNumber class="value" value={vramPct} format={fmtPct} /></div>
        <div class="sub mono">{fmtGB(vramUsed)}/{fmtGB(vramTotal)} GiB</div>
      </button>
      <div class="cell static">
        <div class="top"><span class="label">TEMP</span><span class="tag device">device</span></div>
        <div class="val">
          <TweenNumber class="value" value={tempMax} format={(v) => fmtFloat(v, 0)} />
          <span class="unit">°C</span>
        </div>
        <div class="sub mono">{tempMax == null ? "no sensor reported" : "hottest device"}</div>
      </div>
    </div>
  {/if}
{/if}

<style>
  .readings {
    display: grid;
    grid-template-columns: repeat(6, minmax(0, 1fr));
    background: var(--panel);
    border-bottom: 1px solid var(--rule);
  }
  .readings.devices {
    grid-template-columns: repeat(3, minmax(0, 1fr));
    border-top: 1px solid var(--rule);
  }
  .cell {
    appearance: none;
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding: 8px var(--s3) 9px;
    background: transparent;
    border: 0;
    border-right: 1px solid var(--rule);
    border-radius: 0;
    color: inherit;
    text-align: left;
    font: inherit;
    cursor: pointer;
    transition:
      background-color var(--dur-fast) var(--ease-out),
      box-shadow var(--dur-fast) var(--ease-out);
  }
  .cell:last-child {
    border-right: 0;
  }
  .cell.static {
    cursor: default;
  }
  .cell:hover {
    background: var(--panel-2);
  }
  .cell.on {
    background: var(--panel-2);
    box-shadow: inset 0 -2px 0 var(--sand);
  }
  .top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s2);
  }
  .tags {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .tag.window {
    text-transform: none;
    letter-spacing: 0.06em;
  }
  .val {
    display: flex;
    align-items: baseline;
    gap: 6px;
    min-width: 0;
  }
  .unit {
    font-family: var(--font-mono);
    font-size: var(--fs-label);
    color: var(--ink-3);
    white-space: nowrap;
  }
  .sub {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s2);
    min-width: 0;
    font-size: var(--fs-label);
    line-height: 12px;
    color: var(--ink-3);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .delta {
    color: var(--ink-3);
  }
  .delta.bad {
    color: var(--verm);
  }
  .delta.good {
    color: var(--moss);
  }
  .delta.neutral {
    color: var(--amber);
  }
  @media (max-width: 1199px) {
    .readings {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
    .cell:nth-child(3n) {
      border-right: 0;
    }
    .cell:nth-child(-n + 3) {
      border-bottom: 1px solid var(--rule);
    }
  }
  @media (max-width: 639px) {
    .readings {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .cell:nth-child(2n) {
      border-right: 0;
    }
    .cell:nth-child(-n + 4) {
      border-bottom: 1px solid var(--rule);
    }
    .cell {
      padding: 8px var(--s2) 9px;
    }
    .readings.devices {
      grid-template-columns: repeat(3, minmax(0, 1fr));
    }
    .readings.devices .cell {
      border-bottom: 0;
    }
    .sub {
      align-items: flex-start;
      flex-direction: column;
      gap: 1px;
      white-space: normal;
    }
  }
</style>
