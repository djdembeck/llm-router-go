<script lang="ts">
  import { ROUTER_429_REASONS, type BackendMetrics, type Frame, type HistoryField, type HistoryResponse, type QueueStage } from "./api";
  import { fmtClock, fmtMs, fmtPct, nearestIndex } from "./format";
  import { fmtPressureValue } from "./profile";
  import type { FeedKind } from "./live.svelte";

  let { frame, singleBackend, history, cursor, selected, onSelect, paused, stale, feedKind }: {
    frame: Frame | null;
    singleBackend: string | null;
    history: HistoryResponse | null;
    cursor: number | null;
    selected: string | null;
    onSelect: (name: string | null) => void;
    paused: boolean;
    stale: boolean;
    feedKind: FeedKind;
  } = $props();

  const historical = $derived(cursor != null);
  const backends = $derived((frame?.backends ?? []).filter((b) =>
    singleBackend != null ? b.name === singleBackend : selected == null || b.name === selected));
  const stages: QueueStage[] = ["admission", "gpu", "execution", "prefill"];
  const reasonFields: HistoryField[] = [
    "router429BlockedByTier0Rate", "router429SessionQueueFullRate", "router429SessionTimeoutRate",
    "router429GpuBudgetFullRate", "router429BackendFullRate", "router429BackendQueueFullRate",
    "router429PrefillFullRate", "router429PrefillQueueFullRate",
  ];
  const stageFields: Record<QueueStage, [HistoryField, HistoryField]> = {
    admission: ["admissionWaitMsRate", "admissionWaitSamplesRate"],
    gpu: ["gpuWaitMsRate", "gpuWaitSamplesRate"],
    execution: ["executionWaitMsRate", "executionWaitSamplesRate"],
    prefill: ["prefillWaitMsRate", "prefillWaitSamplesRate"],
  };
  function at(name: string | null, field: HistoryField): number | null {
    const series = name == null ? history?.fleet : history?.backends.find((b) => b.name === name);
    if (!series || cursor == null || !series.t.length) return null;
    if (cursor < series.t[0] || cursor > series.t[series.t.length - 1] + (history?.stepMs ?? 0)) return null;
    return series.series[field]?.[nearestIndex(series.t, cursor)] ?? null;
  }
  function value(b: BackendMetrics, field: HistoryField, live: number | null | undefined): number | null {
    return historical ? at(b.name, field) : live ?? null;
  }
  function figure(v: number | null | undefined): string {
    return v == null ? "—" : fmtPressureValue(v);
  }
  function duration(v: number | null | undefined): string {
    return v == null ? "—" : v === 0 ? "0 ms" : fmtMs(v);
  }
  function depth(b: BackendMetrics, stage: QueueStage): string {
    if (stage === "gpu") return "shared (above)";
    if (stage === "admission") return figure(value(b, "admissionWaiting", b.admission?.waiting));
    return stage === "execution" ? figure(value(b, "waiting", b.waiting)) : figure(value(b, "prefillWaiting", b.prefillWaiting));
  }
  function gate(b: BackendMetrics, stage: QueueStage): { mean: number | null; samples: number | null } {
    if (historical) {
      const [sumField, countField] = stageFields[stage];
      const sum = at(b.name, sumField), count = at(b.name, countField);
      return { mean: sum != null && count != null && count > 0 ? sum / count : null, samples: count };
    }
    const summary = b.queueWait?.[stage];
    return { mean: summary && summary.samples > 0 ? summary.sumMs / summary.samples : null, samples: summary?.samples ?? null };
  }
</script>

