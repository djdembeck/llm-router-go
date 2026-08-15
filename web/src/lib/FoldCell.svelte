<script lang="ts">
  import Trace from "./Trace.svelte";
  import { fmtMs, fmtRate, fmtBytesRate, fmtBytes } from "./fmt.js";
  import type { BackendMetrics, Sample } from "./metrics.js";

  // One backend = one fold-cell. Collapsed face: the in-flight trace (the
  // gold live line). Hover: lifts + detail readout row. Click: the fold
  // opens in place (row-span, full-height trace + ttft overlay + full
  // readout + signal switcher). In packet mode it renders a thin live line.

  type SignalKey = "inflight" | "reqRate" | "ttft" | "tokEst" | "bytesRate";

  interface Props {
    backend: BackendMetrics;
    hist: Sample[];
    order: number; // diagonal index → cascade delay
    open: boolean;
    onToggle: () => void;
  }

  let { backend: b, hist, order, open, onToggle }: Props = $props();

  let signal: SignalKey = $state("inflight");

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
      k === "tokEst" ? s.tokEstRate : s[k],
    );
  }

  const pickTtft = $derived(hist.map((s) => s.ttftMs));

  const signals: [SignalKey, string][] = [
    ["inflight", "in-flight"],
    ["reqRate", "req/s"],
    ["ttft", "ttft"],
    ["tokEst", "tok-est"],
    ["bytesRate", "bytes/s"],
  ];

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
        />
      </div>
      <div class="cell-detail unskew">
        <span>inf <b>{b.inFlight}/{b.maxConcurrent > 0 ? b.maxConcurrent : '∞'}</b></span>
        <span>wait <b class:red={b.waiting > 0}>{b.waiting}</b></span>
        <span>prefill <b>{b.prefillInFlight}/{b.prefillMax || '∞'}</b></span>
        <span>ewma <b>{fmtMs(b.ewmaMs)}</b></span>
        <span>ttft <b>{b.ttftSampleCount === 0 ? '—' : fmtMs(b.ttftMs)}</b></span>
      </div>
    </div>

    <!-- packet row (folded / compact / mobile) -->
    <div class="cell-pkt">
      <span class="pkt-id unskew">{b.name}</span>
      <div class="pkt-trace">
        <Trace
          samples={hist.map((s) => s.inFlight)}
          ts={ts}
          limit={b.maxConcurrent > 0 ? b.maxConcurrent : null}
          color="gold"
          bare
        />
      </div>
      <span class="pkt-val {sat ? 'saturated' : ''}">{b.inFlight}</span>
    </div>
  {:else}
    <!-- opened fold: full-height trace + overlay + readout column -->
    <div class="cell-open">
      <div class="open-trace-wrap">
        <div class="switcher unskew">
          {#each signals as [k, label] (k)}
            <button
              class:on={signal === k}
              onclick={(e) => {
                e.stopPropagation();
                signal = k;
              }}>
              {label}{k === 'tokEst' ? ' est' : ''}
            </button>
          {/each}
          <button
            class="esc"
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
          />
        </div>
        <div class="open-legend unskew">
          <span><i class="swatch gold"></i>{signal === 'inflight' ? 'in-flight' : signal === 'reqRate' ? 'req/s' : signal === 'ttft' ? 'ttft (streaming)' : signal === 'tokEst' ? 'tok-est/s (est. prefill)' : 'bytes/s (measured)'}</span>
          {#if signal !== 'ttft'}
            <span><i class="swatch valley"></i>ttft overlay</span>
          {/if}
          <span>{b.inFlight} in-flight · {b.waiting} waiting</span>
        </div>
      </div>

      <div class="readout unskew">
        <div class="row"><span class="k">backend</span><span class="v">{b.name}</span></div>
        <div class="row"><span class="k">tier</span><span class="v">{b.tier === 0 ? '0 · king' : '1 · subject'}</span></div>
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
          <span class="v">{b.ttftSampleCount === 0 ? '— (no streaming samples)' : fmtMs(b.ttftMs) + ` · ${b.ttftSampleCount} samples`}</span>
        </div>
        <div class="row"><span class="k">req rate</span><span class="v">{fmtRate(b.reqRate)} /s (measured)</span></div>
        <div class="row"><span class="k">bytes rate</span><span class="v">{fmtBytesRate(b.bytesRate)} (measured)</span></div>
        <div class="row"><span class="k">tok rate</span>
          <span class="v">{fmtRate(b.tokEstRate, 0)} <span class="est">est</span></span>
        </div>
        <div class="row"><span class="k">req total</span><span class="v">{b.reqTotal.toLocaleString()}</span></div>
        <div class="row"><span class="k">bytes in</span><span class="v">{fmtBytes(b.bytesInTotal)}</span></div>
        <div class="row"><span class="k">bytes out</span><span class="v">{fmtBytes(b.bytesOutTotal)}</span></div>
        <div class="row"><span class="k">tok est total</span>
          <span class="v">{b.tokEstTotal >= 1e6 ? (b.tokEstTotal / 1e6).toFixed(1) + ' M' : b.tokEstTotal.toLocaleString()} <span class="est">est</span></span>
        </div>
        <div class="open-url">{b.url}</div>
      </div>
    </div>
  {/if}
</div>

<style>
  .est {
    color: var(--gold);
  }
  button.esc {
    margin-left: auto;
  }
  .cell-pkt .saturated,
  .pkt-val.saturated {
    color: var(--red);
  }
</style>
