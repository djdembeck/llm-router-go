<script lang="ts">
  import { onMount } from "svelte";
  import Masthead from "$lib/Masthead.svelte";
  import Sheet from "$lib/Sheet.svelte";
  import FleetStrip from "$lib/FleetStrip.svelte";
  import RequestTape from "$lib/RequestTape.svelte";
  import { clock } from "$lib/fmt.js";
  import { createMetricsStore, type StoreState } from "$lib/metrics.js";

  // The sheet: masthead → tessellated fold-cells → fleet strip → request
  // tape. Deploy state drives every cell's fold angle (one pull, whole
  // field). At narrow width the sheet auto-folds to the packet row.

  const initialState: StoreState = {
    live: false,
    health: "offline",
    feed: null,
    frame: null,
    hist: {},
    fleetHist: [],
    requests: [],
    spikeCount: 0,
    lastFrameAt: null,
    stale: false,
    spikePeak: 0,
  };

  let mstate: StoreState = $state(initialState);
  let deployed = $state(true);
  // cascade on/off replay without remounting the sheet — remounting reset
  // every trace's scroll window and lost the operator's continuity.
  let cascadeOn = $state(true);
  let open: string | null = $state(null);
  let narrow = $state(false);
  let now = $state(Date.now());

  let mq: MediaQueryList | null = null;
  let store: { destroy: () => void } | null = null;
  let cascadeTimer: ReturnType<typeof setTimeout> | null = null;
  let nowTimer: ReturnType<typeof setInterval> | null = null;

  // per-cell element registry: closing a fold returns focus to its cell
  // (otherwise a keyboard user is stranded at <body>)
  const cellEls = new Map<string, HTMLElement>();
  function registerEl(name: string, el: HTMLElement | null) {
    if (el) cellEls.set(name, el);
    else cellEls.delete(name);
  }

  // narrow viewport: the sheet folds to the packet row (mobile/compact)
  const effectiveDeployed = $derived(deployed && !narrow);
  // in narrow mode the sheet is forced folded, so the packet control
  // would be a dead button — hide it rather than lie.
  const showPacketControl = $derived(!narrow);

  // stale: the last truth is still on screen but old — the banner keeps
  // it there with a red time anchor instead of an empty hollow sheet.
  const showStale = $derived(mstate.stale && !!mstate.frame);
  const staleAgo = $derived.by(() => {
    void now; // recompute once a second
    if (!mstate.lastFrameAt) return "";
    const s = Math.max(1, Math.floor((now - mstate.lastFrameAt) / 1000));
    return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
  });

  const frame = $derived(mstate.frame);
  const feed = $derived(mstate.feed);
  const feedIsMock = $derived(!!feed && feed.kind === "sse-mock");
  const hollowMsg = $derived.by(() => {
    if (mstate.live) return "no backends configured — set BACKENDS in the router";
    if (mstate.feed) return `sheet offline — re-arming ${mstate.feed.url}…`;
    return "sheet offline — waiting for /metrics/stream";
  });

  // tier0 limit = sum of the King backends' concurrency caps (null when
  // no King is capped, or none configured) — the masthead's denominator
  const tier0Limit = $derived.by(() => {
    const backends = frame?.backends;
    if (!backends) return null;
    const kings = backends.filter((b) => b.tier === 0);
    const capped = kings.filter((b) => b.maxConcurrent > 0);
    return capped.length ? capped.reduce((a, b) => a + b.maxConcurrent, 0) : null;
  });

  onMount(() => {
    store = createMetricsStore((s) => {
      mstate = s;
    });
    mq = window.matchMedia("(max-width: 720px)");
    narrow = mq.matches;
    if (narrow) deployed = false; // narrow is folded — keep the state honest
    const onMq = (e: MediaQueryListEvent) => {
      const wasNarrow = narrow;
      narrow = e.matches;
      if (e.matches && !wasNarrow) {
        // folding into narrow: close any open fold, sync deploy state
        open = null;
        deployed = false;
      } else if (!e.matches && wasNarrow) {
        // going wide: re-deploy with the cascade
        deployed = true;
        replayCascade();
      }
    };
    mq.addEventListener("change", onMq);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && open) closeFold();
    };
    window.addEventListener("keydown", onKey);
    nowTimer = setInterval(() => (now = Date.now()), 1000);
    return () => {
      mq?.removeEventListener("change", onMq);
      window.removeEventListener("keydown", onKey);
      if (nowTimer) clearInterval(nowTimer);
      if (cascadeTimer) clearTimeout(cascadeTimer);
      store?.destroy();
    };
  });

  function replayCascade() {
    cascadeOn = false;
    if (cascadeTimer) clearTimeout(cascadeTimer);
    cascadeTimer = setTimeout(() => (cascadeOn = true), 30);
  }

  function toggleDeploy() {
    deployed = !deployed;
    if (!deployed) open = null;
    replayCascade();
  }

  function toggleCell(name: string) {
    if (open === name) closeFold();
    else open = name;
  }

  function closeFold() {
    const prev = open;
    open = null;
    if (prev) cellEls.get(prev)?.focus();
  }
</script>

<svelte:head>
  <title>llm-router-go — fleet sheet</title>
</svelte:head>

