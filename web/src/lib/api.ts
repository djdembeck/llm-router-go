// Types mirror the router's live metrics contract plus the additive fields
// introduced with server-side history and device telemetry. Token figures
// are body-based ESTIMATES (~4 bytes/token); measured figures say so.

export interface EngineMetrics {
  status: "ok" | "off" | "err";
  engine: "vllm" | "sglang" | "";
  running: number;
  waiting: number;
  kvPct: number;
  kvHeldPct: number | null;
  hitRate: number;
  prefillTokS: number;
  prefillCacheTokS: number;
  prefillTotalTokS: number;
  decodeTokS: number;
  ttftMs: number;
  itlMs: number;
  e2eMs: number;
  queueMs: number;
  prefillMs: number | null;
  decodeMs: number | null;
  perTokMs: number | null;
  meanPromptTok: number;
  meanGenTok: number;
  waitCap: number | null;
  waitDefer: number | null;
  retracted: number | null;
  retractedTokS: number | null;
  fullPct: number | null;
  swaPct: number | null;
  mambaPct: number | null;
  kvUsedTok: number | null;
  kvCapTok: number | null;
  kvFreeTok: number | null;
  kvEvictTok: number | null;
  mambaUsedTok: number | null;
  mambaCapTok: number | null;
  hicacheHostUsedTok: number | null;
  hicacheHostCapTok: number | null;
  ttftStreamMs: number | null;
  ttftNonStreamMs: number | null;
  kvMemGB: number | null;
  weightMemGB: number | null;
  sloCap: number | null;
  ctxLen: number | null;
  preemptedTotal: number | null;
  abortedTotal: number | null;
  streamDoneTotal: number | null;
  nonStreamDoneTotal: number | null;
  promptTokTotal: number | null;
  genTokTotal: number | null;
  hitQueriesTotal: number | null;
  hitHitsTotal: number | null;
  mmQueriesTotal: number | null;
  mmHitsTotal: number | null;
  reqDoneTotal: number;
  finReasons: Record<string, number> | null;
  // added with the rework
  ttftSamples: number;
  itlSamples: number;
  e2eSamples: number;
  queueSamples: number;
  prefillSamples: number;
  decodeSamples: number;
  perTokSamples: number;
  promptSamples: number;
  genSamples: number;
  ttftStreamSamples: number;
  ttftNonStreamSamples: number;
  httpStatus: number;
  lastError?: string;
}

export const emptyEngine: EngineMetrics = {
  status: "off",
  engine: "",
  running: 0,
  waiting: 0,
  kvPct: 0,
  kvHeldPct: null,
  hitRate: 0,
  prefillTokS: 0,
  prefillCacheTokS: 0,
  prefillTotalTokS: 0,
  decodeTokS: 0,
  ttftMs: 0,
  itlMs: 0,
  e2eMs: 0,
  queueMs: 0,
  prefillMs: null,
  decodeMs: null,
  perTokMs: null,
  meanPromptTok: 0,
  meanGenTok: 0,
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
  reqDoneTotal: 0,
  finReasons: null,
  ttftSamples: 0,
  itlSamples: 0,
  e2eSamples: 0,
  queueSamples: 0,
  prefillSamples: 0,
  decodeSamples: 0,
  perTokSamples: 0,
  promptSamples: 0,
  genSamples: 0,
  ttftStreamSamples: 0,
  ttftNonStreamSamples: 0,
  httpStatus: 0,
};

// The per-backend session-admission (lease) snapshot. null in the frame when
// session admission is disabled for the backend; an object (possibly
// all-zero) when enabled. The authoritative view of router-side capacity
// admission — the thing the dashboard's "backend at capacity" complaint is
// about. Gate-wait figures are the admission gate ONLY, not downstream.
export interface AdmissionMetrics {
  maxAdmitted: number;
  maxWaiting: number;
  idleTTLms: number;
  active: number;
  idle: number;
  waiting: number;
  oldestWaitMs: number;
  reclaimedTotal: number;
  gateWaitMs: number;
  gateWaitTotalMs: number;
  gateWaitCount: number;
  rejQueueFull: number;
  rejQueueTimeout: number;
  rejClientCancel: number;
}

