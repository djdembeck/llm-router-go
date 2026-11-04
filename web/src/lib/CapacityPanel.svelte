<script lang="ts">
  import type { AdmissionMetrics, BackendMetrics, Frame, GpuMetrics } from "./api";
  import { fmtInt, fmtMs, fmtPct } from "./format";

  let { frame, singleBackend }: { frame: Frame | null; singleBackend: string | null } =
    $props();

  const f = $derived(frame);
  const gpu = $derived(f?.gpu ?? null);
  const backends = $derived(
    singleBackend != null
      ? (f?.backends.filter((b) => b.name === singleBackend) ?? [])
      : (f?.backends ?? []),
  );

  // A backend that is session-admission-enabled. The panel is the
  // authoritative "is it at capacity, and WHY" view; backends without
  // admission are router-slot-bound, so they show their slot fill instead.
  function admissionState(b: BackendMetrics): { adm: AdmissionMetrics | null; atCap: boolean } {
    const adm = b.admission;
    if (!adm) return { adm: null, atCap: false };
    const leased = adm.active + adm.idle;
    // The identity cap is the admission gate's ceiling: at cap with a live
    // queue, a new identity is refused until a lease frees — the precise
    // "at capacity" the operator is asking about.
    const atCap = adm.maxAdmitted > 0 && leased >= adm.maxAdmitted;
    return { adm, atCap };
  }

  // Fleet GPU-budget pressure (0–100). 0 budget = no GPU gate configured.
  const gpuPct = $derived(gpu && gpu.budget > 0 ? (gpu.used / gpu.budget) * 100 : null);
  const gpuAtCap = $derived(gpu != null && gpu.budget > 0 && gpu.used >= gpu.budget);
</script>