<div class="page">
  <Masthead
    totalInFlight={frame?.totals?.inFlight ?? 0}
    totalWaiting={frame?.totals?.waiting ?? 0}
    gpu={frame ? frame.gpu : null}
    tier0Inflight={frame?.tier0Inflight ?? 0}
    tier0Limit={tier0Limit}
    deployed={effectiveDeployed}
    showPacketControl={showPacketControl}
    onToggle={toggleDeploy}
  />

  <div class="feed-row unskew">
    <span class="health" data-h={mstate.health}>
      <i></i>{mstate.health}
    </span>
    <span class="feed-note">
      {#if feed}
        feed: {feed.url}{feedIsMock ? ' (dev mock)' : ''}
      {:else}
        feed: —
      {/if}
    </span>
    {#if mstate.lastFrameAt}
      <span class="feed-last">last data {clock(mstate.lastFrameAt)}</span>
    {/if}
    <span class="feed-est">token figures are <em>est</em> — prefill, not decode</span>
  </div>

  {#if showStale}
    <div class="stale-banner unskew" role="alert">
      <span>
        stale — last data {clock(mstate.lastFrameAt ?? 0)} · {staleAgo} ago
      </span>
      <span class="stale-why">
        {#if feed}
          re-arming <b>{feed.url}</b>
        {:else}
          no feed
        {/if}
      </span>
    </div>
  {/if}

  {#if frame && frame.backends.length > 0}
    <Sheet
      frame={frame}
      hist={mstate.hist}
      deployed={effectiveDeployed}
      cascadeOn={cascadeOn}
      open={open}
      onToggle={toggleCell}
      registerEl={registerEl}
    />

    <FleetStrip frame={frame} hist={mstate.fleetHist} />

    <RequestTape
      requests={mstate.requests}
      spikeCount={mstate.spikeCount}
      spikePeak={mstate.spikePeak}
      live={mstate.live}
      stale={mstate.stale}
    />
  {:else}
    <div class="hollow" role="status">
      <span>{hollowMsg}</span>
    </div>
  {/if}

  <!-- the flat, non-skewed, accessible diagram of the same numbers -->
  {#if frame && frame.backends.length > 0}
    <table class="sr-only" aria-label="fleet fold-cells (flat table)">
      <caption>
        Fleet fold-cells — flat table of the same numbers{mstate.stale ? " (stale data; feed re-arming)" : ""}
      </caption>
      <thead>
        <tr>
          <th scope="col">backend</th>
          <th scope="col">tier</th>
          <th scope="col">in-flight</th>
          <th scope="col">limit</th>
          <th scope="col">waiting</th>
          <th scope="col">prefill</th>
          <th scope="col">ewma duration</th>
          <th scope="col">ttft (streaming)</th>
          <th scope="col">req/s</th>
          <th scope="col">tok/s (est)</th>
          <th scope="col">engine running</th>
          <th scope="col">engine queue</th>
          <th scope="col">kv cache %</th>
          <th scope="col">engine prefill tok/s (real)</th>
          <th scope="col">engine decode tok/s (real)</th>
          <th scope="col">engine ttft</th>
        </tr>
      </thead>
      <tbody>
        {#each frame.backends as b (b.name)}
          <tr>
            <th scope="row">{b.name}</th>
            <td>{b.tier === 0 ? "0 (king)" : "1 (subject)"}</td>
            <td>{b.inFlight}</td>
            <td>{b.maxConcurrent > 0 ? b.maxConcurrent : "unlimited"}</td>
            <td>{b.waiting}</td>
            <td>
              {b.prefillInFlight}
              {b.prefillMax > 0 ? ` of ${b.prefillMax}` : ""}
            </td>
            <td>{(b.ewmaMs / 1000).toFixed(1)}s</td>
            <td>{b.ttftSampleCount === 0 ? "no samples" : `${b.ttftMs}ms`}</td>
            <td>{b.reqRate}/s</td>
            <td>{b.tokEstRate}/s (estimated prefill)</td>
            {#if b.engine?.status === "ok"}
              <td>{Math.round(b.engine.running)}</td>
              <td>{Math.round(b.engine.waiting)}</td>
              <td>{b.engine.kvPct > 0 ? `${b.engine.kvPct.toFixed(0)}%` : "—"}</td>
              <td>{Math.round(b.engine.prefillTokS)}/s</td>
              <td>{Math.round(b.engine.decodeTokS)}/s</td>
              <td>{b.engine.ttftMs > 0 ? `${Math.round(b.engine.ttftMs)}ms` : "—"}</td>
            {:else}
              <td colspan="6">no engine feed ({b.engine?.status === "err" ? "unreachable" : "endpoint off"})</td>
            {/if}
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</div>

<style>
  .feed-row {
    display: flex;
    align-items: center;
    gap: 14px;
    margin-top: 10px;
    font-size: 10px;
    letter-spacing: 0.08em;
    flex-wrap: wrap;
  }
  .feed-note {
    color: var(--tx-3);
  }
  .feed-last {
    color: var(--tx-3);
    font-variant-numeric: tabular-nums;
  }
  .feed-est {
    margin-left: auto;
    color: var(--tx-3);
  }
  .feed-est em {
    color: var(--gold);
    font-style: normal;
  }
</style>
