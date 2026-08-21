// The live metrics store. Types mirror .impeccable/metrics-contract.md
// field-for-field (name + type). Feed ladder:
//   1. /metrics/stream (SSE) — primary, production and dev
//   2. /dev-metrics/stream (SSE) — dev-only mock, when no router is up
//   3. /stats (polling) — last resort when both SSE feeds fail
// Production (import.meta.env.PROD) only ever tries 1 then 3.
//
// The router measures requests, bytes, queue/concurrency/GPU state, total
// duration EWMA, and streaming first-byte TTFT. Token figures are ESTIMATED
// new-prefill tokens (~4 bytes/token, request-body based) — every display
// of them carries an "est" marker and nothing is shown as measured decode
// throughput.

import { browser } from "$app/environment";

// ── contract types ───────────────────────────────────────────────────────

export interface BackendMetrics {
  name: string;
  url: string;
  tier: number;
  inFlight: number;
  maxConcurrent: number;
  waiting: number;
  maxQueueDepth: number;
  prefillInFlight: number;
  prefillWaiting: number;
  prefillMax: number;
  avgDurationS: number;
  ewmaMs: number;
  ttftMs: number;
  ttftSampleCount: number;
  /** most recent single first-byte sample (ms); 0 until seen — the live value */
  ttftMsNow: number;
  reqRate: number;
  bytesRate: number;
  tokEstRate: number;
  reqTotal: number;
  bytesInTotal: number;
  bytesOutTotal: number;
  tokEstTotal: number;
  /** the backend's own engine state (scraped from its /metrics) */
  engine: EngineMetrics;
}

/**
 * In-engine truth, scraped from the backend's Prometheus /metrics at 1s.
 * status "ok" = live; "off" = endpoint absent (e.g. SGLang without
 * --enable-metrics); "err" = unreachable. Zero + non-ok means "no data".
 * Core fields are always present; family-specific fields are null when the
 * engine (or feature) does not report them. Engine figures are measured —
 * never estimated.
 */
export interface EngineMetrics {
  status: "ok" | "off" | "err";
  /** engine family: "vllm" | "sglang" | "" (unknown) */
  engine: string;
  running: number;
  waiting: number;
  /** KV/token-pool pressure, 0..100; 0 = unknown */
  kvPct: number;
  /** prefix-cache hit rate, 0..1 */
  hitRate: number;
  /** compute-only prefill tok/s (cache misses) */
  prefillTokS: number;
  /** cache-hit prefill tok/s */
  prefillCacheTokS: number;
  decodeTokS: number;
  /** mean TTFT over the last scrape interval, ms */
  ttftMs: number;
  itlMs: number;
  e2eMs: number;
  queueMs: number;
  /** per-request means over the last interval */
  meanPromptTok: number;
  meanGenTok: number;
  /** lifetime completed requests */
  reqDoneTotal: number;
  /** vllm: waiting split by reason (capacity / deferred) */
  waitCap: number | null;
  waitDefer: number | null;
  /** sglang: retracted requests + retraction tok/s */
  retracted: number | null;
  retractedTokS: number | null;
  /** sglang: full/SWA/mamba pool pressure, 0..100 */
  fullPct: number | null;
  swaPct: number | null;
  mambaPct: number | null;
  /** sglang: KV pool token counts */
  kvUsedTok: number | null;
  kvCapTok: number | null;
  kvFreeTok: number | null;
  kvEvictTok: number | null;
  mambaUsedTok: number | null;
  mambaCapTok: number | null;
  /** sglang: hicache host offload tokens */
  hicacheHostUsedTok: number | null;
  hicacheHostCapTok: number | null;
  /** sglang: TTFT means split by is_streaming */
  ttftStreamMs: number | null;
  ttftNonStreamMs: number | null;
  /** vllm: per-stage request-time means (ms) */
  prefillMs: number | null;
  decodeMs: number | null;
  perTokMs: number | null;
  /** sglang: memory + capacity */
  kvMemGB: number | null;
  weightMemGB: number | null;
  sloCap: number | null;
  ctxLen: number | null;
  preemptedTotal: number | null;
  abortedTotal: number | null;
  streamDoneTotal: number | null;
  nonStreamDoneTotal: number | null;
  /** lifetime prompt / generation tokens */
  promptTokTotal: number | null;
  genTokTotal: number | null;
  hitQueriesTotal: number | null;
  hitHitsTotal: number | null;
  mmQueriesTotal: number | null;
  mmHitsTotal: number | null;
  /** vllm: lifetime finished requests by reason */
  finReasons: Record<string, number> | null;
}

