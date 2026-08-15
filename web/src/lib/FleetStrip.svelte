<script lang="ts">
  import Trace from "./Trace.svelte";
  import { fmtRate, fmtMs } from "./fmt.js";
  import type { FleetSample, Frame } from "./metrics.js";

  // Fleet strip: three wide cells — aggregate req/s, ttft, tok-est/s — each
  // a live trace. The token one is labeled "est" (estimated new-prefill
  // tokens; never presented as measured decode throughput).

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
      <span class="label">ttft · streaming first byte</span>
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
  </div>
  <div class="fleet-foot">
    token figures are estimated new-prefill tokens (request-body based, ~4
    bytes/token) — not measured decode throughput. requests/s and bytes/s are
    measured.
  </div>
</div>

<style>
  .est {
    color: var(--gold);
  }
</style>
