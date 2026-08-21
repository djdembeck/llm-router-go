// DEV-ONLY mock metrics synthesizer. Shared by the dev-only
// +server.ts routes (stream + burst); no imports, so nothing leaks
// into client bundles. adapter-static prunes it from the production
// build because only those dev-only routes import it.
//
// Synthesizes the EXACT contract frame shape
// (.impeccable/metrics-contract.md) with plausible random-walk traffic:
// two backends — one tier-0 "king" and one tier-1 "subject" — with an
// occasional saturation spike on the subject that produces 429s,
// streaming TTFTs, and burst spikes.
//
// State lives at module scope: one process per dev server.

import type {
  EngineMetrics,
  LiveReq,
  SessionReq,
  SessionsFeed,
} from "$lib/metrics.js";

interface MockBackend {
  name: string;
  url: string;
  tier: number;
  maxConcurrent: number;
  maxQueueDepth: number;
  prefillMax: number;
  inFlight: number;
  waiting: number;
  prefillInFlight: number;
  prefillWaiting: number;
  ewmaMs: number;
  ttftMs: number;
  ttftMsNow: number;
  ttftSampleCount: number;
  reqRate: number;
  bytesRate: number;
  tokEstRate: number;
  reqTotal: number;
  bytesInTotal: number;
  bytesOutTotal: number;
  tokEstTotal: number;
  spikeTicks: number;
  // engine's own /metrics truth (simulated)
  engRunning: number;
  engWaiting: number;
  engKV: number;
  engPrefill: number;
  engDecode: number;
  engTTFT: number;
}

const BACKENDS: MockBackend[] = [
  {
    name: "vllm-king",
    url: "http://vllm-1:8000",
    tier: 0,
    maxConcurrent: 4,
    maxQueueDepth: 2,
    prefillMax: 2,
    inFlight: 2,
    waiting: 0,
    prefillInFlight: 1,
    prefillWaiting: 0,
    ewmaMs: 9200,
    ttftMs: 240,
    ttftMsNow: 236,
    ttftSampleCount: 40,
    reqRate: 1.1,
    bytesRate: 52000,
    tokEstRate: 2400,
    reqTotal: 1284,
    bytesInTotal: 2600000,
    bytesOutTotal: 183000000,
    tokEstTotal: 4210000,
    spikeTicks: 0,
    engRunning: 2,
    engWaiting: 0,
    engKV: 42,
    engPrefill: 2600,
    engDecode: 310,
    engTTFT: 235,
  },
  {
    name: "sglang-subject",
    url: "http://sglang-2:8000",
    tier: 1,
    maxConcurrent: 3,
    maxQueueDepth: 2,
    prefillMax: 1,
    inFlight: 1,
    waiting: 0,
    prefillInFlight: 0,
    prefillWaiting: 0,
    ewmaMs: 14800,
    ttftMs: 610,
    ttftMsNow: 590,
    ttftSampleCount: 22,
    reqRate: 0.6,
    bytesRate: 31000,
    tokEstRate: 1500,
    reqTotal: 512,
    bytesInTotal: 990000,
    bytesOutTotal: 74000000,
    tokEstTotal: 1810000,
    spikeTicks: 0,
    engRunning: 1,
    engWaiting: 0,
    engKV: 58,
    engPrefill: 1500,
    engDecode: 190,
    engTTFT: 590,
  },
];

const clamp = (v: number, lo: number, hi: number) =>
  Math.min(hi, Math.max(lo, v));
const walk = (v: number, step: number, lo: number, hi: number) =>
  clamp(v + (Math.random() * 2 - 1) * step, lo, hi);

