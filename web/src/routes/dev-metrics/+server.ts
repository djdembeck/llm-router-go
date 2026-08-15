// DEV-ONLY mock metrics feed. Synthesizes the EXACT contract frame shape
// (.impeccable/metrics-contract.md) with plausible random-walk traffic:
// two backends — one tier-0 "king" and one tier-1 "subject" — with an
// occasional saturation spike on the subject that produces 429s,
// streaming TTFTs, and burst spikes.
//
// This is a SvelteKit +server.ts route: adapter-static prunes server-only
// code from the production build, so it can never ship. The production
// store only ever hits /metrics/stream and /stats.

import { json } from "@sveltejs/kit";
import type { RequestHandler } from "./$types";

// ── shared synthesizer state (module-level; one process per dev server) ─

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
  ttftSampleCount: number;
  reqRate: number;
  bytesRate: number;
  tokEstRate: number;
  reqTotal: number;
  bytesInTotal: number;
  bytesOutTotal: number;
  tokEstTotal: number;
  spikeTicks: number;
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
    ttftSampleCount: 40,
    reqRate: 1.1,
    bytesRate: 52000,
    tokEstRate: 2400,
    reqTotal: 1284,
    bytesInTotal: 2600000,
    bytesOutTotal: 183000000,
    tokEstTotal: 4210000,
    spikeTicks: 0,
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
    ttftSampleCount: 22,
    reqRate: 0.6,
    bytesRate: 31000,
    tokEstRate: 1500,
    reqTotal: 512,
    bytesInTotal: 990000,
    bytesOutTotal: 74000000,
    tokEstTotal: 1810000,
    spikeTicks: 0,
  },
];

const clamp = (v: number, lo: number, hi: number) =>
  Math.min(hi, Math.max(lo, v));
const walk = (v: number, step: number, lo: number, hi: number) =>
  clamp(v + (Math.random() * 2 - 1) * step, lo, hi);

// request ring, newest last — mirrors the server's bounded burst ring
interface BurstReq {
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

interface Frame {
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

  for (const b of BACKENDS) {
    const spike = b.spikeTicks > 0;
    const cap = b.maxConcurrent;
    b.inFlight = Math.round(walk(b.inFlight, spike ? 1.4 : 0.8, 0, cap));
    b.waiting = spike
      ? Math.round(walk(b.waiting, 1.2, 1, b.maxQueueDepth))
      : Math.random() < 0.15 ? 1 : 0;
    b.prefillInFlight = Math.round(walk(b.prefillInFlight, 0.6, 0, b.prefillMax));
    b.prefillWaiting = Math.random() < (spike ? 0.4 : 0.08) ? 1 : 0;
    b.ewmaMs = Math.round(walk(b.ewmaMs, 220, 3000, 26000));
    b.ttftMs = Math.round(walk(b.ttftMs, 40, 90, 1500));
    b.ttftSampleCount = clamp(
      b.ttftSampleCount + (Math.random() < 0.7 ? 1 : 0),
      0,
      400,
    );
    b.reqRate = +walk(b.reqRate, spike ? 1.6 : 0.5, 0, spike ? 9 : 2.6).toFixed(2);
    b.bytesRate = Math.round(walk(b.bytesRate, spike ? 9000 : 4000, 2000, 160000));
    b.tokEstRate = Math.round(walk(b.tokEstRate, spike ? 700 : 300, 0, 9000));
    const n = Math.random() < b.reqRate * 0.5
      ? Math.round(b.reqRate * 0.5) + 1
      : 0;
    for (let i = 0; i < n; i++) spawnRequest(now);
  }

  const tier0 = BACKENDS.filter((b) => b.tier === 0);
  const gpuActive = tier0.some((b) => b.inFlight > 0);
  const gpuUsed = gpuActive ? clamp(tier0[0].inFlight, 0, 4) : 0;

  const backends = BACKENDS.map((b) => ({
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
    ttftSampleCount: b.ttftSampleCount,
    reqRate: b.reqRate,
    bytesRate: b.bytesRate,
    tokEstRate: b.tokEstRate,
    reqTotal: b.reqTotal,
    bytesInTotal: b.bytesInTotal,
    bytesOutTotal: b.bytesOutTotal,
    tokEstTotal: b.tokEstTotal,
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

// ── routes ───────────────────────────────────────────────────────────────

/** GET /dev-metrics/burst — the bounded request ring. */
export const GET: RequestHandler = async ({ url, request }) => {
  if (url.pathname === "/dev-metrics/burst") {
    const limit = Math.min(
      1000,
      Math.max(10, Number(url.searchParams.get("limit")) || 500),
    );
    return json({ requests: ring.slice(-limit) });
  }
  const raw = Number(url.searchParams.get("interval")?.replace("ms", ""));
  const interval = clamp(
    Number.isFinite(raw) && raw > 0 ? raw : 500,
    200,
    10000,
  );

  return new Response(
    new ReadableStream({
      start(controller) {
        let open = true;
        const send = () => {
          if (!open) return;
          try {
            controller.enqueue(
              new TextEncoder().encode(`data: ${JSON.stringify(tick())}\n\n`),
            );
          } catch {
            open = false;
          }
        };
        send();
        const iv = setInterval(send, interval);
        const stop = () => {
          if (!open) return;
          open = false;
          clearInterval(iv);
          try {
            controller.close();
          } catch {
            /* already closed */
          }
        };
        request.signal.addEventListener("abort", stop);
        // let the dev server process exit without waiting on idle streams
        (iv as unknown as { unref?: () => void }).unref?.();
      },
    }),
    {
      headers: {
        "Content-Type": "text/event-stream",
        "Cache-Control": "no-cache",
        Connection: "keep-alive",
        "X-Accel-Buffering": "no",
      },
    },
  );
};
