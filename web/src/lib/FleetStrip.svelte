<script lang="ts">
  import Trace from "./Trace.svelte";
  import { fmtRate, fmtMs } from "./fmt.js";
  import type { FleetSample, Frame } from "./metrics.js";

  // Fleet strip: four wide aggregates — req/s (measured), ttft (router
  // first byte), tok-est/s (estimated prefill, gold, est-labeled), and real
  // decode tok/s (scraped from the engines' own /metrics, valley, "real"
  // — never presented as estimated). The est/real pairing is the honest
  // data boundary made visible: the operator sees both the router's body-
  // based estimate and the engine's actual token throughput side by side.

  interface Props {
    frame: Frame | null;
    hist: FleetSample[];
  }

  let { frame, hist }: Props = $props();

  const ts = $derived(hist.map((s) => s.t));

  const reqRate = $derived(frame?.totals?.reqRate ?? null);
  const tokRate = $derived(frame?.totals?.tokEstRate ?? null);
  const ttftNow = $derived(
    frame
      ? (() => {
          const vals = frame.backends.filter((b) => b.ttftSampleCount > 0);
          if (!vals.length) return null;
          return vals.reduce((a, b) => a + b.ttftMs, 0) / vals.length;
        })()
      : null,
  );
  // REAL decode throughput: sum across backends whose engine feed is live.
  const decodeNow = $derived(
    frame
      ? (() => {
          const eng = frame.backends.filter((b) => b.engine?.status === "ok");
          if (!eng.length) return null;
          return eng.reduce((a, b) => a + (b.engine?.decodeTokS ?? 0), 0);
        })()
      : null,
  );
  const prefillNow = $derived(
    frame
      ? (() => {
          const eng = frame.backends.filter((b) => b.engine?.status === "ok");
          if (!eng.length) return null;
          return eng.reduce((a, b) => a + (b.engine?.prefillTokS ?? 0), 0);
        })()
      : null,
  );
  // REAL prefix-cache hit rate: mean across backends whose engine feed is
  // live — the engines' own gauges, not the router's estimate.
  const hitNow = $derived(
    frame
      ? (() => {
          const eng = frame.backends
            .map((b) => (b.engine?.status === "ok" ? (b.engine?.hitRate ?? 0) * 100 : null))
            .filter((v): v is number => v !== null && v > 0);
          if (!eng.length) return null;
          return eng.reduce((a, v) => a + v, 0) / eng.length;
        })()
      : null,
  );
  // Hottest KV pool across backends: where the next retraction/eviction
  // lands first.
  const kvNow = $derived(
    frame
      ? (() => {
          const vals = frame.backends
            .map((b) => (b.engine?.status === "ok" ? b.engine?.kvPct ?? 0 : 0))
            .filter((v) => v > 0);
          if (!vals.length) return null;
          return Math.max(...vals);
        })()
      : null,
  );
</script>

<div>
  <div class="fleet-strip" role="group" aria-label="fleet aggregates">
    <div class="fleet-cell unskew">
      <span class="label">req/s · measured</span>
      <span class="value">{fmtRate(reqRate)}</span>
      <div class="trace-wrap">
        <Trace samples={hist.map((s) => s.reqRate)} ts={ts} color="gold" />
      </div>
    </div>
    <div class="fleet-cell unskew">
      <span class="label">ttft · first byte</span>
      <span class="value">{fmtMs(ttftNow)}</span>
      <div class="trace-wrap">
        <Trace
          samples={hist.map((s) => s.ttftMs)}
          ts={ts}
          color="valley"
        />
      </div>
    </div>
    <div class="fleet-cell unskew">
      <span class="label">tok/s · new-prefill <span class="est">est</span></span>
      <span class="value">{fmtRate(tokRate, 0)}</span>
      <div class="trace-wrap">
        <Trace samples={hist.map((s) => s.tokEstRate)} ts={ts} color="gold" />
      </div>
    </div>
    <div class="fleet-cell unskew">
      <span class="label">decode tok/s · <span class="real">real</span></span>
      <span class="value">{decodeNow === null ? '—' : fmtRate(decodeNow, 0)}</span>
      <div class="trace-wrap">
        <Trace samples={hist.map((s) => s.engDecode)} ts={ts} color="valley" />
      </div>
    </div>
    <div class="fleet-cell unskew">
      <span class="label">cache hit · <span class="real">real</span></span>
      <span class="value">{hitNow === null ? '—' : Math.round(hitNow) + '%'}</span>
      <div class="trace-wrap">
        <Trace
          samples={hist.map((s) => s.hitRate)}
          ts={ts}
          color="valley"
          yMax={100}
        />
      </div>
    </div>
    <div class="fleet-cell unskew">
      <span class="label">kv peak · <span class="real">real</span></span>
      <span class="value" class:hot={kvNow !== null && kvNow >= 90}>
        {kvNow === null ? '—' : Math.round(kvNow) + '%'}
      </span>
      <div class="trace-wrap">
        <Trace
          samples={hist.map((s) => s.kvPeak)}
          ts={ts}
          color="valley"
          yMax={100}
        />
      </div>
    </div>
  </div>
  <div class="fleet-foot">
    {#if prefillNow !== null}
      <span class="fleet-foot-real">
        real (scraped from engine /metrics): prefill {fmtRate(prefillNow, 0)} tok/s · decode {fmtRate(decodeNow, 0)} tok/s
      </span>
      <span class="fleet-foot-sep">·</span>
    {/if}
    token figures are estimated new-prefill tokens (request-body based, ~4
    bytes/token) — <b>real</b> tok/s is the engine's own measured throughput.
    requests/s and bytes/s are measured.
  </div>
</div>

<style>
  .est {
    color: var(--gold);
  }
  .real {
    color: var(--vl-lit);
  }
</style>
