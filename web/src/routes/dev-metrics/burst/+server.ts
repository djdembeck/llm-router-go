// DEV-ONLY request-ring endpoint for the dev metrics store. Pruned
// from the production build by adapter-static.

import { json } from "@sveltejs/kit";
import type { RequestHandler } from "./$types";
import { mockBurst } from "$lib/mock-synth.js";

/** GET /dev-metrics/burst — the bounded request ring. */
export const GET: RequestHandler = ({ url }) => {
  return json({ requests: mockBurst(Number(url.searchParams.get("limit"))) });
};