export const ROUTER_429_REASONS = [
  "blocked-by-tier0", "session-admission-queue-full", "session-admission-timeout",
  "gpu-budget-full", "backend-full", "backend-queue-full", "prefill-full", "prefill-queue-full",
] as const;
export type QueueStage = "admission" | "gpu" | "execution" | "prefill";
export interface WaitSummary { sumMs: number; samples: number }
export type QueueWait = Record<QueueStage, WaitSummary>;
export interface Router429 {
  total: number;
  rate: number;
  byReason: { reason: string; total: number; rate: number }[];
}
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
  ttftMsNow: number;
  reqRate: number;
  bytesRate: number;
  tokEstRate: number;
  reqTotal: number;
  rejectTotal: number;
  // Per-backend lifetime router-originated rejection counts keyed by the
  // X-Router-Reason string. null when the backend has never rejected.
  rejectReasons: Record<string, number> | null;
  bytesInTotal: number;
  bytesOutTotal: number;
  tokEstTotal: number;
  engine: EngineMetrics;
  // null when session admission is disabled for this backend.
  admission?: AdmissionMetrics | null;
  queueWait?: QueueWait | null;
  router429?: Router429 | null;
  engineQueue?: WaitSummary | null;
}

export interface GpuMetrics {
  used: number;
  budget: number;
  waiting: number;
  active: boolean;
}

export interface GpuDevice {
  key: string;
  index: number;
  name: string;
  utilPct: number;
  memUsedMB: number;
  memTotalMB: number;
  tempC: number | null;
  powerW: number | null;
  source: string;
}

export interface GpuFeed {
  status: "" | "ok" | "off" | "err";
  url: string;
  lastError?: string;
}

export interface TotalsMetrics {
  inFlight: number;
  waiting: number;
  reqRate: number;
  bytesRate: number;
  tokEstRate: number;
  reqTotal: number;
  rejectTotal: number;
  bytesInTotal: number;
  bytesOutTotal: number;
  tokEstTotal: number;
  ttftMs: number;
  ttftSampleCount: number;
  engineOk: number;
  engineRunning: number;
  engineWaiting: number;
  prefillTokS: number;
  prefillCacheTokS: number;
  decodeTokS: number;
  kvPeak: number;
  kvUsedTok: number;
  kvCapTok: number;
  kvMemGB: number;
  weightMemGB: number;
  hitRate: number;
  // Fleet rollup of active session-admission leases (0 when none enabled).
  admActive: number;
  router429?: Router429 | null;
  queueWait?: QueueWait | null;
}

export interface StatsBackend extends Pick<BackendMetrics, "name" | "url" | "tier" | "inFlight" | "maxConcurrent" | "waiting" | "maxQueueDepth" | "prefillInFlight" | "prefillWaiting" | "prefillMax" | "avgDurationS" | "admission" | "engineQueue"> {}
export interface StatsResponse {
  backends: StatsBackend[];
  gpuUsed: number;
  gpuBudget: number;
  gpuWaiting: number;
  tier0Inflight: number;
}

/** One SSE frame: a full live snapshot. */
export interface Frame {
  t: number;
  backends: BackendMetrics[];
  gpu: GpuMetrics;
  gpuDevices: GpuDevice[] | null;
  gpuFeed: GpuFeed;
  tier0Inflight: number;
  totals: TotalsMetrics;
}

export interface BurstRequest {
  t: number;
  start: number;
  backend: string;
  path: string;
  status: number;
  stream: boolean;
  durMs: number;
  ttftMs: number;
  newTokensEst: number;
  bytesIn: number;
  bytesOut: number;
  // X-Router-Reason for a router-originated rejection; "" for accepted.
  rejectReason?: string | null;
  queueWaitMs?: Record<QueueStage, number | null> | null;
  // Admission-gate wait in ms (keyed traffic only); 0 for unkeyed/disabled.
  gateWaitMs: number;
}

export interface LiveReq {
  id: number;
  sess: string;
  backend: string;
  path: string;
  stream: boolean;
  phase: "admitted" | "streaming";
  startMs: number;
  ctxTok: number;
  newTok: number;
  estPrefillMs: number;
  streamMs: number;
}

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

export interface SessionRow {
  id: string;
  backend: string;
  n: number;
  firstMs: number;
  lastMs: number;
  active: boolean;
  liveN: number;
  admissionLease: boolean;
  admissionIdleSinceMs: number;
  ctxTok: number;
  newTok: number;
  cachedTok: number;
  avgTTFTMs: number;
  totalDurS: number;
  tokOutTotal: number;
  lastStatus: number;
  reqs: SessionReq[] | null;
}

export interface SessionsFeed {
  t: number;
  live: LiveReq[];
  sessions: SessionRow[];
}

// ── history ──────────────────────────────────────────────────────────────

export type RangeName = "5m" | "1h" | "6h" | "24h" | "7d" | "30d";

