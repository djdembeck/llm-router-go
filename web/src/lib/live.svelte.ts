// The live feed store. It owns the transport ladder, nothing about layout:
//   1. /metrics/stream (SSE) — production and dev against a router
//   2. /dev-metrics/stream (SSE) — dev-only mock when no router is up
//   3. /stats (polling) — last-resort admission snapshot
// Frames are full snapshots. History lives on the server; this store keeps
// one frame per second (a short live tail for the profile's right edge).

import { browser } from "$app/env";
import {
  emptyEngine,
  getJSON,
  type BurstRequest,
  type Frame,
  type SessionsFeed,
} from "./api";

export type FeedKind = "none" | "sse" | "sse-mock" | "stats";

export interface FeedInfo {
  name: string;
  url: string;
  kind: FeedKind;
  gone: boolean;
}

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

const LIVE_FRAMES_CAP = 900;

export class LiveStore {
  frame = $state<Frame | null>(null);
  health = $state<"live" | "degraded" | "offline">("offline");
  feed = $state<FeedInfo | null>(null);
  lastFrameAt = $state<number | null>(null);
  stale = $state(false);
  paused = $state(false);
  sessions = $state<SessionsFeed | null>(null);
  /** true once a poll succeeded and a later one failed; the kept feed is not current */
  sessionsStale = $state(false);
  requests = $state<BurstRequest[]>([]);
  spikeCount = $state(0);
  spikePeak = $state(0);
  /** bumped once per second when a frame is appended to the live tail */
  framesVersion = $state(0);

  /** one frame per second, oldest first; plain array (version signals reads) */
  liveFrames: Frame[] = [];

  private es: EventSource | null = null;
  private burstTimer: number | null = null;
  private sessionsTimer: number | null = null;
  private staleTimer: number | null = null;
  private watchdogTimer: number | null = null;
  private statsTimeout: number | null = null;
  private statsActive = false;
  private started = false;
  private stop = false;
  private sseGen = 0;
  private lastSec = 0;
  private lastFrameMs = 0;
  private spikeSamples: number[] = [];
  private backendSig = "";
  private sseRetry = 0;
  private sseStartMs = 0;

  start(): void {
    if (!browser || this.started) return;
    this.started = true;
    this.startBurst();
    this.startSessions();
    this.startStaleWatch();
    this.startSse(0);
    this.startWatchdog();
  }

  destroy(): void {
    this.stop = true;
    this.closeEs();
    this.stopStats();
    clearInterval(this.burstTimer ?? undefined);
    clearInterval(this.sessionsTimer ?? undefined);
    clearInterval(this.staleTimer ?? undefined);
    clearInterval(this.watchdogTimer ?? undefined);
    clearTimeout(this.statsTimeout ?? undefined);
    this.burstTimer = null;
    this.sessionsTimer = null;
    this.staleTimer = null;
    this.watchdogTimer = null;
    this.statsTimeout = null;
  }

  togglePause(): void {
    this.paused = !this.paused;
    if (!this.paused) {
      this.lastFrameMs = Date.now();
      this.lastFrameAt = Date.now();
      this.stale = false;
    }
  }

  // ── feed ────────────────────────────────────────────────────────────────

  private setFeed(f: FeedInfo): void {
    this.feed = f;
    this.health = f.kind === "stats" ? "degraded" : "live";
  }

  private closeEs(): void {
    if (this.es) {
      this.es.close();
      this.es = null;
    }
  }

  private onFrame(raw: string, f: FeedInfo): void {
    if (this.paused) return;
    let j: Frame;
    try {
      j = JSON.parse(raw) as Frame;
    } catch {
      return;
    }
    if (!j || !Array.isArray(j.backends) || typeof j.t !== "number") return;
    if (f.kind !== "stats") this.stopStats();
    const sig = j.backends.map((b) => b.name).sort().join("|");
    if (this.backendSig && sig !== this.backendSig) {
      // Different fleet (e.g. router → dev mock, or a reconfigured backend
      // set). Never leave the previous feed's tail/sessions behind.
      this.liveFrames = [];
      this.lastSec = 0;
      this.framesVersion++;
      this.sessions = null;
      this.sessionsStale = false;
      this.requests = [];
      this.spikeCount = 0;
      this.spikePeak = 0;
      this.spikeSamples = [];
    }
    this.backendSig = sig;
    this.setFeed(f);
    this.frame = j;
    this.lastFrameMs = Date.now();
    this.lastFrameAt = this.lastFrameMs;
    this.stale = false;
    const sec = Math.floor(j.t / 1000);
    if (sec !== this.lastSec) {
      this.lastSec = sec;
      this.liveFrames.push(j);
      if (this.liveFrames.length > LIVE_FRAMES_CAP) this.liveFrames.shift();
      this.framesVersion++;
    }
  }

