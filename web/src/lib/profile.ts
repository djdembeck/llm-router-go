// Geometry and value selection for the Fleet Profile. The profile is an
// elevation sheet: time on x, one lane per categorical entity on y. Lanes
// are never interpolated between — a contour across backends would invent
// data. Fixed layers use 0–100 %, auto layers get a readable max.

import type {
  BackendMetrics,
  Frame,
  GpuDevice,
  HistoryField,
  HistoryGpuSeries,
  HistoryResponse,
  RangeName,
  TotalsMetrics,
} from "./api";
import { nearestIndex, niceMax } from "./format";

export type LayerId = "kv" | "util" | "vram" | "ttft" | "engineTtft" | "prefill" | "decode";

export interface LayerDef {
  id: LayerId;
  label: string;
  short: string;
  unit: string;
  source: "engine" | "router" | "device";
  device: boolean;
  fixedMax?: number;
  field?: HistoryField;
  note: string;
}

export const LAYERS: LayerDef[] = [
  {
    id: "kv",
    label: "KV CACHE",
    short: "KV",
    unit: "%",
    source: "engine",
    device: false,
    fixedMax: 100,
    field: "kvPct",
    note: "engine /metrics · token_usage",
  },
  {
    id: "ttft",
    label: "TTFT · ROUTER",
    short: "TTFT",
    unit: "ms",
    source: "router",
    device: false,
    field: "ttftMs",
    note: "router · first response body byte",
  },
  {
    id: "engineTtft",
    label: "TTFT · ENGINE",
    short: "TTFT-E",
    unit: "ms",
    source: "engine",
    device: false,
    field: "engineTTFTMs",
    note: "engine · histogram mean",
  },
  {
    id: "prefill",
    label: "PREFILL · COMPUTE",
    short: "PREFILL",
    unit: "tok/s",
    source: "engine",
    device: false,
    field: "prefillTokS",
    note: "engine · compute only",
  },
  {
    id: "decode",
    label: "DECODE",
    short: "DECODE",
    unit: "tok/s",
    source: "engine",
    device: false,
    field: "decodeTokS",
    note: "engine · generation tokens",
  },
  {
    id: "util",
    label: "GPU UTILIZATION",
    short: "GPU",
    unit: "%",
    source: "device",
    device: true,
    fixedMax: 100,
    note: "exporter · device-wide",
  },
  {
    id: "vram",
    label: "VRAM USED",
    short: "VRAM",
    unit: "%",
    source: "device",
    device: true,
    fixedMax: 100,
    note: "exporter · device memory",
  },
];

export function layerById(id: LayerId): LayerDef {
  return LAYERS.find((l) => l.id === id) ?? LAYERS[0];
}

export interface LanePoint {
  t: number;
  v: number | null;
}

export interface Lane {
  key: string;
  label: string;
  tier: number | null;
  isFleet: boolean;
  isDevice: boolean;
  points: LanePoint[];
  current: number | null;
  source: string;
}

function backendValue(b: BackendMetrics, layer: LayerDef): number | null {
  const e = b.engine;
  switch (layer.id) {
    case "kv":
      return e.status === "ok" ? e.kvPct : null;
    case "ttft":
      return b.ttftSampleCount > 0 ? b.ttftMs : null;
    case "engineTtft":
      return e.status === "ok" && e.ttftSamples > 0 ? e.ttftMs : null;
    case "prefill":
      return e.status === "ok" ? e.prefillTokS : null;
    case "decode":
      return e.status === "ok" ? e.decodeTokS : null;
    default:
      return null;
  }
}

function fleetValue(t: TotalsMetrics, layer: LayerDef): number | null {
  switch (layer.id) {
    case "kv":
      return t.engineOk > 0 ? t.kvPeak : null;
    case "ttft":
      return t.ttftSampleCount > 0 ? t.ttftMs : null;
    case "engineTtft":
      return null;
    case "prefill":
      return t.engineOk > 0 ? t.prefillTokS : null;
    case "decode":
      return t.engineOk > 0 ? t.decodeTokS : null;
    default:
      return null;
  }
}