{#if f && backends.length}
  <section class="cap sheet" aria-label="capacity">
    <div class="sheet-head cap-head">
      <div class="row wrap">
        <span class="label">CAPACITY</span>
        <span class="tag router">router</span>
        <span class="detail muted">admission leases · router slots · GPU budget</span>
      </div>
    </div>

    {#if gpu && gpu.budget > 0}
      <div class="cap-gpu">
        <div class="cap-gpu-row">
          <span class="cap-gpu-label">GPU BUDGET</span>
          <span class="cap-gpu-fig mono">
            {fmtInt(gpu.used)}/{fmtInt(gpu.budget)}
            {#if gpu.waiting > 0}
              <span class="warn">· {gpu.waiting} waiting</span>
            {/if}
          </span>
          <span class={gpuAtCap ? "chip err" : "chip muted"}>
            {gpuAtCap ? "at cap" : "headroom"}
          </span>
        </div>
        <div class="bar" role="img" aria-label="GPU budget {fmtInt(gpu.used)} of {fmtInt(gpu.budget)}">
          <i
            class="fill {gpuAtCap ? 'cap' : ''}"
            style:width="{gpuPct != null ? Math.min(100, gpuPct) : 0}%"
          ></i>
        </div>
      </div>
    {/if}

    {#each backends as b (b.name)}
      {@const { adm, atCap } = admissionState(b)}
      <div class="cap-backend" class:cap={atCap}>
        <div class="cap-bk-head">
          <span class="cap-bk-name mono">{b.name}</span>
          {#if adm}
            <span class="chip {atCap ? 'err' : 'ok'}">{atCap ? "at cap" : "headroom"}</span>
            <span class="cap-bk-fig mono">
              <span class="sand">{fmtInt(adm.active)}</span> active
              <span class="dim">·</span> <span class="dim">{fmtInt(adm.idle)}</span> idle
              <span class="dim">/ {fmtInt(adm.maxAdmitted)} lease</span>
            </span>
          {:else}
            <span class="chip muted">slots</span>
            <span class="cap-bk-fig mono">
              <span class={b.maxConcurrent > 0 && b.inFlight >= b.maxConcurrent ? "warn" : "sand"}>
                {fmtInt(b.inFlight)}
              </span>
              <span class="dim">/ {b.maxConcurrent > 0 ? fmtInt(b.maxConcurrent) : "∞"} slot</span>
              {#if b.waiting > 0}
                <span class="warn">· {fmtInt(b.waiting)} queued</span>
              {/if}
            </span>
          {/if}
        </div>

        {#if adm}
          <div class="cap-bk-row">
            <span class="cap-cell">
              <span class="cap-k">queue</span>
              <span class="cap-v mono">
                {fmtInt(adm.waiting)}<span class="dim">/{adm.maxWaiting > 0 ? fmtInt(adm.maxWaiting) : "∞"}</span>
              </span>
              <span class="cap-sub mono muted">
                oldest {adm.oldestWaitMs > 0 ? fmtMs(adm.oldestWaitMs) : "—"}
              </span>
            </span>
            <span class="cap-cell">
              <span class="cap-k">gate wait</span>
              <span class="cap-v mono">
                {adm.gateWaitCount > 0 ? fmtMs(adm.gateWaitMs) : "—"}
                <span class="dim">avg</span>
              </span>
              <span class="cap-sub mono muted">
                {adm.gateWaitTrailCnt > 0
                  ? `${fmtMs(adm.gateWaitTrailMs / adm.gateWaitTrailCnt)} · 30s`
                  : "no gate samples"}
              </span>
            </span>
            <span class="cap-cell">
              <span class="cap-k">gate rejects</span>
              <span class="cap-v mono">
                {fmtInt(adm.rejQueueFull + adm.rejQueueTimeout + adm.rejClientCancel)}
              </span>
              <span class="cap-sub mono muted">
                full {fmtInt(adm.rejQueueFull)} · timeout {fmtInt(adm.rejQueueTimeout)} ·
                cancel {fmtInt(adm.rejClientCancel)}
              </span>
            </span>
            <span class="cap-cell">
              <span class="cap-k">reclaimed</span>
              <span class="cap-v mono">{fmtInt(adm.reclaimedTotal)}</span>
              <span class="cap-sub mono muted">idle leases under pressure</span>
            </span>
          </div>
        {/if}

        {#if b.rejectReasons}
          {@const reasons = Object.entries(b.rejectReasons).sort((x, y) => y[1] - x[1])}
          {#if reasons.length}
            <div class="cap-reasons mono">
              <span class="cap-k">refusal reasons</span>
              {#each reasons.slice(0, 4) as [reason, n] (reason)}
                <span class="chip warn" title={reason}>{reason} {fmtInt(n)}</span>
              {/each}
            </div>
          {/if}
        {/if}
      </div>
    {/each}
  </section>
{/if}

<style>
  .cap {
    border-top: 1px solid var(--rule);
  }
  .cap-head {
    padding: 7px var(--s3);
  }
  .cap-gpu {
    display: grid;
    gap: 4px;
    padding: 6px var(--s3);
    border-top: 1px solid var(--rule);
  }
  .cap-gpu-row {
    display: flex;
    align-items: center;
    gap: var(--s2);
  }
  .cap-gpu-label {
    font-family: var(--font-mono);
    font-size: var(--fs-label);
    letter-spacing: 0.08em;
    color: var(--ink-3);
  }
  .cap-gpu-fig {
    font-size: var(--fs-detail);
    color: var(--ink-2);
  }
  .cap-gpu-fig .warn {
    color: var(--amber);
  }
  .cap-gpu .chip {
    margin-left: auto;
  }
  .cap-backend {
    padding: 6px var(--s3) 7px;
    border-top: 1px solid var(--rule);
  }
  .cap-backend.cap {
    box-shadow: inset 2px 0 0 var(--verm);
  }
  .cap-bk-head {
    display: flex;
    align-items: center;
    gap: var(--s2);
    flex-wrap: wrap;
  }
  .cap-bk-name {
    font-size: var(--fs-detail);
    font-weight: 500;
    overflow-wrap: anywhere;
  }
  .cap-bk-head .chip {
    margin-left: 0;
  }
  .cap-bk-fig {
    margin-left: auto;
    font-size: var(--fs-detail);
    color: var(--ink-3);
  }
  .cap-bk-fig .sand {
    color: var(--sand);
  }
  .cap-bk-fig .warn {
    color: var(--amber);
  }
  .cap-bk-fig .dim {
    color: var(--ink-3);
  }
  .cap-bk-row {
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: var(--s2);
    margin-top: 6px;
  }
  .cap-cell {
    display: grid;
    gap: 1px;
    min-width: 0;
  }
  .cap-k {
    font-size: var(--fs-label);
    letter-spacing: 0.06em;
    color: var(--ink-3);
    text-transform: uppercase;
  }
  .cap-v {
    font-size: var(--fs-body);
    color: var(--ink);
    white-space: nowrap;
  }
  .cap-v .dim {
    color: var(--ink-3);
  }
  .cap-sub {
    font-size: 9px;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .cap-reasons {
    display: flex;
    align-items: center;
    gap: 4px;
    flex-wrap: wrap;
    margin-top: 6px;
  }
  .cap-reasons .cap-k {
    margin-right: 2px;
  }
  .cap-reasons .chip {
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: 220px;
  }
  .bar {
    position: relative;
    height: 6px;
    background: var(--panel-2);
    border: 1px solid var(--rule);
    overflow: hidden;
  }
  .bar .fill {
    position: absolute;
    inset: 0 auto 0 0;
    background: var(--water);
    transition: width var(--dur) var(--ease-out);
  }
  .bar .fill.cap {
    background: var(--verm);
  }
  @media (max-width: 839px) {
    .cap-bk-row {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
    .cap-bk-fig {
      margin-left: 0;
      width: 100%;
    }
  }
</style>
