// Display formatting for the Miura sheet. Rates/labels that are token
// estimates always carry the "est" marker at the call site — these helpers
// only shape numbers.

export function fmtRate(v: number | null | undefined, digits = 1): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v >= 100) return String(Math.round(v));
  return v.toFixed(digits);
}

export function fmtBytes(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v >= 1e9) return (v / 1e9).toFixed(1) + " GB";
  if (v >= 1e6) return (v / 1e6).toFixed(1) + " MB";
  if (v >= 1e3) return (v / 1e3).toFixed(0) + " KB";
  return v.toFixed(0) + " B";
}

export function fmtBytesRate(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v >= 1e6) return (v / 1e6).toFixed(1) + " MB/s";
  if (v >= 1e3) return (v / 1e3).toFixed(0) + " KB/s";
  return v.toFixed(0) + " B/s";
}

export function fmtMs(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v >= 10000) return (v / 1000).toFixed(1) + " s";
  return v >= 100 ? v.toFixed(0) + " ms" : v.toFixed(0) + " ms";
}

/** seconds (avgDurationS) → readable */
export function fmtS(v: number | null | undefined): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v >= 100) return v.toFixed(0) + " s";
  if (v >= 10) return v.toFixed(1) + " s";
  return v.toFixed(2) + " s";
}

/** compact token/byte counts → 132k / 2.4M / 1.2B */
export function fmtCompact(v: number | null | undefined, digits = 1): string {
  if (v === null || v === undefined || !Number.isFinite(v)) return "—";
  if (v >= 1e9) return (v / 1e9).toFixed(digits) + "B";
  if (v >= 1e6) return (v / 1e6).toFixed(digits) + "M";
  if (v >= 1e4) return (v / 1e3).toFixed(digits) + "k";
  return v >= 1e3 ? (v / 1e3).toFixed(0) + "k" : v.toFixed(0);
}

/** epoch ms → HH:MM:SS */
export function clock(epochMs: number): string {
  const d = new Date(epochMs);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}