  private connectEs(f: FeedInfo, onDrop: () => void): void {
    this.closeEs();
    const src = new EventSource(`${f.url}?interval=500ms`);
    this.es = src;
    src.onmessage = (e) => {
      if (this.es !== src) return;
      this.onFrame(e.data, f);
    };
    src.onerror = () => {
      if (this.es !== src) return;
      // CONNECTING = native retry is in flight; CLOSED = the endpoint is
      // gone for good, so fall through the ladder.
      if (src.readyState === EventSource.CLOSED) onDrop();
    };
  }

  private async probe(url: string): Promise<"ok" | "gone" | "err"> {
    const ac = new AbortController();
    const timer = window.setTimeout(() => ac.abort(), 1200);
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
      r.body?.cancel();
      return "ok";
    } catch {
      clearTimeout(timer);
      return "err";
    }
  }

  private startSse(i: number): void {
    if (this.stop) return;
    const gen = ++this.sseGen;
    // Connect deadline anchor: the watchdog also times out when the
    // connection never delivered a single frame (lastFrameMs is 0 until
    // the first valid frame, so it cannot serve as an anchor on its own).
    this.sseStartMs = Date.now();
    const feeds: FeedInfo[] = [
      { name: "stream", url: "/metrics/stream", kind: "sse", gone: false },
    ];
    if (!import.meta.env.PROD) {
      feeds.push({
        name: "mock",
        url: "/dev-metrics/stream",
        kind: "sse-mock",
        gone: false,
      });
    }
    const tryNext = (idx: number): void => {
      if (this.stop || gen !== this.sseGen) return;
      if (idx >= feeds.length) {
        this.startStats();
        return;
      }
      const f = feeds[idx];
      this.setFeed(f);
      void this.probe(f.url).then((res) => {
        if (this.stop || gen !== this.sseGen) return;
        if (res !== "ok") {
          f.gone = res === "gone";
          tryNext(idx + 1);
          return;
        }
        this.connectEs(f, () => {
          this.closeEs();
          if (gen === this.sseGen) tryNext(idx + 1);
        });
      });
    };
    tryNext(i);
  }

  private startWatchdog(): void {
    if (this.watchdogTimer) return;
    this.watchdogTimer = window.setInterval(() => {
      if (this.stop) return;
      if (this.feed && (this.feed.kind === "sse" || this.feed.kind === "sse-mock")) {
        // Anchor to the newer of last frame and this connection's start:
        // a stale lastFrameMs from a prior connection must not eat the new
        // connection's connect window (a late first frame gets 6s).
        const anchor = Math.max(this.lastFrameMs, this.sseStartMs);
        if (!this.paused && anchor && Date.now() - anchor > 6000) {
          this.closeEs();
          this.startSse(0);
        }
      } else if (this.feed?.kind === "stats" && ++this.sseRetry >= 10) {
        this.sseRetry = 0;
        this.startSse(0);
      }
    }, 3000);
  }

  // ── sessions + burst ────────────────────────────────────────────────────

  private async fetchSessions(): Promise<SessionsFeed | null> {
    const first = await getJSON<SessionsFeed>("/metrics/sessions");
    if (first && Array.isArray(first.live) && Array.isArray(first.sessions)) return first;
    if (!import.meta.env.PROD) {
      const mock = await getJSON<SessionsFeed>("/dev-metrics/sessions");
      if (mock && Array.isArray(mock.live) && Array.isArray(mock.sessions)) return mock;
    }
    return null;
  }

  private async pollSessions(): Promise<void> {
    if (this.paused) return;
    const j = await this.fetchSessions();
    if (j) {
      this.sessions = j;
      this.sessionsStale = false;
    } else if (this.sessions !== null) {
      // A successful poll preceded this one; a failed follow-up must not
      // present the previous feed as current. Keep it, flagged stale.
      this.sessionsStale = true;
    }
  }

  private startSessions(): void {
    if (this.sessionsTimer) return;
    this.sessionsTimer = window.setInterval(() => void this.pollSessions(), 1000);
    void this.pollSessions();
  }

  private async pollBurst(): Promise<void> {
    if (this.paused) return;
    let j = await getJSON<{ requests: BurstRequest[] }>("/metrics/burst?limit=500");
    if ((!j || !Array.isArray(j.requests)) && !import.meta.env.PROD) {
      j = await getJSON<{ requests: BurstRequest[] }>("/dev-metrics/burst");
    }
    if (!j || !Array.isArray(j.requests)) return;
    this.requests = j.requests;
    const cut = Date.now() - 5000;
    this.spikeCount = j.requests.filter((q) => q.t > cut && q.status >= 400).length;
  }

  private startBurst(): void {
    if (this.burstTimer) return;
    this.burstTimer = window.setInterval(() => void this.pollBurst(), 2000);
    void this.pollBurst();
  }

  // ── stale + spike peak ──────────────────────────────────────────────────

  private startStaleWatch(): void {
    if (this.staleTimer) return;
    this.staleTimer = window.setInterval(() => {
      if (this.stop) return;
      const s =
        !this.paused && this.lastFrameAt !== null
          ? Date.now() - this.lastFrameAt > 6000
          : false;
      if (s !== this.stale) this.stale = s;
      this.spikeSamples.push(this.spikeCount);
      if (this.spikeSamples.length > 60) this.spikeSamples.shift();
      const peak = Math.max(0, ...this.spikeSamples);
      if (peak !== this.spikePeak) this.spikePeak = peak;
    }, 1000);
  }

  // ── stats fallback ──────────────────────────────────────────────────────

  private statsToFrame(s: StatsResponse): Frame {
    const t = Date.now();
    const backends = (s.backends ?? []).map((b) => ({
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
      ttftSampleCount: 0,
      ttftMsNow: 0,
      reqRate: 0,
      bytesRate: 0,
      tokEstRate: 0,
      reqTotal: 0,
      rejectTotal: 0,
      bytesInTotal: 0,
      bytesOutTotal: 0,
      tokEstTotal: 0,
      engine: { ...emptyEngine },
    }));
    const inFlight = backends.reduce((a, b) => a + b.inFlight, 0);
    const waiting = backends.reduce((a, b) => a + b.waiting, 0);
    return {
      t,
      backends,
      gpu: {
        used: s.gpuUsed,
        budget: s.gpuBudget,
        waiting: s.gpuWaiting,
        active: s.tier0Inflight > 0,
      },
      gpuDevices: null,
      gpuFeed: { status: "", url: "" },
      tier0Inflight: s.tier0Inflight,
      totals: {
        inFlight,
        waiting,
        reqRate: 0,
        bytesRate: 0,
        tokEstRate: 0,
        reqTotal: 0,
        rejectTotal: 0,
        bytesInTotal: 0,
        bytesOutTotal: 0,
        tokEstTotal: 0,
        ttftMs: 0,
        ttftSampleCount: 0,
        engineOk: 0,
        engineRunning: 0,
        engineWaiting: 0,
        prefillTokS: 0,
        prefillCacheTokS: 0,
        decodeTokS: 0,
        kvPeak: 0,
        kvUsedTok: 0,
        kvCapTok: 0,
        kvMemGB: 0,
        weightMemGB: 0,
        hitRate: 0,
      },
    };
  }

  private async pollStats(): Promise<void> {
    if (this.paused) return;
    const j = await getJSON<StatsResponse>("/stats");
    if (!j || !Array.isArray(j.backends)) return;
    const f = this.statsToFrame(j);
    this.frame = f;
    this.lastFrameMs = Date.now();
    this.lastFrameAt = this.lastFrameMs;
    this.stale = false;
  }

  private stopStats(): void {
    this.statsActive = false;
    clearTimeout(this.statsTimeout ?? 0);
    this.statsTimeout = null;
  }

  private startStats(): void {
    if (this.stop) return;
    // The SSE watchdog re-probes every 30s while this fallback is active;
    // a failed probe lands here again. Refresh the transport first (the
    // failed probe set it to sse), then refuse a duplicate poll chain.
    this.setFeed({ name: "stats", url: "/stats", kind: "stats", gone: false });
    if (this.statsActive) return;
    this.statsActive = true;
    if (!this.burstTimer) {
      this.burstTimer = window.setInterval(() => void this.pollBurst(), 3000);
    }
    if (!this.sessionsTimer) {
      this.sessionsTimer = window.setInterval(() => void this.pollSessions(), 1500);
    }
    const poll = (): void => {
      if (this.stop || !this.statsActive) return;
      void this.pollStats();
      this.statsTimeout = window.setTimeout(poll, 2000);
    };
    void this.pollStats();
    this.statsTimeout = window.setTimeout(poll, 2000);
  }
}