{#if frame && frame.backends.length}
  <section class="cap sheet" aria-label="capacity diagnostics">
    <div class="sheet-head cap-head">
      <div class="row wrap">
        <span class="label">CAPACITY</span><span class="tag router">router</span>
        <span class="detail muted">{historical ? `at ${fmtClock(cursor!)} · bucket avg · oldest bucket max` : stale ? "stale · last sample" : paused ? "paused · last sample" : feedKind === "stats" ? "stats fallback" : "live"}</span>
      </div>
      {#if singleBackend == null && selected != null}<button class="btn" onclick={() => onSelect(null)}>VIEW ALL BACKENDS</button>{/if}
      <span class="detail mono">shared GPU-budget Q {historical ? figure(at(null, "gpuWaiting")) : frame.gpu.budget > 0 ? figure(frame.gpu.waiting) : "off"}</span>
    </div>
    {#each backends as b (b.name)}
      <div class="cap-backend">
        <div class="cap-row mono">
          {#if singleBackend == null}<span class="backend-name">{b.name}</span>{/if}
          <span><span class="label">LEASES</span> {#if historical}{figure(at(b.name, "admissionActive"))} active + {figure(at(b.name, "admissionIdle"))} idle / {figure(at(b.name, "admissionMaxAdmitted"))}{:else if b.admission}{figure(b.admission.active)} active + {figure(b.admission.idle)} idle / {figure(b.admission.maxAdmitted)}{:else}{b.admission === null ? "off" : "unavailable"}{/if}</span>
          <span><span class="label">ADMISSION Q</span> {#if historical || b.admission}{depth(b, "admission")}/{figure(value(b, "admissionMaxWaiting", b.admission?.maxWaiting))} · oldest {duration(value(b, "admissionOldestWaitMs", b.admission?.oldestWaitMs))}{:else}{b.admission === null ? "off" : "unavailable"}{/if}</span>
          <span><span class="label">EXEC Q</span> {depth(b, "execution")}/{figure(value(b, "maxQueueDepth", b.maxQueueDepth))}</span>
          <span><span class="label">PREFILL Q</span> {depth(b, "prefill")}/{figure(value(b, "prefillMax", b.prefillMax))}</span>
          <span><span class="label">ENGINE Q</span> <span class="tag engine">engine</span> {figure(value(b, "engineWaiting", b.engine.status === "ok" ? b.engine.waiting : null))} · KV {fmtPct(value(b, "kvPct", b.engine.status === "ok" ? b.engine.kvPct : null))}</span>
        </div>
        <details>
          <summary class="detail">Gate waits &amp; router 429 reasons{singleBackend == null ? ` · ${b.name}` : ""}</summary>
          <div class="table-scroll">
            <table class="data-table">
              <caption class="detail muted">Router gate exits · {historical ? "30s smoothed observations · sample-rate req/s" : "trailing 30s"}{!historical && !b.queueWait ? " · unavailable" : ""}</caption>
              <thead><tr><th>STAGE</th><th>QUEUE {historical ? "· BUCKET AVG" : ""}</th><th class="num">MEAN GATE WAIT</th><th class="num">{historical ? "SAMPLE RATE" : "SAMPLES"}</th></tr></thead>
              <tbody>
                {#each stages as stage (stage)}
                  {@const observation = gate(b, stage)}
                  <tr><th scope="row">{stage} <span class="tag router">router</span></th><td class="mono">{depth(b, stage)}</td><td class="num">{duration(observation.mean)}</td><td class="num">{figure(observation.samples)}</td></tr>
                {/each}
                <tr><th scope="row">engine <span class="tag engine">engine</span></th><td class="mono">{figure(value(b, "engineWaiting", b.engine.status === "ok" ? b.engine.waiting : null))}</td><td class="num">{historical ? "live only / —" : duration(b.engineQueue && b.engineQueue.samples > 0 ? b.engineQueue.sumMs / b.engineQueue.samples : null)}</td><td class="mono">engine scrape interval{!historical && b.engineQueue ? ` · ${figure(b.engineQueue.samples)} samples` : ""}</td></tr>
              </tbody>
            </table>
          </div>
          <p class="detail muted">Idle TTL <span class="mono">{duration(b.admission?.idleTTLms)}</span> · lifetime idle reclaims <span class="mono">{historical ? "live only / —" : figure(b.admission?.reclaimedTotal)}</span> · idle reservations yield to waiting work; they do not pin KV cache</p>
          <div class="table-scroll">
            <table class="data-table reasons">
              <caption class="detail muted">Router 429 · {historical ? "30s avg · req/s · lifetime totals live only" : "lifetime totals · trailing-30s req/s"}{!historical && !b.router429 ? " · unavailable" : ""}</caption>
              <thead><tr><th>REASON</th><th class="num">LIFETIME</th><th class="num">30s AVG · REQ/S</th></tr></thead>
              <tbody>
                {#each ROUTER_429_REASONS as reason, i (reason)}
                  {@const row = b.router429?.byReason.find((r) => r.reason === reason)}
                  <tr><th scope="row" class="mono">{reason}</th><td class="num">{historical ? "live only / —" : figure(row?.total)}</td><td class="num">{figure(historical ? at(b.name, reasonFields[i]) : row?.rate)}</td></tr>
                {/each}
              </tbody>
            </table>
          </div>
        </details>
      </div>
    {/each}
  </section>
{/if}

<style>
  .cap { min-width: 0; }
  .cap-head { flex-wrap: wrap; gap: var(--s2); padding: 7px var(--s3); }
  .cap-backend { padding: 6px var(--s3); border-top: 1px solid var(--rule); min-width: 0; }
  .cap-row { display: flex; flex-wrap: wrap; align-items: baseline; column-gap: var(--s4); row-gap: var(--s1); font-size: var(--fs-detail); }
  .cap-row > span { overflow-wrap: anywhere; }
  .backend-name { color: var(--sand); }
  summary { cursor: pointer; padding: var(--s2) 0; color: var(--ink-2); }
  .table-scroll { overflow: auto; max-height: 320px; }
  .data-table { min-width: 540px; }
  caption { text-align: left; padding: var(--s2) 0; }
  p { margin: var(--s2) 0; }
  .reasons { color: var(--water); }
  @media (max-width: 639px) { summary { min-height: 44px; align-content: center; } }
</style>