export const emptyEngine: EngineMetrics = {
  status: "off",
  engine: "",
  running: 0,
  waiting: 0,
  kvPct: 0,
  hitRate: 0,
  prefillTokS: 0,
  prefillCacheTokS: 0,
  decodeTokS: 0,
  ttftMs: 0,
  itlMs: 0,
  e2eMs: 0,
  queueMs: 0,
  meanPromptTok: 0,
  meanGenTok: 0,
  reqDoneTotal: 0,
  waitCap: null,
  waitDefer: null,
  retracted: null,
  retractedTokS: null,
  fullPct: null,
  swaPct: null,
  mambaPct: null,
  kvUsedTok: null,
  kvCapTok: null,
  kvFreeTok: null,
  kvEvictTok: null,
  mambaUsedTok: null,
  mambaCapTok: null,
  hicacheHostUsedTok: null,
  hicacheHostCapTok: null,
  ttftStreamMs: null,
  ttftNonStreamMs: null,
  prefillMs: null,
  decodeMs: null,
  perTokMs: null,
  kvMemGB: null,
  weightMemGB: null,
  sloCap: null,
  ctxLen: null,
  preemptedTotal: null,
  abortedTotal: null,
  streamDoneTotal: null,
  nonStreamDoneTotal: null,
  promptTokTotal: null,
  genTokTotal: null,
  hitQueriesTotal: null,
  hitHitsTotal: null,
  mmQueriesTotal: null,
  mmHitsTotal: null,
  finReasons: null,
};

export interface GpuMetrics {
  used: number;
  budget: number;
  waiting: number;
  active: boolean;
}

export interface TotalsMetrics {
  inFlight: number;
  waiting: number;
  reqRate: number;
  bytesRate: number;
  tokEstRate: number;
  reqTotal: number;
  bytesInTotal: number;
  bytesOutTotal: number;
  tokEstTotal: number;
}

/** One SSE frame: a full live snapshot. */
export interface Frame {
  t: number;
  backends: BackendMetrics[];
  gpu: GpuMetrics;
  tier0Inflight: number;
  totals: TotalsMetrics;
}

export interface BurstRequest {
  t: number;
  backend: string;
  path: string;
  status: number;
  stream: boolean;
  durMs: number;
  ttftMs: number;
  newTokensEst: number;
  bytesIn: number;
  bytesOut: number;
}

export interface BurstResponse {
  requests: BurstRequest[];
}

// ── session feed (GET /metrics/sessions) ─────────────────────────────────

/** One in-flight request (router-side, live stack). Token figures are est. */
export interface LiveReq {
  id: number;
  /** conversation session id (12-hex); "" = not a chat request */
  sess: string;
  backend: string;
  path: string;
  stream: boolean;
  phase: "admitted" | "streaming";
  startMs: number;
  ctxTok: number;
  newTok: number;
}

/** One completed request inside a session. Token figures are est. */
export interface SessionReq {
  tMs: number;
  status: number;
  stream: boolean;
  durMs: number;
  ttftMs: number;
  ctxTok: number;
  newTok: number;
  cachedTok: number;
  tokOut: number;
  tokS: number;
}

/** One conversation, aggregated. Token figures are est (body-based). */
export interface SessionRow {
  id: string;
  backend: string;
  n: number;
  firstMs: number;
  lastMs: number;
  active: boolean;
  liveN: number;
  ctxTok: number;
  newTok: number;
  cachedTok: number;
  avgTTFTMs: number;
  totalDurS: number;
  tokOutTotal: number;
  lastStatus: number;
  /** last 32 completed requests, oldest first; null when not fresh enough */
  reqs: SessionReq[] | null;
}

export interface SessionsFeed {
  t: number;
  live: LiveReq[];
  sessions: SessionRow[];
}

