// The server owns history; this store only fetches a range and keeps a
// short cache. The live tail is appended by the page from the 1 s frames, so
// the profile's right edge keeps moving while the fetched series is stable.

import { browser } from "$app/environment";
import { getJSON, type HistoryResponse, type RangeName } from "./api";

const REFRESH_MS: Record<RangeName, number> = {
  "5m": 15_000,
  "1h": 30_000,
  "6h": 90_000,
  "24h": 120_000,
  "7d": 180_000,
  "30d": 300_000,
};

export class HistoryStore {
  range = $state<RangeName>("5m");
  data = $state<HistoryResponse | null>(null);
  loading = $state(false);
  error = $state<string | null>(null);

  private cache = new Map<RangeName, { at: number; data: HistoryResponse }>();
  private timer: number | null = null;
  private gen = 0;

  start(): void {
    if (!browser) return;
    void this.load(this.range, true);
    this.schedule();
  }

  destroy(): void {
    clearInterval(this.timer ?? 0);
    this.timer = null;
  }

  clear(): void {
    this.cache.clear();
    this.data = null;
    this.error = null;
    void this.load(this.range, true);
  }

  setRange(r: RangeName): void {
    if (r === this.range) return;
    this.range = r;
    this.error = null;
    const hit = this.cache.get(r);
    if (hit && Date.now() - hit.at < 5000) {
      this.data = hit.data;
      this.loading = false;
    } else {
      void this.load(r, true);
    }
    this.schedule();
  }

  async load(r: RangeName = this.range, force = false): Promise<void> {
    if (!browser) return;
    const hit = this.cache.get(r);
    if (!force && hit && Date.now() - hit.at < 5000) {
      if (r === this.range) {
        this.data = hit.data;
        this.loading = false;
      }
      return;
    }
    const gen = ++this.gen;
    if (!this.data || r === this.range) this.loading = true;
    let j = await getJSON<HistoryResponse>(`/metrics/history?range=${r}`);
    if ((!j || !Array.isArray(j.backends)) && !import.meta.env.PROD) {
      j = await getJSON<HistoryResponse>(`/dev-metrics/history?range=${r}`);
    }
    if (gen !== this.gen) return;
    this.loading = false;
    if (!j || !Array.isArray(j.backends) || !j.fleet) {
      this.error = "history unavailable";
      return;
    }
    this.error = null;
    this.cache.set(r, { at: Date.now(), data: j });
    if (r === this.range) this.data = j;
  }

  private schedule(): void {
    clearInterval(this.timer ?? 0);
    this.timer = window.setInterval(() => void this.load(this.range), REFRESH_MS[this.range]);
  }
}
