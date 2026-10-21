<script lang="ts">
  import { onMount } from "svelte";
  import TopBar from "$lib/TopBar.svelte";
  import FleetRail from "$lib/FleetRail.svelte";
  import ReadingsStrip from "$lib/ReadingsStrip.svelte";
  import FleetProfile from "$lib/FleetProfile.svelte";
  import LiveSessions from "$lib/LiveSessions.svelte";
  import BackendTable from "$lib/BackendTable.svelte";
  import RequestFeed from "$lib/RequestFeed.svelte";
  import StaleBanner from "$lib/StaleBanner.svelte";
  import { LiveStore } from "$lib/live.svelte";
  import { HistoryStore } from "$lib/history.svelte";
  import type { Frame, MetricAverages, RangeName } from "$lib/api";
  import type { LayerId } from "$lib/profile";

  const live = new LiveStore();
  const history = new HistoryStore();

  let now = $state(Date.now());
  let selected = $state<string | null>(null);
  let layerId = $state<LayerId>("kv");
  let requestLogOpen = $state(false);
  let cursor = $state<number | null>(null);
  let theme = $state<"night" | "day">(
    typeof document !== "undefined" && document.documentElement.dataset.theme === "day"
      ? "day"
      : "night",
  );
  let clockTimer: number | null = null;

  // Samples arrive every second. A one-second sample is not a meaningful
  // display window for TTFT or engine throughput, so the summary reads a 15s
  // rolling mean and refreshes every 5s. Counts stay live; the profile and
  // sessions remain 1s. The longer hold is also what lets the 1.2s number
  // tween settle instead of chasing each scrape.
  const METRIC_WINDOW = 15;
  const METRIC_HOLD_MS = 5000;
  let metricFrame = $state<Frame | null>(null);
  let metricAverages = $state<MetricAverages | null>(null);
  let metricTimer: ReturnType<typeof setTimeout> | null = null;
  let metricLast = 0;
  $effect(() => {
    const f = live.frame;
    if (!f) return;
    const wait = Math.max(0, metricLast + METRIC_HOLD_MS - Date.now());
    if (metricTimer) clearTimeout(metricTimer);
    metricTimer = setTimeout(() => {
      const tail = live.liveFrames.slice(-METRIC_WINDOW);
      const avg = (pick: (fr: Frame) => number | null): number | null => {
        let sum = 0;
        let n = 0;
        for (const fr of tail) {
          const v = pick(fr);
          if (v == null || !Number.isFinite(v)) continue;
          sum += v;
          n++;
        }
        return n ? sum / n : null;
      };
      metricAverages = {
        ttftMs: avg((fr) => (fr.totals.ttftSampleCount > 0 ? fr.totals.ttftMs : null)),
        prefillTokS: avg((fr) => fr.totals.prefillTokS),
        decodeTokS: avg((fr) => fr.totals.decodeTokS),
      };
      metricFrame = f;
      metricLast = Date.now();
    }, wait);
    return () => {
      if (metricTimer) clearTimeout(metricTimer);
    };
  });

  onMount(() => {
    const el = document.documentElement;
    theme = el.dataset.theme === "day" ? "day" : "night";
    live.start();
    history.start();
    clockTimer = window.setInterval(() => (now = Date.now()), 1000);
    return () => {
      live.destroy();
      history.destroy();
      clearInterval(clockTimer ?? 0);
    };
  });

  let lastFeedKind = "";
  $effect(() => {
    const kind = live.feed?.kind ?? "";
    if (kind && lastFeedKind && (kind === "sse-mock") !== (lastFeedKind === "sse-mock")) {
      // Switching between the real fleet and the dev mock means a different
      // fleet; clear history. A real→stats transport fallback is the SAME
      // fleet and keeps the last fetched series (frozen truth).
      history.clear();
      cursor = null;
      metricFrame = null;
      metricAverages = null;
      metricLast = 0;
    }
    if (kind) lastFeedKind = kind;
  });

  const showStale = $derived(live.stale && !!live.frame);
  const feedName = $derived(
    live.feed
      ? live.feed.kind === "sse"
        ? "/metrics/stream"
        : live.feed.kind === "sse-mock"
          ? "dev mock · /dev-metrics/stream"
          : "/stats · polling"
      : "no feed",
  );
  const historyError = $derived(
    history.error && !history.data ? "history feed unavailable — showing live frames only" : null,
  );

  function toggleTheme(): void {
    theme = theme === "night" ? "day" : "night";
    document.documentElement.dataset.theme = theme;
    try {
      localStorage.setItem("lrg-theme", theme);
    } catch {
      /* private mode: the choice lasts this session */
    }
  }

  function onRange(r: RangeName): void {
    cursor = null;
    history.setRange(r);
  }
