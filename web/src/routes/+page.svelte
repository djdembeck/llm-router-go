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
  import type { BurstRequest } from "$lib/api";
  import type { LayerId } from "$lib/profile";

  const live = new LiveStore();
  const history = new HistoryStore();
  let bandEl: HTMLDivElement | undefined = $state();
  /**
   * At >=1280px the live band is capped to the first viewport. The cap is the
   * band's distance from the viewport top (not a constant per-mode offset), so
   * chrome that inserts above the band — the STALE banner, the history-feed
   * notice, a wrapped server-identity row — shrinks the band in place instead
   * of pushing its bottom past the fold. Sizing stays inert below 1280px, and
   * the 643px natural cap (CSS) keeps tall windows unchanged.
   */
  $effect(() => {
    const el = bandEl;
    const mq = window.matchMedia("(min-width: 1280px)");
    let bandRo: ResizeObserver | null = null;
    const apply = () => {
      if (!el) return;
      // Document-relative top, not viewport-relative: the fold is the first
      // viewport, and the page scrolls — rect.top alone would grow the band
      // as the user scrolled down.
      const top = el.getBoundingClientRect().top + window.scrollY;
      el.style.setProperty(
        "--band-offset",
        mq.matches ? `${Math.ceil(top)}px` : "",
      );
    };
    apply();
    bandRo = new ResizeObserver(apply);
    if (el) bandRo.observe(el);
    // The body covers chrome inserted above the band (STALE banner, notice,
    // a wrapped identity row); the band itself covers its own padding changes.
    bandRo.observe(document.body);
    mq.addEventListener("change", apply);
    return () => {
      mq.removeEventListener("change", apply);
      bandRo?.disconnect();
    };
  });

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
    // /stats frames carry no engine/GPU data. On either crossing of the
    // boundary, a cached metric frame from the other feed kind would keep
    // presenting its readings as current, so gate the cache on the kind.
    if ((kind === "stats") !== (lastFeedKind === "stats")) {
      metricFrame = null;
      metricAverages = null;
      metricLast = 0;
    }
    if (kind) lastFeedKind = kind;
  });

  const singleBackend = $derived(
    live.frame?.backends.length === 1 ? live.frame.backends[0] : null,
  );
  const singleBackendName = $derived(singleBackend?.name ?? null);
  const statsFallback = $derived(live.feed?.kind === "stats");
  const fleetMode = $derived((live.frame?.backends.length ?? 0) > 1);
  const displayMetricFrame = $derived(
    metricFrame && live.frame &&
    metricFrame.backends.length === live.frame.backends.length &&
    metricFrame.backends.every((b, i) => b.name === live.frame?.backends[i]?.name)
      ? metricFrame
      : live.frame,
  );
  $effect(() => {
    if (singleBackend || (selected && !live.frame?.backends.some((b) => b.name === selected))) {
      selected = null;
    }
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

  // Burst polling keeps replacing the list while the stream is stale, but
  // the tape must hold under the STALE banner: capture the snapshot at the
  // stale transition and keep returning that same reference (plain vars,
  // no extra reactivity).
  let heldRequests: BurstRequest[] | null = null;
  const frozenRequests = $derived.by(() => {
    if (live.stale) {
      if (!heldRequests) heldRequests = live.requests;
      return heldRequests;
    }
    heldRequests = null;
    return live.requests;
  });

  // 429s are grouped in the burst ring by backend; the rail needs the
  // 5-minute count per lane for its saturation cell (lifetime counters cannot
  // answer "is pressure building now"). Only 429s count — burst responses
  // also carry other >=400 codes, and a 500 is not a saturation signal.
  const rejectMins = $derived.by(() => {
    const cut = now - 5 * 60_000;
    const counts: Record<string, number> = {};
    for (const r of frozenRequests) {
      if (r.status === 429 && r.t >= cut) counts[r.backend] = (counts[r.backend] ?? 0) + 1;
    }
    return counts;
  });

  function toggleTheme(): void {
    theme = theme === "night" ? "day" : "night";
    // The bootstrap helper owns the dataset plus the tab icon and browser
    // chrome, so a themed page never sits under the other sheet's icon.
    (window as Window & { __lrgSheet?: (t: "night" | "day") => void }).__lrgSheet?.(theme);
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
  <title>{singleBackend ? `${singleBackend.name} — server survey` : "llm-router-go — fleet survey"}</title>
  <meta
    name="description"
    content="Live GPU-aware inference routing: sessions, GPU and KV pressure, prefill and decode throughput, TTFT, and history."
  />
</svelte:head>

<h1 class="sr-only">{singleBackend ? `${singleBackend.name} server survey` : "llm-router-go fleet survey"}</h1>

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

  <div class="body" class:fleet={fleetMode} class:single={singleBackend != null}>
    {#if fleetMode}
      <FleetRail
        frame={live.frame}
        metricFrame={displayMetricFrame}
        history={history.data}
        {selected}
        onSelect={(name) => (selected = name)}
        feed={live.feed}
        lastFrameAt={live.lastFrameAt}
        {now}
        {rejectMins}
      />
    {/if}

    <main class="main">
      {#if singleBackend}
        <section class="server-context" aria-label="server">
          <div class="server-identity">
            <h2 class="server-name mono">{singleBackend.name}</h2>
            <span
              class="server-engine detail mono"
              class:err={singleBackend.engine.status === "err"}
              title={statsFallback ? "Engine telemetry is unavailable through /stats" : singleBackend.engine.lastError || "Engine Prometheus /metrics scrape"}
            >
              <span class="dot" class:ok={singleBackend.engine.status === "ok"} aria-hidden="true"></span>
              {singleBackend.engine.engine || "engine"} · metrics {statsFallback ? "unavailable" : singleBackend.engine.status === "err" ? "error" : singleBackend.engine.status}
            </span>
          </div>
          <div class="server-context-meta detail mono muted">
            <span>{singleBackend.maxConcurrent || "unlimited"} request slots</span>
            <span
              class:err={live.frame?.gpuFeed.status === "err"}
              title={statsFallback ? "Device telemetry is unavailable through /stats" : live.frame?.gpuFeed.lastError || "Device telemetry from GPU_METRICS_URL"}
            >
              {#if statsFallback}
                GPU telemetry unavailable
              {:else if live.frame?.gpuFeed.status === "ok"}
                {live.frame.gpuDevices?.length ?? 0} GPU devices
              {:else if live.frame?.gpuFeed.status === "err"}
                GPU exporter error
              {:else if live.frame?.gpuFeed.status === "off"}
                GPU exporter unavailable
              {:else}
                GPU metrics not configured
              {/if}
            </span>
          </div>
        </section>
      {:else if live.frame?.backends.length === 0}
        <div class="notice">no backends configured</div>
      {/if}
      <ReadingsStrip
        singleBackend={singleBackendName}
        frame={live.frame}
        metricFrame={displayMetricFrame}
        averages={displayMetricFrame === metricFrame ? metricAverages : null}
        history={history.data}
        {layerId}
        onLayer={(id) => (layerId = id)}
      />

      <div class="live-band" class:log-open={requestLogOpen} bind:this={bandEl}>
        <section class="profile-sheet sheet" aria-label={singleBackend ? "server profile" : "fleet profile"}>
          <FleetProfile
            singleBackend={singleBackendName}
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
          singleBackend={singleBackendName}
          feed={live.sessions}
          now={live.paused || live.stale || live.sessionsStale ? (live.lastFrameAt ?? now) : now}
          paused={live.paused}
          stale={live.stale || live.sessionsStale}
          tall={!requestLogOpen}
        />
      </div>

      <div class="lower">
        <BackendTable
          singleBackend={singleBackendName}
          frame={displayMetricFrame}
          history={history.data}
          {cursor}
          {selected}
          onSelect={(name) => (selected = name)}
          stale={live.stale}
          lastFrameAt={live.lastFrameAt}
        />
        <RequestFeed
          singleBackend={singleBackendName}
          bind:open={requestLogOpen}
          requests={frozenRequests}
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
    grid-template-columns: minmax(0, 1fr);
    align-items: stretch;
  }
  .body.fleet {
    grid-template-columns: 232px minmax(0, 1fr);
  }
  .server-context {
    display: flex;
    align-items: center;
    justify-content: space-between;
    flex-wrap: wrap;
    gap: var(--s2) var(--s5);
    padding: var(--s3) var(--s4);
    border-bottom: 1px solid var(--rule);
    background: var(--panel);
  }
  .server-identity,
  .server-context-meta {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    min-width: 0;
    gap: var(--s2) var(--s4);
  }
  .server-name {
    margin: 0;
    font-size: var(--fs-title);
    font-weight: 500;
    line-height: 20px;
    overflow-wrap: anywhere;
  }
  .server-engine {
    display: inline-flex;
    align-items: center;
    gap: var(--s2);
    color: var(--ink-3);
  }
  .server-engine .dot {
    width: 5px;
    height: 5px;
    flex: 0 0 auto;
    background: var(--ink-3);
  }
  .server-engine .dot.ok {
    background: var(--moss);
  }
  .server-engine.err {
    color: var(--verm);
  }
  .server-engine.err .dot {
    background: var(--verm);
  }
  .body.single .lower {
    grid-template-columns: minmax(0, 1fr);
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
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  .profile-sheet :global(.profile) {
    flex: 1 1 auto;
    min-height: 0;
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
  /* Laptop/desktop (≥1280): the live band must end at the fold so the two
     readings rows above it and the profile+sessions below it read in one
     glance. The band height is capped at its natural 643px sheet height (so
     tall windows are unchanged) and shrinks with the viewport when the fold
     is higher. Inside, the profile plot already flexes; the session list is
     flexed to fill so both share the row and meet at the bottom. The offset
     is the band's measured distance from the viewport top, so chrome inserted
     above it (the STALE banner, the history notice, a wrapped server-identity
     row) shrinks the band instead of pushing it past the fold. The cap only
     applies while the request log is closed: with the log open the band
     falls back to auto sizing and the session list's 320px trade lifts the
     lower band back up, as before the cap. */
  @media (min-width: 1280px) {
    /* Closed log only: with the request log open the band falls back to auto
       sizing so the historical 560→320 session-list trade lifts the lower
       band back up. The min-content floor keeps a short viewport from
       compressing the band below its children's floors (96px plot, 280px
       list): the band grows past the cap and the page scrolls, instead of
       the profile foot/ruler painting over .lower. */
    .live-band:not(.log-open) {
      height: min(643px, calc(100dvh - var(--band-offset, 258px)));
      min-height: min-content;
    }
  }
  @media (max-width: 1279px) {
    .live-band {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  @media (min-width: 1024px) and (max-width: 1279px) {
    .body.single .live-band {
      grid-template-columns: minmax(0, 1fr) 360px;
    }
  }
  @media (max-width: 959px) {
    .body.fleet {
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