function gpuValue(d: GpuDevice, layer: LayerDef): number | null {
  if (layer.id === "util") return d.utilPct;
  if (layer.id === "vram") {
    return d.memTotalMB > 0 ? (d.memUsedMB / d.memTotalMB) * 100 : null;
  }
  return null;
}

/** Build every lane for the selected layer, merging server history with the
 *  live tail (frames newer than the fetched history's last point). */
export function buildLanes(
  history: HistoryResponse | null,
  frames: Frame[],
  layer: LayerDef,
  singleBackend: string | null,
): Lane[] {
  const lanes: Lane[] = [];
  if (layer.device) {
    const devs = new Map<string, Lane>();
    for (const g of history?.gpu ?? []) {
      const pts = (g.t ?? []).map((t, i) => ({ t, v: gpuSeriesAt(g, layer, i) }));
      devs.set(g.key, {
        key: g.key,
        label: g.name,
        tier: null,
        isFleet: false,
        isDevice: true,
        points: pts,
        current: lastValue(pts),
        source: g.source || "device",
      });
    }
    for (const f of frames) {
      let last = 0;
      for (const key of devs.keys()) {
        const lane = devs.get(key);
        if (lane && lane.points.length) {
          last = Math.max(last, lane.points[lane.points.length - 1].t);
        }
      }
      if (f.t <= last) continue;
      for (const d of f.gpuDevices ?? []) {
        const lane = devs.get(d.key);
        const v = gpuValue(d, layer);
        if (lane) {
          if (lane.points.length && f.t <= lane.points[lane.points.length - 1].t) continue;
          lane.points.push({ t: f.t, v });
          lane.current = v ?? lane.current;
        } else {
          devs.set(d.key, {
            key: d.key,
            label: d.name,
            tier: null,
            isFleet: false,
            isDevice: true,
            points: [{ t: f.t, v }],
            current: v,
            source: d.source || "device",
          });
        }
      }
    }
    return [...devs.values()].sort((a, b) => a.key.localeCompare(b.key));
  }

  if (singleBackend === null) {
    // Fleet aggregation is unnecessary when the server lane is authoritative.
    const fleetPts: LanePoint[] = [];
    if (history) {
      const arr = history.fleet?.series?.[layer.field as HistoryField] ?? [];
      for (let i = 0; i < (history.fleet?.t?.length ?? 0); i++) {
        fleetPts.push({ t: history.fleet.t[i], v: arr[i] ?? null });
      }
    }
    for (const f of frames) {
      // Merge after the last real sample, not the history grid's end.
      const last = fleetPts.length ? fleetPts[fleetPts.length - 1].t : 0;
      if (f.t <= last) continue;
      fleetPts.push({ t: f.t, v: fleetValue(f.totals, layer) });
    }
    lanes.push({
      key: "__fleet",
      label: "FLEET",
      tier: null,
      isFleet: true,
      isDevice: false,
      points: fleetPts,
      current: productFleetCurrent(frames, layer),
      source: layer.source,
    });
  }

  const names: string[] = singleBackend === null ? [] : [singleBackend];
  if (singleBackend === null) {
    const seen = new Set<string>();
    for (const b of history?.backends ?? []) {
      names.push(b.name);
      seen.add(b.name);
    }
    for (const f of frames) {
      for (const b of f.backends) {
        if (!seen.has(b.name)) {
          seen.add(b.name);
          names.push(b.name);
        }
      }
    }
  }

  for (const name of names) {
    const hs = history?.backends?.find((b) => b.name === name);
    const pts: LanePoint[] = [];
    if (hs) {
      const arr = hs.series[layer.field as HistoryField] ?? [];
      for (let i = 0; i < hs.t.length; i++) pts.push({ t: hs.t[i], v: arr[i] ?? null });
    }
    for (const f of frames) {
      const last = pts.length ? pts[pts.length - 1].t : 0;
      if (f.t <= last) continue;
      const b = f.backends.find((x) => x.name === name);
      if (!b) continue;
      pts.push({ t: f.t, v: backendValue(b, layer) });
    }
    const lastFrame = frames.length ? frames[frames.length - 1] : null;
    const b = lastFrame?.backends.find((x) => x.name === name) ?? null;
    lanes.push({
      key: name,
      label: name,
      tier: b?.tier ?? null,
      isFleet: false,
      isDevice: false,
      points: pts,
      current: b ? backendValue(b, layer) : lastValue(pts),
      source: layer.source,
    });
  }
  return lanes;
}

