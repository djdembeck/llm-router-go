<script lang="ts">
  import { onMount } from "svelte";
  import type { BurstRequest } from "./api";
  import { csvEscape, fmtClock, fmtInt, fmtMs } from "./format";

  let {
    requests,
    spikeCount,
    spikePeak,
    open = $bindable(false),
    singleBackend,
  }: {
    requests: BurstRequest[];
    spikeCount: number;
    spikePeak: number;
    open?: boolean;
    singleBackend: string | null;
  } = $props();

  let onlyErrors = $state(false);
  const errors = $derived(requests.filter((r) => r.status >= 429).length);
  const shown = $derived.by(() => {
    const filtered = onlyErrors ? requests.filter((r) => r.status >= 429) : requests;
    return filtered
      .slice()
      .sort((a, b) => b.t - a.t)
      .slice(0, 80);
  });

  onMount(() => {
    try {
      open = localStorage.getItem("lrg-request-log") === "1";
    } catch {
      /* private mode: start collapsed */
    }
  });

  function toggle(): void {
    open = !open;
    try {
      localStorage.setItem("lrg-request-log", open ? "1" : "0");
    } catch {
      /* private mode: the choice lasts this session */
    }
  }

  function statusClass(status: number): string {
    if (status >= 500) return "err";
    if (status === 429) return "water";
    if (status >= 400) return "warn";
    return "ok";
  }

  function exportCsv(): void {
    const head = [
      "completed_ms",
      "start_ms",
      "backend",
      "path",
      "status",
      "stream",
      "ttft_ms",
      "dur_ms",
      "bytes_in",
      "bytes_out",
      "new_tokens_est",
    ];
    const lines = [head.join(",")];
    for (const r of requests) {
      lines.push(
        [
          r.t,
          r.start,
          csvEscape(r.backend),
          csvEscape(r.path),
          r.status,
          r.stream,
          r.ttftMs.toFixed(1),
          r.durMs.toFixed(1),
          r.bytesIn,
          r.bytesOut,
          r.newTokensEst,
        ].join(","),
      );
    }
    const blob = new Blob([lines.join("\n")], { type: "text/csv" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `llm-router-requests-${new Date().toISOString().slice(0, 19)}.csv`;
    a.click();
    URL.revokeObjectURL(a.href);
  }
</script>

<section class="feed sheet" aria-label="request feed">
  <div class="sheet-head feed-head">
    <div class="row wrap">
      <span class="label">REQUESTS</span>
      <span class="chip {spikeCount ? 'warn' : 'muted'}">
        {spikeCount}/5s · peak {spikePeak}
      </span>
      {#if errors > 0}
        <span class="chip water">{errors} 429+</span>
      {/if}
    </div>
    <div class="row">
      {#if open}
        <label class="check">
          <input type="checkbox" bind:checked={onlyErrors} />
          429+
        </label>
        <button class="btn" onclick={exportCsv} disabled={requests.length === 0}>
          CSV
        </button>
      {/if}
      <button
        class="btn"
        aria-expanded={open}
        aria-controls="request-log"
        onclick={toggle}
      >
        {open ? "HIDE" : "SHOW"}
      </button>
    </div>
  </div>

  {#if open}
    <div class="scroll" id="request-log">
      <table class="data-table" class:single={singleBackend != null}>
        <thead>
          <tr>
            <th class="col-time">TIME</th>
            {#if singleBackend == null}
              <th class="col-backend">BACKEND</th>
            {/if}
            <th class="col-path">PATH</th>
            <th class="col-status num">STATUS</th>
            <th class="col-ttft num">TTFT</th>
            <th class="col-dur num">DUR</th>
            <th class="col-tok num">TOK <span class="tag est">est</span></th>
          </tr>
        </thead>
        <tbody>
          {#each shown as r (r.t + ":" + r.start + ":" + r.backend + ":" + r.path + ":" + r.status)}
            <tr>
              <td class="col-time mono">{fmtClock(r.t)}</td>
              {#if singleBackend == null}
                <td class="col-backend mono">{r.backend}</td>
              {/if}
              <td class="col-path path mono" title={r.path}>{r.path}</td>
              <td class="col-status num {statusClass(r.status)}">{r.status}</td>
              <td class="col-ttft num">{r.ttftMs > 0 ? fmtMs(r.ttftMs) : "—"}</td>
              <td class="col-dur num">{fmtMs(r.durMs)}</td>
              <td class="col-tok num">{fmtInt(r.newTokensEst)}</td>
            </tr>
          {:else}
            <tr class="empty">
              <td colspan={singleBackend == null ? 7 : 6}>
                {onlyErrors ? "no 429+ rejections in the recent ring" : "the tape is quiet"}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>

    <div class="foot detail muted">
      <span>router-measured status, TTFT, duration, bytes</span>
      <span>token figures est (~4 bytes/token)</span>
    </div>
  {/if}
</section>

<style>
  .feed {
    display: flex;
    flex-direction: column;
    min-width: 0;
    /* Clip stale rows; the table updates in place, never over the band below. */
    overflow: hidden;
    contain: paint;
  }
  .feed-head {
    flex-wrap: wrap;
  }
  .scroll {
    max-height: 316px;
    overflow: auto;
  }
  .feed .data-table {
    table-layout: fixed;
  }
  .feed .data-table .col-time { width: 12%; }
  .feed .data-table .col-backend { width: 13%; }
  .feed .data-table .col-path { width: 30%; }
  .feed .data-table .col-status { width: 8%; }
  .feed .data-table .col-ttft { width: 10%; }
  .feed .data-table .col-dur { width: 10%; }
  .feed .data-table .col-tok { width: 9%; }
  /* The backend column is omitted in single-server mode; PATH absorbs its share. */
  .feed .data-table.single .col-path { width: 43%; }
  .path {
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    color: var(--ink-3);
  }
  .data-table td.ok {
    color: var(--moss);
  }
  .data-table td.water {
    color: var(--water);
  }
  .data-table td.warn {
    color: var(--amber);
  }
  .data-table td.err {
    color: var(--verm);
  }
  .foot {
    display: flex;
    gap: var(--s3);
    flex-wrap: wrap;
    padding: 5px var(--s3);
    border-top: 1px solid var(--rule);
    font-size: 10px;
  }
  @media (max-width: 639px) {
    .col-path {
      display: none;
    }
    .foot {
      display: none;
    }
  }
</style>
