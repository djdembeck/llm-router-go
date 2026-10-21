<script lang="ts">
  import type { Frame, HistoryResponse, HistorySeries } from "./api";
  import { DASH, fmtClock, fmtCompact, fmtInt, fmtMs, fmtPct, fmtRate } from "./format";
  import { nearestIndex } from "./format";

  let {
    frame,
    history,
    cursor,
    selected,
    onSelect,
    stale,
    lastFrameAt,
  }: {
    frame: Frame | null;
    history: HistoryResponse | null;
    cursor: number | null;
    selected: string | null;
    onSelect: (name: string | null) => void;
    stale: boolean;
    lastFrameAt: number | null;
  } = $props();

  type SortKey =
    | "name"
    | "slots"
    | "engine"
    | "kv"
    | "pool"
    | "mem"
    | "ttft"
    | "prefill"
    | "decode"
    | "hit";
  let sortKey = $state<SortKey>("name");
  let dir = $state<1 | -1>(1);

  interface Row {
    name: string;
    tier: number;
    slots: string;
    slotsV: number | null;
    engine: string;
    engineV: number | null;
    kv: number | null;
    pool: string;
    poolV: number | null;
    mem: string;
    memV: number | null;
    ttft: number | null;
    prefill: number | null;
    decode: number | null;
    hit: number | null;
    ok: boolean;
    lastError: string;
  }

  function histRow(name: string, hs: HistorySeries, i: number): Row {
    const s = hs.series;
    const at = (k: keyof typeof s): number | null => {
      const arr = s[k];
      return arr && arr[i] != null ? (arr[i] as number) : null;
    };
    const inFlight = at("inFlight");
    const max = at("maxConcurrent");
    const er = at("engineRunning");
    const ew = at("engineWaiting");
    const kv = at("kvPct");
    const used = at("kvUsedTok");
    const cap = at("kvCapTok");
    const kvGB = at("kvMemGB");
    const wGB = at("weightMemGB");
    const tier = frame?.backends.find((b) => b.name === name)?.tier ?? 0;
    return {
      name,
      tier,
      slots: `${fmtInt(inFlight)}/${max ? fmtInt(max) : "∞"}`,
      slotsV: inFlight,
      engine: er == null ? DASH : `${fmtInt(er)}/${fmtInt(ew)}`,
      engineV: er,
      kv,
      pool: used != null && cap != null ? `${fmtCompact(used)}/${fmtCompact(cap)}` : DASH,
      poolV: used,
      mem: kvGB != null || wGB != null ? `${fmtRate((kvGB ?? 0) + (wGB ?? 0))}` : DASH,
      memV: kvGB != null || wGB != null ? (kvGB ?? 0) + (wGB ?? 0) : null,
      ttft: at("ttftMs"),
      prefill: at("prefillTokS"),
      decode: at("decodeTokS"),
      hit: at("hitRate") != null ? (at("hitRate") as number) * 100 : null,
      ok: er != null,
      lastError: "",
    };
  }

  function frameRow(b: NonNullable<Frame["backends"]>[number]): Row {
    const ok = b.engine.status === "ok";
    const mem =
      ok && (b.engine.kvMemGB != null || b.engine.weightMemGB != null)
        ? (b.engine.kvMemGB ?? 0) + (b.engine.weightMemGB ?? 0)
        : null;
    return {
      name: b.name,
      tier: b.tier,
      slots: `${fmtInt(b.inFlight)}/${b.maxConcurrent || "∞"}`,
      slotsV: b.inFlight,
      engine: ok ? `${fmtInt(b.engine.running)}/${fmtInt(b.engine.waiting)}` : DASH,
      engineV: ok ? b.engine.running : null,
      kv: ok ? b.engine.kvPct : null,
      pool:
        ok && b.engine.kvUsedTok != null && b.engine.kvCapTok != null
          ? `${fmtCompact(b.engine.kvUsedTok)}/${fmtCompact(b.engine.kvCapTok)}`
          : DASH,
      poolV: ok ? (b.engine.kvUsedTok ?? null) : null,
      mem: mem != null ? fmtRate(mem) : DASH,
      memV: mem,
      ttft: b.ttftSampleCount > 0 ? b.ttftMs : null,
      prefill: ok ? b.engine.prefillTokS : null,
      decode: ok ? b.engine.decodeTokS : null,
      hit: ok ? b.engine.hitRate * 100 : null,
      ok,
      lastError: b.engine.lastError ?? "",
    };
  }

  const atCursor = $derived(cursor != null && history != null);
  const cursorLabel = $derived(cursor != null ? fmtClock(cursor) : "");

  const rows = $derived.by(() => {
    const out: Row[] = [];
    if (cursor != null && history) {
      for (const hs of history.backends) {
        const i = nearestIndex(hs.t, cursor);
        if (i < 0) continue;
        out.push(histRow(hs.name, hs, i));
      }
    } else if (frame) {
      for (const b of frame.backends) out.push(frameRow(b));
    }
    const keyMap: Record<SortKey, keyof Row> = {
      name: "name",
      slots: "slotsV",
      engine: "engineV",
      kv: "kv",
      pool: "poolV",
      mem: "memV",
      ttft: "ttft",
      prefill: "prefill",
      decode: "decode",
      hit: "hit",
    };
    out.sort((a, b) => {
      const k = keyMap[sortKey];
      const av = a[k] as number | string | null;
      const bv = b[k] as number | string | null;
      if (typeof av === "string" || typeof bv === "string") {
        return String(av).localeCompare(String(bv)) * dir;
      }
      const an = av ?? -Infinity;
      const bn = bv ?? -Infinity;
      return (an < bn ? -1 : an > bn ? 1 : 0) * dir;
    });
    return out;
  });

  function sortBy(k: SortKey): void {
    if (sortKey === k) dir = dir === 1 ? -1 : 1;
    else {
      sortKey = k;
      dir = 1;
    }
  }