function gpuSeriesAt(g: HistoryGpuSeries, layer: LayerDef, i: number): number | null {
  if (layer.id === "util") return g.series.utilPct?.[i] ?? null;
  if (layer.id === "vram") {
    const used = g.series.memUsedMB?.[i];
    const total = g.series.memTotalMB?.[i];
    return used != null && total != null && total > 0 ? (used / total) * 100 : null;
  }
  return null;
}

function productFleetCurrent(frames: Frame[], layer: LayerDef): number | null {
  if (!frames.length) return null;
  return fleetValue(frames[frames.length - 1].totals, layer);
}

function lastValue(pts: LanePoint[]): number | null {
  for (let i = pts.length - 1; i >= 0; i--) {
    if (pts[i].v != null) return pts[i].v;
  }
  return null;
}

export interface Scale {
  max: number;
  ticks: { v: number; label: string }[];
}

export function scaleFor(
  layer: LayerDef,
  lanes: Lane[],
  domain?: { from: number; to: number },
): Scale {
  if (layer.fixedMax) {
    const max = layer.fixedMax;
    return {
      max,
      ticks: [0.25, 0.5, 0.75, 1].map((f) => ({
        v: max * f,
        label: layer.unit === "%" ? `${Math.round(max * f)}%` : `${Math.round(max * f)}`,
      })),
    };
  }
  // Only the visible domain may set the scale: the live tail keeps up to
  // 15 min of one-second frames, and a spike outside the window must not
  // compress the current data.
  let peak = 0;
  for (const lane of lanes) {
    for (const p of lane.points) {
      if (domain && (p.t < domain.from || p.t > domain.to)) continue;
      if (p.v != null && p.v > peak) peak = p.v;
    }
  }
  const max = niceMax(Math.max(peak, 1));
  const fmt = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(1)}k` : `${Math.round(v)}`);
  return {
    max,
    ticks: [0.25, 0.5, 0.75, 1].map((f) => ({ v: max * f, label: fmt(max * f) })),
  };
}

export function valueAt(lane: Lane, t: number): number | null {
  if (!lane.points.length) return null;
  const times = lane.points.map((p) => p.t);
  const i = nearestIndex(times, t);
  return i >= 0 ? lane.points[i].v : null;
}

export function summaryAt(lane: Lane, from: number, to: number): {
  last: number | null;
  avg: number | null;
  min: number | null;
  max: number | null;
} {
  let n = 0,
    sum = 0,
    min = Infinity,
    max = -Infinity,
    last: number | null = null;
  for (const p of lane.points) {
    if (p.t < from || p.t > to || p.v == null) continue;
    n++;
    sum += p.v;
    if (p.v < min) min = p.v;
    if (p.v > max) max = p.v;
    last = p.v;
  }
  return n
    ? { last, avg: sum / n, min, max }
    : { last, avg: null, min: null, max: null };
}

export function fmtLayerValue(layer: LayerDef, v: number | null): string {
  if (v == null || !Number.isFinite(v)) return "—";
  if (layer.unit === "%") return `${v.toFixed(v < 10 ? 1 : 0)}%`;
  if (layer.unit === "ms") return v < 1000 ? `${Math.round(v)}ms` : `${(v / 1000).toFixed(2)}s`;
  return v >= 1000 ? `${(v / 1000).toFixed(1)}k` : `${Math.round(v)}`;
}

export function tickLabel(ms: number, range: RangeName): string {
  const d = new Date(ms);
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  if (range === "5m" || range === "1h") return `${hh}:${mm}`;
  if (range === "6h" || range === "24h") return `${hh}:${mm}`;
  return `${d.getMonth() + 1}/${d.getDate()} ${hh}:${mm}`;
}
