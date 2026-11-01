// DEV-ONLY synthetic history for the dashboard profile. Pruned from the
// production build by adapter-static.

import { json } from "@sveltejs/kit";
import type { RequestHandler } from "./$types";
import { mockHistory } from "$lib/mock-synth.js";
import type { RangeName } from "$lib/api.js";

const RANGES: RangeName[] = ["5m", "1h", "6h", "24h", "7d", "30d"];

/** GET /dev-metrics/history?range=5m|1h|6h|24h|7d|30d */
export const GET: RequestHandler = ({ url }) => {
  const raw = url.searchParams.get("range") ?? "1h";
  const range: RangeName = RANGES.includes(raw as RangeName)
    ? (raw as RangeName)
    : "1h";
  return json(mockHistory(range));
};