</script>

<section class="backend-table sheet" aria-label="backend detail">
  <div class="sheet-head">
    <div class="row wrap">
      <span class="label">BACKEND DETAIL</span>
      <span class="tag router">router</span>
      <span class="tag engine">engine</span>
      <span class="detail muted">
        {atCursor
          ? `at ${cursorLabel}`
          : stale && lastFrameAt
            ? `last data ${fmtClock(lastFrameAt)}`
            : "current"}
      </span>
    </div>
    <span class="detail muted hint">click a row to focus its lane</span>
  </div>

  <div class="scroll">
    <table class="data-table">
      <thead>
        <tr>
          <th class="first" aria-sort={sortKey === "name" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("name")}>BACKEND</button>
          </th>
          <th class="num" aria-sort={sortKey === "slots" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("slots")}><i class="hdot router"></i>SLOTS</button>
          </th>
          <th class="num" aria-sort={sortKey === "engine" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("engine")}><i class="hdot engine"></i>RUN/WAIT</button>
          </th>
          <th class="num" aria-sort={sortKey === "kv" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("kv")}><i class="hdot engine"></i>KV</button>
          </th>
          <th class="num" aria-sort={sortKey === "pool" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("pool")}>KV POOL</button>
          </th>
          <th class="num" aria-sort={sortKey === "mem" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("mem")}>ENG MEM</button>
          </th>
          <th class="num" aria-sort={sortKey === "ttft" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("ttft")}><i class="hdot router"></i>TTFT</button>
          </th>
          <th class="num" aria-sort={sortKey === "prefill" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("prefill")}>PREFILL</button>
          </th>
          <th class="num" aria-sort={sortKey === "decode" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("decode")}>DECODE</button>
          </th>
          <th class="num" aria-sort={sortKey === "hit" ? (dir === 1 ? "ascending" : "descending") : "none"}>
            <button class="sort" onclick={() => sortBy("hit")}>HIT</button>
          </th>
        </tr>
      </thead>
      <tbody>
        {#each rows as r (r.name)}
          <tr
            class:selected={selected === r.name}
            tabindex="0"
            onclick={() => onSelect(selected === r.name ? null : r.name)}
            onkeydown={(e) => {
              if (e.key !== "Enter" && e.key !== " ") return;
              e.preventDefault();
              onSelect(selected === r.name ? null : r.name);
            }}
          >
            <td class="first">
              <span class="name mono">{r.name}</span>
              <span
                class="tier mono"
                title={`Tier ${r.tier} — ${r.tier === 0 ? "King" : "Subject"}`}
              >{r.tier === 0 ? "KING" : "SUBJECT"}</span>
              {#if !r.ok}<span class="engine-off" title={r.lastError || "engine /metrics unavailable"}>off</span>{/if}
            </td>
            <td class="num">{r.slots}</td>
            <td class="num">{r.engine}</td>
            <td class="num" class:hot={(r.kv ?? 0) >= 90} class:warm={(r.kv ?? 0) >= 75 && (r.kv ?? 0) < 90}>
              {r.kv == null ? DASH : fmtPct(r.kv)}
            </td>
            <td class="num">{r.pool}</td>
            <td class="num">{r.mem}<span class="unit">{r.mem !== DASH ? " GiB" : ""}</span></td>
            <td class="num">{r.ttft == null ? DASH : fmtMs(r.ttft)}</td>
            <td class="num">{r.prefill == null ? DASH : fmtInt(r.prefill)}</td>
            <td class="num">{r.decode == null ? DASH : fmtInt(r.decode)}</td>
            <td class="num">{r.hit == null ? DASH : fmtPct(r.hit)}</td>
          </tr>
        {:else}
          <tr class="empty"><td colspan="10">no backend data yet</td></tr>
        {/each}
      </tbody>
    </table>
  </div>
  <div class="foot detail muted">
    <span>SLOTS · TTFT are router-measured</span>
    <span>RUN/WAIT · KV · PREFILL · DECODE · HIT are engine-measured</span>
    <span>ENG MEM = KV + weights, engine-reported</span>
  </div>
</section>

<style>
  .backend-table {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .scroll {
    overflow-x: auto;
  }
  .backend-table .data-table {
    table-layout: fixed;
  }
  .backend-table .data-table th { padding: 0; }
  .sort {
    appearance: none;
    display: flex;
    align-items: center;
    gap: 4px;
    width: 100%;
    min-height: 28px;
    padding: 6px 8px;
    background: transparent;
    border: 0;
    border-radius: 0;
    color: inherit;
    font: inherit;
    letter-spacing: inherit;
    text-transform: inherit;
    text-align: inherit;
    cursor: pointer;
  }
  .sort:hover {
    color: var(--ink);
  }
  .sort:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: -2px;
  }
  .sort::after {
    content: "";
    display: inline-block;
    width: 6px;
    height: 4px;
    margin-left: 5px;
    background: var(--sand);
    clip-path: polygon(50% 0, 100% 100%, 0 100%);
    opacity: 0;
  }
  th[aria-sort="ascending"] .sort::after {
    opacity: 1;
  }
  th[aria-sort="descending"] .sort::after {
    opacity: 1;
    transform: rotate(180deg);
  }
  .backend-table .data-table th:nth-child(1) { width: 17%; }
  .backend-table .data-table th:nth-child(2) { width: 8%; }
  .backend-table .data-table th:nth-child(3) { width: 10%; }
  .backend-table .data-table th:nth-child(4) { width: 7%; }
  .backend-table .data-table th:nth-child(5) { width: 13%; }
  .backend-table .data-table th:nth-child(6) { width: 8%; }
  .backend-table .data-table th:nth-child(7) { width: 8%; }
  .backend-table .data-table th:nth-child(8) { width: 9%; }
  .backend-table .data-table th:nth-child(9) { width: 9%; }
  .backend-table .data-table th:nth-child(10) { width: 7%; }
  .first {
    position: sticky;
    left: 0;
    z-index: 2;
    background: var(--panel);
  }
  tbody tr:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: -2px;
  }
  tbody tr:hover .first {
    background: var(--panel-2);
  }
  tbody tr.selected .first {
    background: var(--panel-3);
  }
  .name {
    color: var(--ink);
  }
  .tier {
    color: var(--sand);
    font-size: 10px;
    margin-left: 6px;
  }
  .engine-off {
    margin-left: 6px;
    font-size: 10px;
    color: var(--verm);
    text-transform: uppercase;
    letter-spacing: 0.08em;
  }
  .unit {
    color: var(--ink-3);
    font-size: 10px;
  }
  .hdot {
    display: inline-block;
    width: 4px;
    height: 4px;
    margin-right: 4px;
    vertical-align: 1px;
  }
  .hdot.router {
    background: var(--moss);
  }
  .hdot.engine {
    background: var(--water);
  }
  .data-table td.hot {
    color: var(--verm);
  }
  .data-table td.warm {
    color: var(--amber);
  }
  .foot {
    display: flex;
    gap: var(--s3);
    flex-wrap: wrap;
    padding: 5px var(--s3);
    border-top: 1px solid var(--rule);
    font-size: 9px;
  }
  @media (max-width: 639px) {
    .foot,
    .hint {
      display: none;
    }
    .backend-table .data-table th {
      padding: 0;
    }
    .sort {
      min-height: 44px;
      padding: 8px;
    }
    .backend-table .data-table th:nth-child(1) { width: 28%; }
    .backend-table .data-table th:nth-child(2) { width: 12%; }
    .backend-table .data-table th:nth-child(3) { width: 18%; }
    .backend-table .data-table th:nth-child(4) { width: 12%; }
    .backend-table .data-table th:nth-child(7) { width: 14%; }
    .backend-table .data-table th:nth-child(8) { width: 16%; }
    .backend-table .data-table th:nth-child(5),
    .backend-table .data-table td:nth-child(5),
    .backend-table .data-table th:nth-child(6),
    .backend-table .data-table td:nth-child(6),
    .backend-table .data-table th:nth-child(9),
    .backend-table .data-table td:nth-child(9),
    .backend-table .data-table th:nth-child(10),
    .backend-table .data-table td:nth-child(10) {
      display: none;
    }
  }
</style>