</script>

<svelte:head>
  <title>llm-router-go — fleet survey</title>
  <meta
    name="description"
    content="Live GPU-aware inference routing: sessions, GPU and KV pressure, prefill and decode throughput, TTFT, and history."
  />
</svelte:head>

<h1 class="sr-only">llm-router-go fleet survey</h1>

<div class="app">
  <TopBar
    health={live.health}
    feedName={feedName}
    mock={live.feed?.kind === "sse-mock"}
    lastFrameAt={live.lastFrameAt}
    paused={live.paused}
    range={history.range}
    {now}
    {theme}
    onRange={onRange}
    onPause={() => live.togglePause()}
    onTheme={toggleTheme}
  />

  {#if showStale}
    <StaleBanner lastFrameAt={live.lastFrameAt} {now} feedName={feedName} />
  {/if}
  {#if historyError}
    <div class="notice" role="status">{historyError}</div>
  {/if}

  <div class="body">
    <FleetRail
      frame={live.frame}
      metricFrame={metricFrame}
      history={history.data}
      {selected}
      onSelect={(name) => (selected = name)}
      feed={live.feed}
      lastFrameAt={live.lastFrameAt}
      {now}
    />

    <main class="main">
      <ReadingsStrip
        frame={live.frame}
        metricFrame={metricFrame}
        averages={metricAverages}
        history={history.data}
        {layerId}
        onLayer={(id) => (layerId = id)}
      />

      <div class="live-band">
        <section class="profile-sheet sheet" aria-label="fleet profile">
          <FleetProfile
            history={history.data}
            frames={live.liveFrames}
            framesVersion={live.framesVersion}
            {layerId}
            range={history.range}
            {selected}
            bind:cursor
            requests={live.requests}
            sessions={live.sessions}
            paused={live.paused}
            stale={live.stale}
            {theme}
          />
        </section>

        <LiveSessions
          feed={live.sessions}
          now={live.paused || live.stale ? (live.lastFrameAt ?? now) : now}
          paused={live.paused}
          stale={live.stale}
          tall={!requestLogOpen}
        />
      </div>

      <div class="lower">
        <BackendTable
          frame={metricFrame ?? live.frame}
          history={history.data}
          {cursor}
          {selected}
          onSelect={(name) => (selected = name)}
          stale={live.stale}
          lastFrameAt={live.lastFrameAt}
        />
        <RequestFeed
          bind:open={requestLogOpen}
          requests={live.requests}
          spikeCount={live.spikeCount}
          spikePeak={live.spikePeak}
        />
      </div>
    </main>
  </div>
</div>

<style>
  .app {
    max-width: 1760px;
    margin: 0 auto;
    min-height: 100vh;
    background: var(--ground);
    transition: background-color var(--dur) var(--ease-soft);
  }
  .body {
    display: grid;
    grid-template-columns: 232px minmax(0, 1fr);
    align-items: stretch;
  }
  .main {
    display: flex;
    flex-direction: column;
    min-width: 0;
    padding: 0 0 32px;
  }
  .live-band {
    display: grid;
    grid-template-columns: minmax(0, 1fr) 400px;
    gap: var(--s3);
    padding: var(--s3) var(--s3) 0;
    align-items: stretch;
  }
  .profile-sheet {
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .profile-sheet :global(.profile) {
    flex: 1 1 auto;
  }
  .lower {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--s3);
    padding: var(--s3) var(--s3) 0;
    align-items: start;
  }
  .notice {
    padding: 5px var(--s5);
    border-bottom: 1px solid var(--rule);
    background: var(--panel);
    color: var(--ink-3);
    font-size: var(--fs-detail);
  }
  @media (min-width: 1600px) {
    .lower {
      grid-template-columns: minmax(0, 1.4fr) minmax(0, 1fr);
    }
  }
  @media (max-width: 1279px) {
    .live-band {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  @media (max-width: 959px) {
    .body {
      grid-template-columns: minmax(0, 1fr);
    }
    .main {
      padding-bottom: var(--s6);
    }
    .live-band,
    .lower {
      padding: var(--s3) var(--s3) 0;
    }
  }
  @media (max-width: 639px) {
    .main {
      padding-bottom: var(--s5);
    }
    .live-band,
    .lower {
      padding: var(--s2) var(--s2) 0;
      gap: var(--s2);
    }
    .notice {
      padding: 5px var(--s3);
      font-size: var(--fs-label);
    }
  }
</style>