export const RANGES: { id: RangeName; label: string; long: string }[] = [
  { id: "5m", label: "5m", long: "5 minutes" },
  { id: "1h", label: "1h", long: "1 hour" },
  { id: "6h", label: "6h", long: "6 hours" },
  { id: "24h", label: "24h", long: "24 hours" },
  { id: "7d", label: "7d", long: "7 days" },
  { id: "30d", label: "30d", long: "30 days" },
];

export type HistoryField =
  | "inFlight"
  | "waiting"
  | "maxConcurrent"
  | "prefillInFlight"
  | "prefillWaiting"
  | "reqRate"
  | "tokEstRate"
  | "ewmaMs"
  | "ttftMs"
  | "ttftMax"
  | "ttftSamples"
  | "engineRunning"
  | "engineWaiting"
  | "prefillTokS"
  | "prefillCacheTokS"
  | "decodeTokS"
  | "kvPct"
  | "kvHeldPct"
  | "kvMax"
  | "kvUsedTok"
  | "kvCapTok"
  | "kvMemGB"
  | "weightMemGB"
  | "hitRate"
  | "engineTTFTMs"
  // Additive capacity series (25 → 59 fields): router slot
  // limits, the admission lease/gate breakdown, the fleet GPU-budget
  // triplet, the router-429 per-reason rates, and the per-stage gate-wait
  // sum/sample rates. The admission LEASE gauges (maxAdmitted, maxWaiting,
  // active, idle, waiting, oldestWaitMs) are null for a backend without
  // session admission enabled — its gate-wait (admissionGateWaitMs,
  // admissionGateWaitTotalMs, admissionGateWaitCount) and rejection
  // (admissionRej*) series are too, since they live in the same frame.
  // The router429* series (total and per-reason) are null for a backend
  // that never emitted a router 429. The gate-wait rate pairs
  // (*WaitMsRate/*WaitSamplesRate) are null until a gate exit is recorded
  // in the trailing-30s window — for the admission stage, 0 samples for an
  // unkeyed backend — which tracks queue-wait presence, not 429 history.
  | "maxQueueDepth"
  | "prefillMax"
  | "admissionMaxAdmitted"
  | "admissionMaxWaiting"
  | "admissionActive"
  | "admissionIdle"
  | "admissionWaiting"
  | "admissionOldestWaitMs"
  | "admissionGateWaitMs"
  | "admissionGateWaitTotalMs"
  | "admissionGateWaitCount"
  | "admissionRejQueueFull"
  | "admissionRejQueueTimeout"
  | "admissionRejClientCancel"
  | "gpuUsed"
  | "gpuBudget"
  | "gpuWaiting"
  | "router429Rate"
  | "router429BlockedByTier0Rate"
  | "router429SessionQueueFullRate"
  | "router429SessionTimeoutRate"
  | "router429GpuBudgetFullRate"
  | "router429BackendFullRate"
  | "router429BackendQueueFullRate"
  | "router429PrefillFullRate"
  | "router429PrefillQueueFullRate"
  | "admissionWaitMsRate"
  | "admissionWaitSamplesRate"
  | "gpuWaitMsRate"
  | "gpuWaitSamplesRate"
  | "executionWaitMsRate"
  | "executionWaitSamplesRate"
  | "prefillWaitMsRate"
  | "prefillWaitSamplesRate";

export type Series = Partial<Record<HistoryField, (number | null)[]>>;

export interface HistorySeries {
  name: string;
  t: number[];
  series: Series;
}

export interface HistoryGpuSeries {
  key: string;
  index: number;
  name: string;
  source: string;
  t: number[];
  series: Partial<
    Record<"utilPct" | "memUsedMB" | "memTotalMB" | "tempC" | "powerW", (number | null)[]>
  >;
}

export interface TtftSummary {
  n: number;
  coverageMs: number;
  p50: number;
  p95: number;
}

export interface HistoryResponse {
  from: number;
  to: number;
  stepMs: number;
  range: RangeName;
  backends: HistorySeries[];
  fleet: HistorySeries;
  gpu: HistoryGpuSeries[];
  gpuFeed: GpuFeed;
  ttft: TtftSummary;
  now: number;
}

// ── fetchers (dev mock fallbacks live in the stores) ─────────────────────

export async function getJSON<T>(url: string): Promise<T | null> {
  try {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) return null;
    return (await res.json()) as T;
  } catch {
    return null;
  }
}


/** Rolling live averages used by the readings strip. */
export interface MetricAverages {
  ttftMs: number | null;
  prefillTokS: number | null;
  decodeTokS: number | null;
}
