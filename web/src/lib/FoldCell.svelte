<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import Trace from "./Trace.svelte";
  import { fmtMs, fmtRate, fmtBytesRate, fmtBytes, fmtCompact } from "./fmt.js";
  import type { BackendMetrics, Sample } from "./metrics.js";

  // One backend = one fold-cell. Collapsed face: the in-flight trace (the
  // gold live line). Hover: lifts + detail readout row. Click: the fold
  // opens in place (row-span, full-height trace + ttft overlay + full
  // readout + signal switcher). In packet mode (folded / mobile) it
  // renders a thin live line; tapping it expands an inline readout.

  type SignalKey =
    | "inflight"
    | "reqRate"
    | "ttft"
    | "tokEst"
    | "bytesRate"
    | "engRunning"
    | "engPrefill"
    | "engDecode";

  interface Props {
    backend: BackendMetrics;
    hist: Sample[];
    order: number; // diagonal index → cascade delay
    open: boolean;
    onToggle: () => void;
    /** page registers each cell's root so focus can return here on close */
    registerEl: (name: string, el: HTMLElement | null) => void;
  }

  let { backend: b, hist, order, open, onToggle, registerEl }: Props = $props();

  let root: HTMLElement | undefined = $state();
  let totalsOpen = $state(false);

  onMount(() => registerEl(b.name, root ?? null));
  onDestroy(() => registerEl(b.name, null));

  let signal: SignalKey = $state("inflight");
  // history window for the opened trace: 30s glance, 60s default, 5min
  // history. The buffer holds 5 minutes; the collapsed face keeps its own
  // 30s glance window.
  let win: number = $state(60);

  const ts = $derived(hist.map((s) => s.t));

  const showMax = $derived(b.maxConcurrent > 0);

  const sat = $derived(
    (b.maxConcurrent > 0 && b.inFlight >= b.maxConcurrent) ||
      (b.maxQueueDepth > 0 && b.waiting >= b.maxQueueDepth),
  );

  function pick(k: SignalKey): (number | null)[] {
    return hist.map((s) =>
      k === "inflight" ? s.inFlight :
      k === "ttft" ? s.ttftMs :
      k === "tokEst" ? s.tokEstRate :
      k === "engRunning" ? s.engRunning :
      k === "engPrefill" ? s.engPrefill :
      k === "engDecode" ? s.engDecode : s[k],
    );
  }

  const pickTtft = $derived(hist.map((s) => s.ttftMs));

  const routerSignals: [SignalKey, string][] = [
    ["inflight", "in-flight"],
    ["reqRate", "req/s"],
    ["ttft", "ttft"],
    ["tokEst", "tok-est"],
    ["bytesRate", "bytes/s"],
  ];
  const engSignals: [SignalKey, string][] = [
    ["engRunning", "engine run"],
    ["engPrefill", "eng prefill"],
    ["engDecode", "eng decode"],
  ];
  const engOk = $derived(b.engine?.status === "ok");

  // engine v2: composite row texts, empty when the field is null (the row
  // is hidden) — core rows fall back to "—" in the template
  const eng = $derived(b.engine);
  const engWaitSfx = $derived.by(() => {
    const e = eng;
    if (!e) return "";
    if ((e.waitCap ?? 0) > 0 || (e.waitDefer ?? 0) > 0)
      return ` (cap ${e.waitCap ?? 0} / defer ${e.waitDefer ?? 0})`;
    return "";
  });
  const engPoolsSfx = $derived.by(() => {
    const e = eng;
    if (!e) return "";
    const parts = [
      e.fullPct !== null ? `full ${e.fullPct.toFixed(0)}%` : null,
      e.swaPct !== null ? `swa ${e.swaPct.toFixed(0)}%` : null,
      e.mambaPct !== null ? `mamba ${e.mambaPct.toFixed(0)}%` : null,
    ].filter(Boolean);
    return parts.length ? parts.join(" · ") : "";
  });
  const engHitLifeSfx = $derived.by(() => {
    const e = eng;
    if (!e || e.hitHitsTotal === null || e.hitQueriesTotal === null) return "";
    return `${fmtCompact(e.hitHitsTotal)}/${fmtCompact(e.hitQueriesTotal)}`;
  });
  const engTTFTSplitSfx = $derived.by(() => {
    const e = eng;
    if (!e || e.ttftStreamMs === null || e.ttftNonStreamMs === null) return "";
    return `stream ${e.ttftStreamMs.toFixed(0)} / batch ${e.ttftNonStreamMs.toFixed(0)}`;
  });
  const engRatioSfx = $derived.by(() => {
    const e = eng;
    if (
      !e ||
      e.promptTokTotal === null ||
      e.genTokTotal === null ||
      e.promptTokTotal <= 0 ||
      e.genTokTotal <= 0
    )
      return "";
    return (e.genTokTotal / e.promptTokTotal).toFixed(2);
  });
  const signalLabel = $derived(
    signal === "inflight" ? "in-flight" :
    signal === "reqRate" ? "req/s (router)" :
    signal === "ttft" ? "ttft (router, streaming)" :
    signal === "tokEst" ? "tok-est/s (est. prefill)" :
    signal === "bytesRate" ? "bytes/s (measured)" :
    signal === "engRunning" ? "engine running (from /metrics)" :
    signal === "engPrefill" ? "engine prefill tok/s (real)" :
    "engine decode tok/s (real)",
  );

  function onKeydown(e: KeyboardEvent) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onToggle();
    }
    if (e.key === "Escape" && open) onToggle();
  }