// request ring, newest last — mirrors the server's bounded burst ring
export interface BurstReq {
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
const BURST_CAP = 500;
let ring: BurstReq[] = [];
const T0 = Date.now();

function spawnRequest(t: number): void {
  const b = Math.random() < 0.62 ? BACKENDS[0] : BACKENDS[1];
  const stream = Math.random() < 0.8;
  const tokEst = Math.round(64 + Math.random() * (b.tier === 0 ? 2200 : 4200));
  let status = 200;
  // the subject saturates occasionally → the router rejects with 429
  if (b.spikeTicks > 0 && Math.random() < 0.5) status = 429;
  if (Math.random() < 0.03) status = 502;
  const rejected = status !== 200;
  const ttftMs =
    rejected || !stream ? 0 : Math.round(b.ttftMs * (0.7 + Math.random() * 0.7));
  const durMs = rejected
    ? Math.round(300 + Math.random() * 900)
    : Math.round(b.ewmaMs * (0.6 + Math.random() * 0.9));
  const bytesIn = Math.max(256, Math.round(tokEst * 4));
  const bytesOut = rejected
    ? Math.round(120 + Math.random() * 300)
    : Math.round(durMs * (8 + Math.random() * 10));
  ring.push({
    t,
    backend: b.name,
    path: "/v1/chat/completions",
    status,
    stream,
    durMs,
    ttftMs,
    newTokensEst: rejected ? 0 : tokEst,
    bytesIn,
    bytesOut,
  });
  if (ring.length > BURST_CAP) ring.splice(0, ring.length - BURST_CAP);
  if (!rejected) {
    b.reqTotal++;
    b.bytesInTotal += bytesIn;
    b.bytesOutTotal += bytesOut;
    b.tokEstTotal += tokEst;
  }
}

// pre-seed a little history so the tape is not empty on first paint
for (let i = 60; i > 0; i--) spawnRequest(T0 - i * 1900);

export interface Frame {
  t: number;
  backends: Array<Record<string, unknown>>;
  gpu: { used: number; budget: number; waiting: number; active: boolean };
  tier0Inflight: number;
  totals: Record<string, unknown>;
}

function tick(): Frame {
  const now = Date.now();
  // subject saturation spike: a burst that fills its slots and produces
  // 429s, decaying over ~6 ticks
  if (BACKENDS[1].spikeTicks > 0) BACKENDS[1].spikeTicks--;
  else if (Math.random() < 0.02) BACKENDS[1].spikeTicks = 5;

  for (let i = 0; i < BACKENDS.length; i++) {
    const b = BACKENDS[i];
    const spike = b.spikeTicks > 0;
    const cap = b.maxConcurrent;
    b.inFlight = Math.round(walk(b.inFlight, spike ? 1.4 : 0.8, 0, cap));
    b.waiting = spike
      ? Math.round(walk(b.waiting, 1.2, 1, b.maxQueueDepth))
      : Math.random() < 0.15 ? 1 : 0;
    b.prefillInFlight = Math.round(walk(b.prefillInFlight, 0.6, 0, b.prefillMax));
    b.prefillWaiting = Math.random() < (spike ? 0.4 : 0.08) ? 1 : 0;
    b.ewmaMs = Math.round(walk(b.ewmaMs, 220, 3000, 26000));
    // ttftMsNow is the most recent first-byte sample; the EWMA trails it
    b.ttftMsNow = Math.round(walk(b.ttftMsNow, 60, 80, 1600));
    b.ttftMs = Math.round(0.7 * b.ttftMs + 0.3 * b.ttftMsNow);
    b.ttftSampleCount = clamp(
      b.ttftSampleCount + (Math.random() < 0.7 ? 1 : 0),
      0,
      400,
    );
    b.reqRate = +walk(b.reqRate, spike ? 1.6 : 0.5, 0, spike ? 9 : 2.6).toFixed(2);
    b.bytesRate = Math.round(walk(b.bytesRate, spike ? 9000 : 4000, 2000, 160000));
    b.tokEstRate = Math.round(walk(b.tokEstRate, spike ? 700 : 300, 0, 9000));
    // engine truth follows the traffic: KV climbs under saturation, decode
    // throughput tracks in-flight, prefill bursts with arrivals.
    b.engRunning = clamp(b.inFlight, 0, b.maxConcurrent + 2);
    b.engWaiting = b.waiting + (spike ? 1 : 0);
    b.engKV = Math.round(walk(b.engKV, spike ? 6 : 2, 15, spike ? 99 : 88));
    b.engPrefill = Math.round(walk(b.engPrefill, spike ? 900 : 350, 0, 12000));
    b.engDecode = Math.round(b.engRunning * walk(b.engDecode / Math.max(1, b.engRunning), 25, 80, 220));
    b.engTTFT = Math.round(walk(b.engTTFT, 45, 90, 1400));
    stepEngineV2(i, spike);
    const n = Math.random() < b.reqRate * 0.5
      ? Math.round(b.reqRate * 0.5) + 1
      : 0;
    for (let i = 0; i < n; i++) spawnRequest(now);
  }

  const tier0 = BACKENDS.filter((b) => b.tier === 0);
  const gpuActive = tier0.some((b) => b.inFlight > 0);
  const gpuUsed = gpuActive ? clamp(tier0[0].inFlight, 0, 4) : 0;

  const backends = BACKENDS.map((b, i) => ({
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
    avgDurationS: +(b.ewmaMs / 1000).toFixed(2),
    ewmaMs: b.ewmaMs,
    ttftMs: b.ttftMs,
    ttftMsNow: b.ttftMsNow,
    ttftSampleCount: b.ttftSampleCount,
    reqRate: b.reqRate,
    bytesRate: b.bytesRate,
    tokEstRate: b.tokEstRate,
    reqTotal: b.reqTotal,
    bytesInTotal: b.bytesInTotal,
    bytesOutTotal: b.bytesOutTotal,
    tokEstTotal: b.tokEstTotal,
    engine: engineV2(i),
  }));

  return {
    t: now,
    backends,
    gpu: {
      used: gpuUsed,
      budget: 4,
      waiting: BACKENDS[1].waiting > 0 && gpuActive ? 1 : 0,
      active: gpuActive,
    },
    tier0Inflight: tier0.reduce((a, b) => a + b.inFlight, 0),
    totals: {
      inFlight: BACKENDS.reduce((a, b) => a + b.inFlight, 0),
      waiting: BACKENDS.reduce((a, b) => a + b.waiting, 0),
      reqRate: +BACKENDS.reduce((a, b) => a + b.reqRate, 0).toFixed(2),
      bytesRate: BACKENDS.reduce((a, b) => a + b.bytesRate, 0),
      tokEstRate: BACKENDS.reduce((a, b) => a + b.tokEstRate, 0),
      reqTotal: BACKENDS.reduce((a, b) => a + b.reqTotal, 0),
      bytesInTotal: BACKENDS.reduce((a, b) => a + b.bytesInTotal, 0),
      bytesOutTotal: BACKENDS.reduce((a, b) => a + b.bytesOutTotal, 0),
      tokEstTotal: BACKENDS.reduce((a, b) => a + b.tokEstTotal, 0),
    },
  };
}

/** Advance the simulation one frame and return it. */
export function mockTick(): Frame {
  return tick();
}

// ── engine /metrics v2 (simulated, per-backend family) ───────────────────
// The family follows the backend name: "sglang" → sglang metrics shape,
// anything else → vllm. State is module-scoped so the walk is continuous
// across ticks; vllm-only and sglang-only fields are null per family.

interface EngineV2State {
  family: "vllm" | "sglang";
  // shared
  itl: number;
  meanPrompt: number;
  meanGen: number;
  reqDoneTotal: number;
  promptTokTotal: number;
  genTokTotal: number;
  hitRate: number;
  // vllm
  hitQueries: number;
  hitHits: number;
  mmQueries: number;
  mmHits: number;
  finStop: number;
  finLength: number;
  finAbort: number;
  finError: number;
  preempted: number;
  prefillMs: number;
  decodeMs: number;
  perTokMs: number;
  queueMs: number;
  waitCap: number;
  waitDefer: number;
  // sglang
  fullPct: number;
  swaPct: number;
  mambaPct: number;
  kvCapTok: number;
  kvUsedTok: number;
  mambaCapTok: number;
  mambaUsedTok: number;
  retracted: number;
  retractedTokS: number;
  abortedTotal: number;
  streamDone: number;
  nonStreamDone: number;
  ttftStream: number;
  ttftNonStream: number;
  sloCap: number;
  ctxLen: number;
  weightGB: number;
}

const ENG: EngineV2State[] = BACKENDS.map((b, i) => {
  const sg = b.name.toLowerCase().includes("sglang");
  return {
    family: sg ? "sglang" : "vllm",
    itl: sg ? 22 : 18,
    meanPrompt: sg ? 900 : 700,
    meanGen: sg ? 520 : 380,
    reqDoneTotal: sg ? 4120 : 9800,
    promptTokTotal: sg ? 3.4e6 : 6.4e6,
    genTokTotal: sg ? 1.6e6 : 2.9e6,
    hitRate: 0.3 + Math.random() * 0.3,
    hitQueries: 2.1e6,
    hitHits: 0.9e6,
    mmQueries: 8400,
    mmHits: 2100,
    finStop: 8200,
    finLength: 1300,
    finAbort: 140,
    finError: 21,
    preempted: 6,
    prefillMs: 240,
    decodeMs: 4100,
    perTokMs: 16,
    queueMs: 90,
    waitCap: 0,
    waitDefer: 0,
    fullPct: 55,
    swaPct: 12,
    mambaPct: 22,
    kvCapTok: 2400000 + i * 100000,
    kvUsedTok: 1300000,
    mambaCapTok: 512,
    mambaUsedTok: 118,
    retracted: 0,
    retractedTokS: 0,
    abortedTotal: 9,
    streamDone: 3300,
    nonStreamDone: 820,
    ttftStream: 480,
    ttftNonStream: 720,
    sloCap: 32 + (i % 2) * 32,
    ctxLen: 32768 + (i % 2) * 32768,
    weightGB: 12.1,
  };
});

function stepEngineV2(i: number, spike: boolean): void {
  const e = ENG[i];
  const b = BACKENDS[i];
  const done = Math.round(b.reqRate * (0.6 + Math.random() * 0.5));
  e.reqDoneTotal += done;
  e.promptTokTotal += Math.round(done * walk(e.meanPrompt, 60, 200, 3200));
  e.genTokTotal += Math.round(done * walk(e.meanGen, 40, 80, 1200));
  e.itl = +walk(e.itl, 1.4, 8, 40).toFixed(1);
  e.hitRate = +walk(e.hitRate, 0.03, 0.3, 0.6).toFixed(3);
  e.meanPrompt = Math.round(walk(e.meanPrompt, 60, 200, 3200));
  e.meanGen = Math.round(walk(e.meanGen, 40, 80, 1200));
  e.queueMs = Math.round(walk(e.queueMs, spike ? 60 : 22, 15, 900));
  if (e.family === "vllm") {
    e.hitQueries += Math.round(walk(400, 90, 50, 2400));
    e.hitHits = Math.round(e.hitQueries * walk(0.45, 0.02, 0.3, 0.6));
    e.mmQueries = Math.round(walk(e.mmQueries, 40, 5000, 60000));
    e.mmHits = Math.round(e.mmQueries * walk(0.24, 0.02, 0.1, 0.4));
    const f = Math.random();
    if (f < 0.72) e.finStop += done;
    else if (f < 0.92) e.finLength += done;
    else if (f < 0.98) e.finAbort += done;
    else e.finError += done;
    if (Math.random() < 0.03) e.preempted++;
    e.prefillMs = Math.round(walk(e.prefillMs, 30, 80, 1400));
    e.decodeMs = Math.round(walk(e.decodeMs, 220, 900, 15000));
    e.perTokMs = +walk(e.perTokMs, 1.2, 6, 40).toFixed(1);
    e.waitCap = b.engWaiting > 0 ? (Math.random() < 0.7 ? b.engWaiting : 0) : 0;
    e.waitDefer = b.engWaiting > 0 ? (Math.random() < 0.3 ? 1 : 0) : 0;
  } else {
    e.fullPct = Math.round(walk(e.fullPct, spike ? 5 : 1.8, 15, 98));
    e.swaPct = Math.round(walk(e.swaPct, 1.4, 3, 45));
    e.mambaPct = Math.round(walk(e.mambaPct, 2.2, 5, 40));
    e.kvUsedTok = Math.round(
      (e.kvCapTok * e.fullPct) / 100 + (Math.random() * 2 - 1) * 40000,
    );
    e.kvUsedTok = clamp(e.kvUsedTok, 0, e.kvCapTok);
    e.mambaUsedTok = Math.round(walk(e.mambaUsedTok, 6, 20, 480));
    e.retracted = Math.round(walk(e.retracted, spike ? 1.2 : 0.6, 0, 3));
    e.retractedTokS =
      e.retracted === 0 ? 0 : Math.round(walk(e.retractedTokS, 120, 400, 6000));
    if (Math.random() < 0.02) e.abortedTotal++;
    const s = Math.round(done * 0.8);
    e.streamDone += s;
    e.nonStreamDone += done - s;
    e.ttftStream = Math.round(walk(e.ttftStream, 40, 120, 1200));
    e.ttftNonStream = Math.round(walk(e.ttftNonStream, 40, e.ttftStream + 80, 1800));
    e.ctxLen = Math.round(walk(e.ctxLen, 2048, 8192, 131072) / 8192) * 8192;
  }
}

function engineV2(i: number): EngineMetrics {
  const b = BACKENDS[i];
  const e = ENG[i];
  const sg = e.family === "sglang";
  const nRun = Math.max(1, b.engRunning);
  const perReq = b.engDecode / nRun;
  return {
    status: "ok",
    engine: e.family,
    running: b.engRunning,
    waiting: b.engWaiting,
    kvPct: b.engKV,
    hitRate: e.hitRate,
    prefillTokS: Math.round(b.engPrefill * (1 - e.hitRate)),
    prefillCacheTokS: Math.round(b.engPrefill * e.hitRate),
    decodeTokS: b.engDecode,
    ttftMs: b.engTTFT,
    itlMs: e.itl,
    e2eMs: Math.round(
      b.engTTFT + (e.meanGen / Math.max(1, perReq)) * 1000,
    ),
    queueMs: e.queueMs,
    meanPromptTok: e.meanPrompt,
    meanGenTok: e.meanGen,
    reqDoneTotal: e.reqDoneTotal,
    waitCap: sg ? null : e.waitCap,
    waitDefer: sg ? null : e.waitDefer,
    retracted: sg ? e.retracted : null,
    retractedTokS: sg ? e.retractedTokS : null,
    fullPct: sg ? e.fullPct : null,
    swaPct: sg ? e.swaPct : null,
    mambaPct: sg ? e.mambaPct : null,
    kvUsedTok: sg ? e.kvUsedTok : null,
    kvCapTok: sg ? e.kvCapTok : null,
    kvFreeTok: sg ? e.kvCapTok - e.kvUsedTok : null,
    kvEvictTok: sg ? Math.round(e.kvUsedTok * 0.06) : null,
    mambaUsedTok: sg ? e.mambaUsedTok : null,
    mambaCapTok: sg ? e.mambaCapTok : null,
    hicacheHostUsedTok: null,
    hicacheHostCapTok: null,
    ttftStreamMs: sg ? e.ttftStream : null,
    ttftNonStreamMs: sg ? e.ttftNonStream : null,
    prefillMs: sg ? null : e.prefillMs,
    decodeMs: sg ? null : e.decodeMs,
    perTokMs: sg ? null : e.perTokMs,
    kvMemGB: sg ? 28.4 : null,
    weightMemGB: sg ? e.weightGB : null,
    sloCap: sg ? e.sloCap : null,
    ctxLen: sg ? e.ctxLen : null,
    preemptedTotal: sg ? null : e.preempted,
    abortedTotal: sg ? e.abortedTotal : null,
    streamDoneTotal: sg ? e.streamDone : null,
    nonStreamDoneTotal: sg ? e.nonStreamDone : null,
    promptTokTotal: e.promptTokTotal,
    genTokTotal: e.genTokTotal,
    hitQueriesTotal: sg ? null : e.hitQueries,
    hitHitsTotal: sg ? null : e.hitHits,
    mmQueriesTotal: sg ? null : e.mmQueries,
    mmHitsTotal: sg ? null : e.mmHits,
    finReasons: sg
      ? null
      : {
          stop: e.finStop,
          length: e.finLength,
          abort: e.finAbort,
          error: e.finError,
        },
  };
}

// ── session feed (simulated conversations) ───────────────────────────────

interface MockTurn {
  tMs: number;
  status: number;
  stream: boolean;
  durMs: number;
  ttftMs: number;
  ctxTok: number;
  newTok: number;
  cachedTok: number;
  tokOut: number;
}

interface MockConv {
  id: string;
  backend: string;
  ctxTok: number; // running context size after the last turn
  nextTurnAt: number;
  liveStart: number | null; // in-flight request start, or null
  liveDurMs: number; // how long the live request runs before completing
  liveNewTok: number;
  turns: MockTurn[];
}

const hexId = () =>
  Array.from(
    { length: 12 },
    () => "0123456789abcdef"[Math.floor(Math.random() * 16)],
  ).join("");

let liveReqId = 1000;

function makeTurn(tMs: number, convCtx: number, stream = Math.random() < 0.8): MockTurn {
  const ctxTok = Math.round(convCtx * (0.85 + Math.random() * 0.15));
  const newTok = Math.round(100 + Math.random() * 800);
  const cachedTok = Math.max(0, ctxTok - newTok);
  const roll = Math.random();
  const status = roll < 0.04 ? 429 : roll < 0.055 ? 500 : 200;
  const rejected = status !== 200;
  const ttftMs =
    rejected || !stream
      ? 0
      : Math.round(40 + ctxTok / 60 + (Math.random() * 2 - 1) * 25);
  const tokOut = rejected ? 0 : Math.round(60 + Math.random() * 540);
  const durMs = rejected
    ? Math.round(300 + Math.random() * 600)
    : Math.round(ttftMs + tokOut / 8);
  return {
    tMs,
    status,
    stream,
    durMs,
    ttftMs,
    ctxTok,
    newTok,
    cachedTok,
    tokOut,
  };
}

const growCtx = (c: MockConv, turn: MockTurn) => {
  c.ctxTok = Math.round(Math.max(c.ctxTok, turn.ctxTok) * (1.02 + Math.random() * 0.04));
};

const CONVS: MockConv[] = Array.from(
  { length: 4 + Math.floor(Math.random() * 3) },
  (_, i) => {
    const c: MockConv = {
      id: hexId(),
      backend: BACKENDS[i % BACKENDS.length].name,
      ctxTok: Math.round(400 + Math.random() * 1800),
      nextTurnAt: 0,
      liveStart: null,
      liveDurMs: 0,
      liveNewTok: 0,
      turns: [],
    };
    // seed 3–7 historical turns at 8–40s gaps, ending ≤ 100s ago
    const nTurns = 3 + Math.floor(Math.random() * 5);
    let t = T0 - (nTurns * 14 + 100) * 1000;
    for (let k = 0; k < nTurns; k++) {
      const turn = makeTurn(t, c.ctxTok);
      c.turns.push(turn);
      growCtx(c, turn);
      t += (8 + Math.random() * 32) * 1000;
    }
    c.nextTurnAt = t + (20 + Math.random() * 90) * 1000;
    return c;
  },
);

// a few lone live requests that are not part of a conversation
const LONE_LIVE = [
  { backend: BACKENDS[0].name, start: T0 - 6500, stream: true, path: "/v1/chat/completions" },
  { backend: BACKENDS[1].name, start: T0 - 2400, stream: false, path: "/v1/completions" },
].map((l) => ({
  ...l,
  id: hexId(),
  ctxTok: Math.round(300 + Math.random() * 2200),
  newTok: Math.round(100 + Math.random() * 900),
}));

const convReqs = (c: MockConv): SessionReq[] =>
  c.turns.map((t) => ({
    tMs: Math.round(t.tMs),
    status: t.status,
    stream: t.stream,
    durMs: t.durMs,
    ttftMs: t.ttftMs,
    ctxTok: t.ctxTok,
    newTok: t.newTok,
    cachedTok: t.cachedTok,
    tokOut: t.tokOut,
    tokS: t.durMs > 0 ? +(t.tokOut / (t.durMs / 1000)).toFixed(1) : 0,
  }));

/** Build the simulated session feed (same walk state as the frame feed). */
export function synthSessions(nowMs: number): SessionsFeed {
  for (const c of CONVS) {
    // advance the in-flight conversation's live request
    if (c.liveStart === null && Math.random() < 0.12) {
      c.liveStart = nowMs - (3000 + Math.random() * 27000);
      c.liveDurMs = 15000 + Math.random() * 20000;
      c.liveNewTok = Math.round(100 + Math.random() * 900);
    }
    // complete the live request once its stream is done
    if (c.liveStart !== null && nowMs - c.liveStart > c.liveDurMs) {
      const turn = makeTurn(c.liveStart + c.liveDurMs, c.ctxTok, true);
      c.turns.push(turn);
      growCtx(c, turn);
      c.nextTurnAt = turn.tMs + (8 + Math.random() * 32) * 1000;
      c.liveStart = null;
    }
    // new turns on schedule (a fresh live request stands in for it)
    if (c.liveStart === null && c.nextTurnAt <= nowMs) {
      c.nextTurnAt = nowMs + (8 + Math.random() * 32) * 1000;
      if (Math.random() < 0.5) {
        c.liveStart = nowMs - (3000 + Math.random() * 27000);
        c.liveDurMs = 15000 + Math.random() * 20000;
        c.liveNewTok = Math.round(100 + Math.random() * 900);
      }
    }
    if (c.turns.length > 40) c.turns.splice(0, c.turns.length - 40);
  }

  const live: LiveReq[] = [];
  for (const c of CONVS) {
    if (c.liveStart !== null) {
      live.push({
        id: ++liveReqId,
        sess: c.id,
        backend: c.backend,
        path: "/v1/chat/completions",
        stream: true,
        phase: nowMs - c.liveStart > 3500 ? "streaming" : "admitted",
        startMs: c.liveStart,
        ctxTok: c.ctxTok,
        newTok: c.liveNewTok,
      });
    }
  }
  for (const l of LONE_LIVE) {
    // lone requests loop: when they finish, restart a few seconds out
    if (nowMs - l.start > 18000) l.start = nowMs - 1000;
    live.push({
      id: ++liveReqId,
      sess: "",
      backend: l.backend,
      path: l.path,
      stream: l.stream,
      phase: nowMs - l.start > 3500 ? "streaming" : "admitted",
      startMs: l.start,
      ctxTok: l.ctxTok,
      newTok: l.newTok,
    });
  }
  live.sort((a, b) => a.startMs - b.startMs);

  const sessions = CONVS.map((c) => {
    const reqs = convReqs(c);
    const isLive = c.liveStart !== null;
    const lastMs = Math.round(
      isLive
        ? Math.max(c.liveStart!, ...c.turns.map((t) => t.tMs))
        : (c.turns.at(-1)?.tMs ?? 0),
    );
    const active = isLive || nowMs - lastMs < 120000;
    const latest = isLive
      ? { ctxTok: c.ctxTok, newTok: c.liveNewTok }
      : (c.turns.at(-1) ?? { ctxTok: 0, newTok: 0 });
    const cachedTok = Math.max(0, latest.ctxTok - latest.newTok);
    const ttf = reqs.filter((r) => r.stream && r.ttftMs > 0);
    return {
      id: c.id,
      backend: c.backend,
      n: c.turns.length,
      firstMs: Math.round(c.turns[0]?.tMs ?? lastMs),
      lastMs,
      active,
      liveN: isLive ? 1 : 0,
      ctxTok: latest.ctxTok,
      newTok: latest.newTok,
      cachedTok,
      avgTTFTMs: ttf.length
        ? Math.round(ttf.reduce((a, r) => a + r.ttftMs, 0) / ttf.length)
        : 0,
      totalDurS: +(reqs.reduce((a, r) => a + r.durMs, 0) / 1000).toFixed(1),
      tokOutTotal: reqs.reduce((a, r) => a + r.tokOut, 0),
      lastStatus: reqs.at(-1)?.status ?? 200,
      reqs: null as SessionReq[] | null,
    };
  });

  // per-request detail: active conversations within the first 20 rows
  sessions.slice(0, 20).forEach((s, i) => {
    if (s.active) s.reqs = convReqs(CONVS[i]).slice(-32);
  });

  sessions.sort((a, b) =>
    a.active !== b.active ? (a.active ? -1 : 1) : b.lastMs - a.lastMs,
  );

  return { t: nowMs, live, sessions };
}

/** The bounded request ring, newest last. */
export function mockBurst(limit: number): BurstReq[] {
  const n = Math.min(1000, Math.max(10, Number(limit) || 500));
  return ring.slice(-n);
}
