// DEV-ONLY SSE frame feed for the dev metrics store. Pruned from the
// production build by adapter-static.

import type { RequestHandler } from "./$types";
import { mockTick } from "$lib/mock-synth.js";

/** GET /dev-metrics/stream — the live frame feed (SSE). */
export const GET: RequestHandler = ({ url, request }) => {
  const raw = Number(url.searchParams.get("interval")?.replace("ms", ""));
  const interval = Math.min(
    1000,
    Math.max(200, Number.isFinite(raw) && raw > 0 ? raw : 500),
  );

  return new Response(
    new ReadableStream({
      start(controller) {
        let open = true;
        const send = () => {
          if (!open) return;
          try {
            controller.enqueue(
              new TextEncoder().encode(`data: ${JSON.stringify(mockTick())}\n\n`),
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
        // (Node returns a Timeout handle; a browser build returns a number)
        if (typeof iv !== "number" && "unref" in iv) iv.unref();
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