/** The session feed: /metrics/sessions (router), dev-metrics fallback. */
export async function fetchSessions(): Promise<SessionsFeed | null> {
  for (const url of ["/metrics/sessions", "/dev-metrics/sessions"]) {
    try {
      const res = await fetch(url, { cache: "no-store" });
      if (!res.ok) continue;
      const j = (await res.json()) as SessionsFeed;
      if (j && Array.isArray(j.live) && Array.isArray(j.sessions)) return j;
      return null;
    } catch {
      return null;
    }
  }
  return null;
}

// /stats fallback shape (pre-dates the SSE fields).
interface StatsBackend {
  name: string;
  url: string;
  tier: number;
  inFlight: number;
  maxConcurrent: number;
  waiting: number;
  maxQueueDepth: number;
  prefillInFlight: number;
  prefillWaiting: number;
  prefillMax: number;
  avgDurationS: number;
}
interface StatsResponse {
  backends: StatsBackend[];
  gpuUsed: number;
  gpuBudget: number;
  gpuWaiting: number;
  tier0Inflight: number;
}

// ── history buffers ──────────────────────────────────────────────────────

/** ~5 min at the 500ms tick — the operator's glance unit is the minute. */
export const HISTORY_LEN = 600;
export const WINDOW_S = 60;

export interface Sample {
  t: number;
  inFlight: number;
  waiting: number;
  /** null = no streaming samples yet (ttftSampleCount === 0) */
  ttftMs: number | null;
  ewmaMs: number | null;
  reqRate: number | null;
  bytesRate: number | null;
  tokEstRate: number | null;
  /** engine's own running requests (null = no engine feed) */
  engRunning: number | null;
  /** engine's REAL prefill tokens/s (null = no engine feed) */
  engPrefill: number | null;
  /** engine's REAL decode tokens/s (null = no engine feed) */
  engDecode: number | null;
}

/** Fleet-wide aggregate sample (for the fleet strip). */
export interface FleetSample {
  t: number;
  inFlight: number;
  waiting: number;
  reqRate: number | null;
  ttftMs: number | null;
  tokEstRate: number | null;
  /** sum of REAL engine token rates (null when no engine feed) */
  engPrefill: number | null;
  engDecode: number | null;
}

export function emptySample(t: number): Sample {
  return {
    t,
    inFlight: 0,
    waiting: 0,
    ttftMs: null,
    ewmaMs: null,
    reqRate: null,
    bytesRate: null,
    tokEstRate: null,
    engRunning: null,
    engPrefill: null,
    engDecode: null,
  };
}

// ── store ────────────────────────────────────────────────────────────────

export type FeedKind = "none" | "sse" | "sse-mock" | "stats";

/** A periodic timer handle: number in the browser, Timeout object in Node. */
type StoreTimer = ReturnType<typeof setInterval>;

export interface Feed {
  name: string;
  url: string;
  kind: FeedKind;
  /** false when the endpoint does not exist (404/405), true on real errors. */
  gone: boolean;
}

export interface StoreState {
  /** true once a frame has ever arrived. */
  live: boolean;
  /** live: streaming, degraded: on a fallback feed, offline: no feed. */
  health: "live" | "degraded" | "offline";
  feed: Feed | null;
  frame: Frame | null;
  /** per-backend history, newest last, ≤ HISTORY_LEN samples. */
  hist: Record<string, Sample[]>;
  /** fleet-wide aggregate history, newest last, ≤ HISTORY_LEN. */
  fleetHist: FleetSample[];
  requests: BurstRequest[];
  /** requests that completed in the last 5s. */
  spikeCount: number;
  /** wall time (ms) of the last frame received, null before the first. */
  lastFrameAt: number | null;
  /** true when the last frame is >6s old — the sheet shows STALE data. */
  stale: boolean;
  /** max of spikeCount over the last 60s (evidence does not decay away). */
  spikePeak: number;
}