</script>

<div
  class="fold-cell {sat ? 'is-saturated' : ''} {open ? 'is-open' : ''}"
  style:--fold-delay="{order * 55}ms"
  style:--fold-order={order}
  role="button"
  tabindex="0"
  aria-expanded={open}
  aria-label="{b.name} — {b.inFlight} in flight"
  bind:this={root}
  onclick={onToggle}
  onkeydown={onKeydown}
>
  {#if !open}
    <!-- collapsed face: the small-multiple -->
    <div class="cell-face">
      <div class="cell-top">
        <span class="crease-id unskew">
          <span class="name">{b.name}</span>
          <span class="tier {b.tier === 0 ? 'king' : ''}">
            {b.tier === 0 ? 'T0·KING' : 'T1'}
          </span>
        </span>
        <span class="cell-val {sat ? 'saturated' : ''} unskew">
          {b.inFlight}{showMax ? `/${b.maxConcurrent}` : ''}
        </span>
      </div>
      <div class="cell-trace">
        <Trace
          samples={hist.map((s) => s.inFlight)}
          ts={ts}
          limit={b.maxConcurrent > 0 ? b.maxConcurrent : null}
          color="gold"
          windowS={30}
        />
      </div>
      <div class="cell-detail unskew">
        <span>inf <b>{b.inFlight}/{b.maxConcurrent > 0 ? b.maxConcurrent : '∞'}</b></span>
        <span>wait <b class:red={b.waiting > 0}>{b.waiting}</b></span>
        <span>prefill <b>{b.prefillInFlight}/{b.prefillMax || '∞'}</b></span>
        <span>kv <b>{b.engine?.status === 'ok' && b.engine.kvPct > 0 ? b.engine.kvPct.toFixed(0) + '%' : '—'}</b></span>
        <span>ewma <b>{fmtMs(b.ewmaMs)}</b></span>
        <span>ttft <b>{b.ttftSampleCount === 0 ? '—' : fmtMs(b.ttftMs)}</b></span>
      </div>
      {#if sat}
        <!-- the red inversion is status, not alarm: say what it means -->
        <div class="sat-note unskew">
          queue full · <span class="st-throttle">429</span> + retry-after — bounded, clears on its own
        </div>
      {/if}
    </div>
  {:else}
    <!-- opened fold: full-height trace + overlay + readout column -->
    <div class="cell-open">
      <div class="open-trace-wrap">
        <div class="switcher-row unskew">
          <span class="sig-head">router</span>
          <div class="switcher" role="group" aria-label="router signal">
            {#each routerSignals as [k, label] (k)}
              <button
                class:on={signal === k}
                aria-pressed={signal === k}
                onclick={(e) => {
                  e.stopPropagation();
                  signal = k;
                }}>
                {label}{k === 'tokEst' ? ' est' : ''}
              </button>
            {/each}
          </div>
          {#if engOk}
            <span class="sig-head">engine · /metrics</span>
            <div class="switcher" role="group" aria-label="engine signal (scraped from the backend's /metrics)">
              {#each engSignals as [k, label] (k)}
                <button
                  class:on={signal === k}
                  aria-pressed={signal === k}
                  onclick={(e) => {
                    e.stopPropagation();
                    signal = k;
                  }}>
                  {label}
                </button>
              {/each}
            </div>
          {/if}
          <button
            class="esc"
            aria-label="fold this cell"
            onclick={(e) => {
              e.stopPropagation();
              onToggle();
            }}>
            fold ✕
          </button>
        </div>
        <div class="open-trace">
          <Trace
            samples={pick(signal)}
            ts={ts}
            limit={signal === 'inflight' && b.maxConcurrent > 0 ? b.maxConcurrent : null}
            color="gold"
            overlay={signal === 'ttft' ? null : pickTtft}
            windowS={win}
          />
        </div>
        <div class="open-legend unskew">
          <span><i class="swatch gold"></i>{signalLabel}</span>
          {#if signal !== 'ttft'}
            <span><i class="swatch valley"></i>ttft overlay</span>
          {/if}
          <span>{b.inFlight} in-flight · {b.waiting} waiting</span>
          <span class="legend-right">
            <span class="win-head">window</span>
            <div class="switcher win" role="group" aria-label="history window">
              {#each [30, 60, 300] as w (w)}
                <button
                  class:on={win === w}
                  aria-pressed={win === w}
                  onclick={(e) => {
                    e.stopPropagation();
                    win = w;
                  }}>
                  {w === 300 ? '5m' : w + 's'}
                </button>
              {/each}
            </div>
          </span>
        </div>
      </div>

      <div class="readout unskew">
        <div class="group-head"><span class="gk">identity</span></div>
        <div class="row"><span class="k">backend</span><span class="v">{b.name}</span></div>
        <div class="row"><span class="k">tier</span><span class="v">{b.tier === 0 ? '0 · king' : '1 · subject'}</span></div>

        <div class="group-head"><span class="gk">live state</span></div>
        <div class="row"><span class="k">in-flight</span>
          <span class="v {sat ? 'red' : ''}">{b.inFlight}{b.maxConcurrent > 0 ? ` / ${b.maxConcurrent}` : ''}</span>
        </div>
        <div class="row"><span class="k">queue</span>
          <span class="v">{b.waiting}{b.maxQueueDepth > 0 ? ` / ${b.maxQueueDepth}` : ''} waiting</span>
        </div>
        <div class="row"><span class="k">prefill</span>
          <span class="v">{b.prefillInFlight}{b.prefillMax > 0 ? ` / ${b.prefillMax}` : ''} inflight{b.prefillWaiting > 0 ? ` · ${b.prefillWaiting} wait` : ''}</span>
        </div>
        <div class="row"><span class="k">ewma dur</span><span class="v">{fmtMs(b.ewmaMs)}</span></div>
        <div class="row"><span class="k">ttft</span>
          <span class="v">{b.ttftSampleCount === 0 ? '— (no streaming samples)' : fmtMs(b.ttftMsNow > 0 ? b.ttftMsNow : b.ttftMs) + ` · ewma ${fmtMs(b.ttftMs)} · ${b.ttftSampleCount} samples`}</span>
        </div>

        <div class="group-head"><span class="gk">rates</span></div>
        <div class="row"><span class="k">req rate</span><span class="v">{fmtRate(b.reqRate)} /s (measured)</span></div>
        <div class="row"><span class="k">bytes rate</span><span class="v">{fmtBytesRate(b.bytesRate)} (measured)</span></div>
        <div class="row"><span class="k">tok rate</span>
          <span class="v">{fmtRate(b.tokEstRate, 0)} <span class="est">est</span></span>
        </div>

        <div class="group-head"><span class="gk">engine (scraped /metrics)</span></div>
        {#if engOk}
          <div class="row"><span class="k">running</span><span class="v">{Math.round(eng.running)}</span></div>
          <div class="row"><span class="k">waiting</span><span class="v">{Math.round(eng.waiting)}{engWaitSfx}</span></div>
          {#if eng.retracted !== null}
            <div class="row"><span class="k">retracted</span><span class="v">{Math.round(eng.retracted)}</span></div>
          {/if}

          {#if eng.reqDoneTotal > 0 || eng.abortedTotal !== null || eng.preemptedTotal !== null}
            <div class="grp">requests · real</div>
          {/if}
          {#if eng.reqDoneTotal > 0}
            <div class="row"><span class="k">completed</span>
              <span class="v">{eng.reqDoneTotal.toLocaleString()} (lifetime)</span>
            </div>
          {/if}
          {#if eng.finReasons}
            <div class="row"><span class="k">fin reasons</span>
              <span class="v">stop {eng.finReasons.stop ?? 0} · length {eng.finReasons.length ?? 0} · abort {eng.finReasons.abort ?? 0} · <span class:err={eng.finReasons.error > 0}>{'error ' + (eng.finReasons.error ?? 0)}</span></span>
            </div>
          {/if}
          {#if eng.abortedTotal !== null}
            <div class="row"><span class="k">aborted</span><span class="v">{eng.abortedTotal}</span></div>
          {/if}
          {#if eng.preemptedTotal !== null}
            <div class="row"><span class="k">preempted</span><span class="v">{eng.preemptedTotal}</span></div>
          {/if}
          {#if eng.streamDoneTotal !== null || eng.nonStreamDoneTotal !== null}
            <div class="row"><span class="k">stream / batch</span>
              <span class="v">{eng.streamDoneTotal !== null ? eng.streamDoneTotal.toLocaleString() : '—'} / {eng.nonStreamDoneTotal !== null ? eng.nonStreamDoneTotal.toLocaleString() : '—'}</span>
            </div>
          {/if}

          <div class="grp">tokens · real</div>
          <div class="row"><span class="k">prefill</span>
            <span class="v">{fmtRate(eng.prefillTokS, 0)} tok/s</span>
          </div>
          <div class="row"><span class="k">prefill cached</span>
            <span class="v">{fmtRate(eng.prefillCacheTokS, 0)} tok/s</span>
          </div>
          <div class="row"><span class="k">decode</span>
            <span class="v">{fmtRate(eng.decodeTokS, 0)} tok/s</span>
          </div>
          {#if eng.promptTokTotal !== null}
            <div class="row"><span class="k">prompt tok</span>
              <span class="v">{fmtCompact(eng.promptTokTotal)} (total)</span>
            </div>
          {/if}
          {#if eng.genTokTotal !== null}
            <div class="row"><span class="k">gen tok</span>
              <span class="v">{fmtCompact(eng.genTokTotal)} (total)</span>
            </div>
          {/if}
          {#if engRatioSfx}
            <div class="row"><span class="k">gen:prompt</span><span class="v">{engRatioSfx}</span></div>
          {/if}

          <div class="grp">cache</div>
          <div class="row"><span class="k">kv cache</span>
            <span class="v">{eng.kvPct > 0 ? eng.kvPct.toFixed(0) + '%' : '—'}{engPoolsSfx ? ` · ${engPoolsSfx}` : ''}</span>
          </div>
          {#if eng.kvUsedTok !== null && eng.kvCapTok !== null}
            <div class="row"><span class="k">pool tok</span>
              <span class="v">{fmtCompact(eng.kvUsedTok)} / {fmtCompact(eng.kvCapTok)}{eng.kvFreeTok !== null ? ` · free ${fmtCompact(eng.kvFreeTok)}` : ''}{eng.kvEvictTok !== null ? ` · evict ${fmtCompact(eng.kvEvictTok)}` : ''}</span>
            </div>
          {/if}
          {#if eng.mambaUsedTok !== null && eng.mambaCapTok !== null}
            <div class="row"><span class="k">mamba tok</span>
              <span class="v">{Math.round(eng.mambaUsedTok)} / {Math.round(eng.mambaCapTok)}</span>
            </div>
          {/if}
          <div class="row"><span class="k">hit rate</span>
            <span class="v">{eng.hitRate > 0 ? (eng.hitRate * 100).toFixed(0) + '%' : '—'}{engHitLifeSfx ? ` · lifetime ${engHitLifeSfx}` : ''}</span>
          </div>
          {#if eng.mmQueriesTotal !== null && eng.mmHitsTotal !== null && eng.mmQueriesTotal > 0}
            <div class="row"><span class="k">mm cache</span>
              <span class="v">{((eng.mmHitsTotal / eng.mmQueriesTotal) * 100).toFixed(0)}%</span>
            </div>
          {/if}
          {#if eng.hicacheHostUsedTok !== null && eng.hicacheHostCapTok !== null}
            <div class="row"><span class="k">hicache host</span>
              <span class="v">{fmtCompact(eng.hicacheHostUsedTok)} / {fmtCompact(eng.hicacheHostCapTok)}</span>
            </div>
          {/if}
          {#if eng.kvMemGB !== null && eng.weightMemGB !== null}
            <div class="row"><span class="k">mem</span>
              <span class="v">{eng.kvMemGB.toFixed(1)} GB kv · {eng.weightMemGB.toFixed(1)} GB weights</span>
            </div>
          {/if}

          <div class="grp">latency · real</div>
          <div class="row"><span class="k">ttft</span>
            <span class="v">{eng.ttftMs > 0 ? fmtMs(eng.ttftMs) : '—'}{engTTFTSplitSfx ? ` · ${engTTFTSplitSfx}` : ''}</span>
          </div>
          <div class="row"><span class="k">itl</span>
            <span class="v">{eng.itlMs > 0 ? fmtMs(eng.itlMs) : '—'}</span>
          </div>
          <div class="row"><span class="k">e2e</span>
            <span class="v">{eng.e2eMs > 0 ? fmtMs(eng.e2eMs) : '—'}</span>
          </div>
          <div class="row"><span class="k">queue</span>
            <span class="v">{eng.queueMs > 0 ? fmtMs(eng.queueMs) : '—'}</span>
          </div>
          {#if eng.prefillMs !== null && eng.decodeMs !== null}
            <div class="row"><span class="k">pre / dec</span>
              <span class="v">{fmtMs(eng.prefillMs)} · {fmtMs(eng.decodeMs)}</span>
            </div>
          {/if}
          {#if eng.perTokMs !== null}
            <div class="row"><span class="k">per-tok</span>
              <span class="v">{eng.perTokMs.toFixed(1)} ms</span>
            </div>
          {/if}
          {#if eng.meanPromptTok > 0 || eng.meanGenTok > 0}
            <div class="row"><span class="k">avg req</span>
              <span class="v">{Math.round(eng.meanPromptTok)} in · {Math.round(eng.meanGenTok)} out tok</span>
            </div>
          {/if}

          {#if eng.sloCap !== null || eng.ctxLen !== null}
            <div class="grp">capacity</div>
          {/if}
          {#if eng.sloCap !== null}
            <div class="row"><span class="k">slo cap</span><span class="v">{Math.round(eng.sloCap)}</span></div>
          {/if}
          {#if eng.ctxLen !== null}
            <div class="row"><span class="k">ctx len</span><span class="v">{Math.round(eng.ctxLen).toLocaleString()}</span></div>
          {/if}
        {:else}
          <div class="row"><span class="k">status</span>
            <span class="v">off — {b.engine?.status === 'err' ? 'endpoint unreachable' : `no /metrics${(b.url || '').toLowerCase().startsWith('http') ? ' (engine flag?)' : ''}`}</span>
          </div>
          <div class="row"><span class="k">source</span><span class="v">{b.url}/metrics</span></div>
        {/if}

        <button
          class="totals-toggle"
          aria-expanded={totalsOpen}
          onclick={(e) => {
            e.stopPropagation();
            totalsOpen = !totalsOpen;
          }}>
          <span>lifetime totals <span class="est-hint">(est where marked)</span></span>
          <span>{totalsOpen ? '−' : '＋'}</span>
        </button>
        <div class="totals-rows {totalsOpen ? '' : 'collapsed'}">
          <div class="row"><span class="k">req total</span><span class="v">{b.reqTotal.toLocaleString()}</span></div>
          <div class="row"><span class="k">bytes in</span><span class="v">{fmtBytes(b.bytesInTotal)}</span></div>
          <div class="row"><span class="k">bytes out</span><span class="v">{fmtBytes(b.bytesOutTotal)}</span></div>
          <div class="row"><span class="k">tok est total</span>
            <span class="v">{b.tokEstTotal >= 1e6 ? (b.tokEstTotal / 1e6).toFixed(1) + ' M' : b.tokEstTotal.toLocaleString()} <span class="est">est</span></span>
          </div>
        </div>
        <div class="open-url">{b.url}</div>
      </div>
    </div>
  {/if}

  <!-- packet row (folded / compact / mobile) — always in the DOM; CSS
       shows it only when the sheet is folded, and expands its inline
       readout when the row itself is open. (Not inside the {#if !open}
       above: removing it from the DOM would make the open state's CSS
       unreachable and the tap a no-op.) -->
  <div class="cell-pkt">
    <span class="pkt-id unskew">{b.name}</span>
    <div class="pkt-trace">
      <Trace
        samples={hist.map((s) => s.inFlight)}
        ts={ts}
        limit={b.maxConcurrent > 0 ? b.maxConcurrent : null}
        color="gold"
        bare
        windowS={30}
      />
    </div>
    <span class="pkt-val {sat ? 'saturated' : ''} unskew">{b.inFlight}</span>
    <div class="pkt-detail unskew" aria-hidden={!open}>
      <span>inf <b>{b.inFlight}/{b.maxConcurrent > 0 ? b.maxConcurrent : '∞'}</b></span>
      <span>wait <b class:red={b.waiting > 0}>{b.waiting}</b></span>
      <span>prefill <b>{b.prefillInFlight}/{b.prefillMax || '∞'}</b></span>
    </div>
  </div>
</div>

<style>
  .est {
    color: var(--gold);
  }
  .cell-pkt .saturated,
  .pkt-val.saturated {
    color: var(--red);
  }
</style>
