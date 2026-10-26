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

  let controller: ReadableStreamDefaultController | undefined;
  let open = true;
  let iv: NodeJS.Timeout | number | undefined;
  const cleanup = (close: boolean) => {
    if (!open) return;
    open = false;
    clearInterval(iv); // no-op when iv is undefined
    request.signal.removeEventListener("abort", stop);
    if (close && controller) {
      try {
        controller.close();
      } catch {
        /* already closed */
      }
    }
  };
  const send = () => {
    if (!open || !controller) return;
    try {
      controller.enqueue(
        new TextEncoder().encode(`data: ${JSON.stringify(mockTick())}\n\n`),
      );
    } catch {
      cleanup(false); // enqueue failed: the stream is going, free the interval
    }
  };
  const stop = () => cleanup(true); // request aborted: close the stream + free the interval

  return new Response(
    new ReadableStream({
      start(c) {
        controller = c;
        send();
        iv = setInterval(send, interval);
        request.signal.addEventListener("abort", stop);
        // let the dev server process exit without waiting on idle streams
        // (Node returns a Timeout handle; a browser build returns a number)
        if (typeof iv !== "number" && "unref" in iv) iv.unref();
      },
      // Consumer cancelled the stream (tab close, fetch().cancel()): free
      // the interval; the stream is already gone, so no close is needed.
      cancel: () => cleanup(false),
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