export function createMetricsStore(on: (s: StoreState) => void) {
  let live = false;
  let health: StoreState["health"] = "offline";
  let feed: Feed | null = null;
  let frame: Frame | null = null;
  const hist: Record<string, Sample[]> = {};
  const fleetHist: FleetSample[] = [];
  let requests: BurstRequest[] = [];
  let spikeCount = 0;
  let lastFrameMs = 0;
  let lastFrameAt: number | null = null;
  let stale = false;
  let spikePeak = 0;
  /** one spikeCount sample per second, ≤ 60 (a minute of evidence). */
  const spikeSamples: number[] = [];

  // feed control
  let es: EventSource | null = null;
  let burstTimer: StoreTimer | null = null;
  let statTimer: StoreTimer | null = null;
  let staleTimer: StoreTimer | null = null;
  let stop = false;
  let tickN = 0;
  let sseGen = 0;

  function emit() {
    on({
      live,
      health,
      feed,
      frame,
      hist: { ...hist },
      fleetHist: fleetHist.slice(),
      requests: requests.slice(-90),
      spikeCount,
      lastFrameAt,
      stale,
      spikePeak,
    });
  }

  function pushFrame(f: Frame, origin: "sse" | "stats" = "sse") {
    live = true;
    frame = f;
    lastFrameMs = Date.now();
    lastFrameAt = Date.now();
    if (stale) stale = false;
    const now = Date.now();
    for (const b of f.backends) {
      let h = hist[b.name];
      if (!h) {
        h = hist[b.name] = [];
      }
      // /stats frames carry no rates/counters — leave them null (gap)
      const hasRate = origin === "sse" && f.t > 0 && f.t - now < 15000;
      const engOk = origin === "sse" && b.engine?.status === "ok";
      h.push({
        t: f.t,
        inFlight: b.inFlight,
        waiting: b.waiting,
        ttftMs: b.ttftSampleCount === 0 ? null : b.ttftMs,
        ewmaMs: b.ewmaMs > 0 ? b.ewmaMs : null,
        reqRate: hasRate ? b.reqRate : null,
        bytesRate: hasRate ? b.bytesRate : null,
        tokEstRate: hasRate ? b.tokEstRate : null,
        engRunning: engOk ? b.engine.running : null,
        engPrefill: engOk ? b.engine.prefillTokS : null,
        engDecode: engOk ? b.engine.decodeTokS : null,
      });
      if (h.length > HISTORY_LEN) h.splice(0, h.length - HISTORY_LEN);
    }
    // drop buffers for backends that vanished
    for (const k of Object.keys(hist)) {
      if (!f.backends.some((b) => b.name === k)) delete hist[k];
    }

    // fleet-wide aggregate sample
    const tVals = f.backends
      .filter((b) => b.ttftSampleCount > 0)
      .map((b) => b.ttftMs);
    const eng = f.backends.filter((b) => b.engine?.status === "ok");
    fleetHist.push({
      t: f.t,
      inFlight: f.totals?.inFlight ?? 0,
      waiting: f.totals?.waiting ?? 0,
      reqRate: origin === "sse" ? f.totals?.reqRate ?? null : null,
      ttftMs: tVals.length
        ? tVals.reduce((a, v) => a + v, 0) / tVals.length
        : null,
      tokEstRate: origin === "sse" ? f.totals?.tokEstRate ?? null : null,
      engPrefill:
        origin === "sse" && eng.length
          ? eng.reduce((a, b) => a + (b.engine?.prefillTokS ?? 0), 0)
          : null,
      engDecode:
        origin === "sse" && eng.length
          ? eng.reduce((a, b) => a + (b.engine?.decodeTokS ?? 0), 0)
          : null,
    });
    if (fleetHist.length > HISTORY_LEN)
      fleetHist.splice(0, fleetHist.length - HISTORY_LEN);
  }

  function setFeed(f: Feed) {
    feed = f;
    health = f.kind === "sse" ? "live" : f.kind === "sse-mock" ? "live" : "degraded";
  }

  function closeEs() {
    if (es) {
      es.close();
      es = null;
    }
  }

  // ── burst tape ─────────────────────────────────────────────────────────

  function burstPath() {
    if (feed?.kind === "sse-mock") return "/dev-metrics/burst";
    return "/metrics/burst";
  }

  async function pollBurst() {
    try {
      const r = await fetch(burstPath(), { cache: "no-store" });
      if (!r.ok) return;
      const j: BurstResponse = await r.json();
      if (Array.isArray(j.requests)) {
        requests = j.requests;
        const cut = Date.now() - 5000;
        spikeCount = requests.filter(
          (q) => q.t > cut && (q.status >= 429 || q.status >= 500),
        ).length;
      }
    } catch {
      /* burst feed is best-effort */
    }
  }

  function startBurst() {
    if (burstTimer) return;
    burstTimer = setInterval(pollBurst, 2000);
    void pollBurst();
  }

  // ── stale flag + spike peak ────────────────────────────────────────────
  // The sheet must tell the operator when the last true number went old.
  // One 1s watch: flags stale >6s after the last frame, and keeps a
  // 60s peak of the spike counter so evidence does not decay away before
  // the operator notices.

  function staleTick() {
    if (stop) return;
    const s = live && lastFrameAt !== null ? Date.now() - lastFrameAt > 6000 : false;
    if (s !== stale) {
      stale = s;
      emit();
    }
    spikeSamples.push(spikeCount);
    if (spikeSamples.length > 60) spikeSamples.shift();
    const peak = Math.max(0, ...spikeSamples);
    if (peak !== spikePeak) {
      spikePeak = peak;
      emit();
    }
  }

  function startStaleWatch() {
    if (staleTimer) return;
    staleTimer = setInterval(staleTick, 1000);
  }

  // ── stats polling fallback ─────────────────────────────────────────────

  function statsToFrame(s: StatsResponse): Frame {
    const t = Date.now();
    const backends: BackendMetrics[] = (s.backends ?? []).map((b) => ({
      name: b.name,
      url: b.url,
      tier: b.tier,
      inFlight: b.inFlight,
      maxConcurrent: b.maxConcurrent,
      waiting: b.waiting,
      maxQueueDepth: b.maxQueueDepth,
      prefillInFlight: b.prefillInFlight,
      prefillWaiting: b.prefillWaiting,
      prefillMax: b.prefillMax,
      avgDurationS: b.avgDurationS,
      ewmaMs: Math.round(b.avgDurationS * 1000),
      ttftMs: 0,
      ttftMsNow: 0,
      ttftSampleCount: 0,
      reqRate: 0,
      bytesRate: 0,
      tokEstRate: 0,
      reqTotal: 0,
      bytesInTotal: 0,
      bytesOutTotal: 0,
      tokEstTotal: 0,
      engine: { ...emptyEngine },
    }));
    const totals = backends.reduce(
      (a, b) => ({
        inFlight: a.inFlight + b.inFlight,
        waiting: a.waiting + b.waiting,
      }),
      { inFlight: 0, waiting: 0 },
    );
    return {
      t,
      backends,
      gpu: {
        used: s.gpuUsed,
        budget: s.gpuBudget,
        waiting: s.gpuWaiting,
        active: s.tier0Inflight > 0,
      },
      tier0Inflight: s.tier0Inflight,
      totals: {
        ...totals,
        reqRate: 0,
        bytesRate: 0,
        tokEstRate: 0,
        reqTotal: 0,
        bytesInTotal: 0,
        bytesOutTotal: 0,
        tokEstTotal: 0,
      },
    };
  }

  async function pollStats() {
    try {
      const r = await fetch("/stats", { cache: "no-store" });
      if (!r.ok) return;
      const j = (await r.json()) as StatsResponse;
      if (!Array.isArray(j.backends)) return;
      pushFrame(statsToFrame(j), "stats");
    } catch {
      /* stats feed is best-effort */
    }
  }

  function startStats() {
    setFeed({ name: "stats", url: "/stats", kind: "stats", gone: false });
    if (statTimer) return;
    statTimer = setInterval(pollStats, 2000);
    void pollStats();
  }

  // ── SSE ladder ─────────────────────────────────────────────────────────

  const streamUrl = (base: string) => `${base}?interval=500ms`;

  // Probe whether an SSE endpoint exists and answers. GET (not HEAD): a
  // GET-only Go handler answers HEAD with 405, which would be read as
  // "gone" and skip the real feed. Abort after ~1.2s — the SSE headers
  // (or the 404/405) arrive before then.
  async function probe(url: string): Promise<"ok" | "gone" | "err"> {
    const ac = new AbortController();
    const timer = setTimeout(() => ac.abort(), 1200);
    try {
      const r = await fetch(url, {
        method: "GET",
        cache: "no-store",
        signal: ac.signal,
        headers: { Accept: "text/event-stream" },
      });
      clearTimeout(timer);
      if (r.status === 404 || r.status === 405) return "gone";
      if (!r.ok) return "err";
      r.body?.cancel(); // release the probe stream
      return "ok";
    } catch {
      clearTimeout(timer);
      return "err";
    }
  }

  function sseTick() {
    tickN++;
    // emit on every frame; extra emit every 4 ticks (2s) so the paused
    // watchdog and tape counters stay fresh even on a silent stream
    if (tickN % 4 === 0) emit();
  }

  function onFrame(raw: string, f: Feed) {
    try {
      const j = JSON.parse(raw) as Frame;
      if (!j || !Array.isArray(j.backends) || typeof j.t !== "number") return;
      setFeed(f);
      pushFrame(j);
      emit();
    } catch {
      /* malformed frame: skip */
    }
  }

  function connectEs(f: Feed, onDrop: () => void) {
    closeEs();
    const src = new EventSource(streamUrl(f.url));
    es = src;
    src.onmessage = (e) => {
      if (es !== src) return;
      onFrame(e.data, f);
      sseTick();
    };
    src.onerror = () => {
      if (es !== src) return;
      // CONNECTING = the browser is auto-retrying (native SSE reconnect) —
      // wait it out. CLOSED = the endpoint died for good — drop to the next
      // feed in the ladder.
      if (src.readyState === EventSource.CLOSED) onDrop();
    };
  }

  function startSse(i: number) {
    if (stop) return;
    const gen = ++sseGen;
    const feeds: Feed[] = [];
    feeds.push({ name: "stream", url: "/metrics/stream", kind: "sse", gone: false });
    if (!import.meta.env.PROD) {
      feeds.push({
        name: "mock",
        url: "/dev-metrics/stream",
        kind: "sse-mock",
        gone: false,
      });
    }
    function tryNext(idx: number) {
      if (stop || gen !== sseGen) return;
      if (idx >= feeds.length) {
        startStats();
        emit();
        return;
      }
      const f = feeds[idx];
      setFeed(f);
      emit();
      // probe existence first so a 404/405 or a dead endpoint skips the
      // feed immediately (dev without the mock route, prod where the
      // route is absent, no router behind the dev proxy)
      void probe(f.url).then((res) => {
        if (stop || gen !== sseGen) return;
        if (res === "gone" || res === "err") {
          f.gone = res === "gone";
          // In dev, a skipped feed is usually a misconfigured mock route —
          // say so once instead of letting the dashboard sit hollow and
          // the operator wonder why.
          if (!import.meta.env.PROD) {
            console.warn(
              `[llm-router] skipped ${f.url} (${res}) — falling through the feed ladder`,
            );
          }
          tryNext(idx + 1);
          return;
        }
        connectEs(f, () => {
          closeEs();
          if (gen === sseGen) tryNext(idx + 1);
        });
      });
    }
    tryNext(i);
  }

  // ── watchdog: a silent SSE feed is dropped; a stats-degraded store
  //    re-probes the primary feed every 30s ──────────────────────────────

  let sseRetry = 0;
  function startWatchdog() {
    setInterval(() => {
      if (stop) return;
      if (feed && (feed.kind === "sse" || feed.kind === "sse-mock")) {
        if (live && Date.now() - lastFrameMs > 6000) {
          closeEs();
          startSse(0);
        }
      } else if (feed?.kind === "stats" && ++sseRetry >= 10) {
        sseRetry = 0;
        startSse(0);
      }
    }, 3000);
  }

  // ── lifecycle ──────────────────────────────────────────────────────────

  function start() {
    if (stop) return;
    startBurst();
    startStaleWatch();
    startSse(0);
    startWatchdog();
  }

  function destroy() {
    stop = true;
    closeEs();
    if (burstTimer) clearInterval(burstTimer);
    if (statTimer) clearInterval(statTimer);
    if (staleTimer) clearInterval(staleTimer);
    burstTimer = null;
    statTimer = null;
    staleTimer = null;
  }

  if (browser) start();

  return { get: emit, destroy };
}
