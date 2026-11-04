// Every figure in this dashboard is mono, tabular, and unit-bearing. These
// helpers are the only place numbers become text.

const nf0 = new Intl.NumberFormat("en-US", { maximumFractionDigits: 0 });
const nf1 = new Intl.NumberFormat("en-US", { minimumFractionDigits: 1, maximumFractionDigits: 1 });

export const DASH = "—";

/** Module-time reduced-motion preference; JS transitions must opt out too,
 *  not only CSS. SSR resolves false and the client module re-evaluates. */
export const calmMotion =
  typeof window !== "undefined" &&
  window.matchMedia("(prefers-reduced-motion: reduce)").matches;

export function fmtInt(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  return nf0.format(v);
}

export function fmtFloat(v: number | null | undefined, digits = 1): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  if (digits === 0) return nf0.format(v);
  return v.toFixed(digits);
}

/** Compact count: 1.2k, 41k, 2.4M. */
export function fmtCompact(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  const a = Math.abs(v);
  if (a >= 1e9) return `${(v / 1e9).toFixed(a >= 1e10 ? 0 : 1)}G`;
  if (a >= 1e6) return `${(v / 1e6).toFixed(a >= 1e7 ? 0 : 1)}M`;
  if (a >= 1e3) return `${(v / 1e3).toFixed(a >= 1e4 ? 0 : 1)}k`;
  return nf0.format(v);
}

export function fmtPct(v: number | null | undefined, digits = 0): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  return `${v.toFixed(digits)}%`;
}

export function fmtMs(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  if (v < 1) return "<1ms";
  if (v < 1000) return `${Math.round(v)}ms`;
  if (v < 60000) return `${(v / 1000).toFixed(v < 10000 ? 2 : 1)}s`;
  const m = Math.floor(v / 60000);
  const s = Math.round((v % 60000) / 1000);
  return `${m}m ${String(s).padStart(2, "0")}s`;
}

export function fmtRate(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  if (Math.abs(v) >= 100) return nf0.format(v);
  if (Math.abs(v) >= 10) return v.toFixed(1);
  return v.toFixed(2);
}

export function fmtBytes(v: number | null | undefined): string {
  if (v == null || !Number.isFinite(v)) return DASH;
  const a = Math.abs(v);
  if (a >= 1 << 30) return `${(v / (1 << 30)).toFixed(1)}G`;
  if (a >= 1 << 20) return `${(v / (1 << 20)).toFixed(1)}M`;
  if (a >= 1 << 10) return `${(v / (1 << 10)).toFixed(1)}k`;
  return `${Math.round(v)}B`;
}

export function fmtGB(mb: number | null | undefined): string {
  if (mb == null || !Number.isFinite(mb)) return DASH;
  return `${(mb / 1024).toFixed(1)}`;
}

export function fmtClock(ms: number): string {
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}:${String(d.getSeconds()).padStart(2, "0")}`;
}

export function fmtHHMM(ms: number): string {
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

/** Elapsed: 0:24, 12:03, 1h 02m. */
export function fmtAge(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return DASH;
  const s = Math.floor(ms / 1000);
  if (s < 3600) {
    const m = Math.floor(s / 60);
    return `${m}:${String(s % 60).padStart(2, "0")}`;
  }
  const h = Math.floor(s / 3600);
  return `${h}h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
}

/** Relative age: 4s, 3m, 2h, 6d. */
export function fmtSince(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

export function fmtDelta(
  cur: number | null | undefined,
  prev: number | null | undefined,
  digits = 0,
): { text: string; dir: -1 | 0 | 1 } {
  if (cur == null || prev == null || !Number.isFinite(cur) || !Number.isFinite(prev)) {
    return { text: DASH, dir: 0 };
  }
  const d = cur - prev;
  if (Math.abs(d) < 1e-9) return { text: "0", dir: 0 };
  const sign = d > 0 ? "+" : "−";
  const a = Math.abs(d);
  const text =
    a >= 1000
      ? `${sign}${fmtCompact(a)}`
      : `${sign}${a.toFixed(digits)}`;
  return { text, dir: d > 0 ? 1 : -1 };
}

export function clamp(v: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, v));
}

/** A readable axis maximum for an auto-scaled layer. */
export function niceMax(v: number): number {
  if (!Number.isFinite(v) || v <= 0) return 1;
  const exp = Math.floor(Math.log10(v));
  const base = Math.pow(10, exp);
  const f = v / base;
  const step = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10;
  return step * base;
}

export function nearestIndex(times: number[], t: number): number {
  if (times.length === 0) return -1;
  let lo = 0;
  let hi = times.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (times[mid] < t) lo = mid + 1;
    else hi = mid;
  }
  if (lo > 0 && Math.abs(times[lo - 1] - t) <= Math.abs(times[lo] - t)) return lo - 1;
  return lo;
}

export function csvEscape(v: string): string {
  return /[",\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v;
}
