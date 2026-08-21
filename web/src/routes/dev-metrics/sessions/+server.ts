// DEV-ONLY session feed for the dev metrics store. Pruned from the
// production build by adapter-static.

import { json } from "@sveltejs/kit";
import type { RequestHandler } from "./$types";
import { synthSessions } from "$lib/mock-synth.js";

/** GET /dev-metrics/sessions — the simulated live stack + conversations. */
export const GET: RequestHandler = () => {
  return json(synthSessions(Date.now()));
};
