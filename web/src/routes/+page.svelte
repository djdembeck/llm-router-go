<script lang="ts">
  import { onMount } from "svelte";
  import Masthead from "$lib/Masthead.svelte";
  import Sheet from "$lib/Sheet.svelte";
  import FleetStrip from "$lib/FleetStrip.svelte";
  import RequestTape from "$lib/RequestTape.svelte";
  import { createMetricsStore, type StoreState } from "$lib/metrics.js";

  // The sheet: masthead → tessellated fold-cells → fleet strip → request
  // tape. Deploy state drives every cell's fold angle (one pull, whole
  // field). At narrow width the sheet is auto-folded to the packet row.

  const initialState: StoreState = {
    live: false,
    health: "offline",
    feed: null,
    frame: null,
    hist: {},
    fleetHist: [],
    requests: [],
    spikeCount: 0,
  };

  let mstate: StoreState = $state(initialState);
  let epoch = $state(0);
  let deployed = $state(true);
  let cascadeKey = $state(0);
  let open: string | null = $state(null);
  let narrow = $state(false);

  let mq: MediaQueryList | null = null;
  let store: { destroy: () => void } | null = null;

  onMount(() => {
    store = createMetricsStore((s) => {
      mstate = s;
      epoch++;
    });
    mq = window.matchMedia("(max-width: 720px)");
    narrow = mq.matches;
    const onMq = (e: MediaQueryListEvent) => {
      narrow = e.matches;
      if (e.matches) open = null;
    };
    mq.addEventListener("change", onMq);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") open = null;
    };
    window.addEventListener("keydown", onKey);
    return () => {
      mq?.removeEventListener("change", onMq);
      window.removeEventListener("keydown", onKey);
      store?.destroy();
    };
  });

  const isNarrow = $derived(narrow);
  // narrow viewport: the sheet folds to the packet row (mobile/compact)
  const effectiveDeployed = $derived(deployed && !isNarrow);

  function toggleDeploy() {
    deployed = !deployed;
    if (!deployed) open = null;
    cascadeKey++; // replays the stepped diagonal cascade
  }

  function toggleCell(name: string) {
    open = open === name ? null : name;
  }

  const frame = $derived(mstate.frame);
  const feed = $derived(mstate.feed);
  const feedIsMock = $derived(!!feed && feed.kind === "sse-mock");
  const hollowMsg = $derived.by(() => {
    if (mstate.live) return "no backends configured — set BACKENDS in the router";
    if (mstate.feed) return "sheet offline — re-arming the feed…";
    return "sheet offline — waiting for /metrics/stream";
  });
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
    deployed={effectiveDeployed}
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
    <span class="feed-est">token figures are <em>est</em> — prefill, not decode</span>
  </div>

  {#if frame && frame.backends.length > 0}
    {#key cascadeKey}
      <Sheet
        frame={frame}
        hist={mstate.hist}
        deployed={effectiveDeployed}
        open={open}
        onToggle={toggleCell}
      />
    {/key}

    <FleetStrip frame={frame} hist={mstate.fleetHist} />

    <RequestTape
      requests={mstate.requests}
      spikeCount={mstate.spikeCount}
      live={mstate.live}
    />
  {:else}
    <div class="hollow" role="status">
      <span>{hollowMsg}</span>
    </div>
  {/if}

  <!-- the flat, non-skewed, accessible diagram of the same numbers -->
  {#if frame && frame.backends.length > 0}
    <table class="sr-only" aria-label="fleet fold-cells (flat table)">
      <caption>Fleet fold-cells — flat table of the same numbers</caption>
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
  }
  .feed-note {
    color: var(--tx-3);
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
