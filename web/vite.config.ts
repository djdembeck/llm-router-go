import adapter from "@sveltejs/adapter-static";
import { sveltekit } from "@sveltejs/kit/vite";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

// The real router listens on :80 in Docker / bare-binary deployments. When a
// router is up locally, these proxy rules forward its endpoints to the dev
// server so the store's primary feed (/metrics/stream) works against real
// data. When no router is up, the store falls back to /dev-metrics/stream.
// After the listener split /metrics/* live on the management listener
// (METRICS_LISTEN, default 127.0.0.1:9100); /stats and /v1/models stay on
// the external listener.
const proxyTarget = process.env.DEV_ROUTER_PROXY || "http://localhost:80";
const metricsProxyTarget =
  process.env.METRICS_ROUTER_PROXY || "http://localhost:9100";

const routerProxy = {
  target: proxyTarget,
  changeOrigin: true,
};

const metricsRouterProxy = {
  target: metricsProxyTarget,
  changeOrigin: true,
};

export default defineConfig({
  plugins: [
    tailwindcss(),
    sveltekit({
      adapter: adapter({
        fallback: "index.html",
      }),
      // $lib was removed in Kit 3 for #lib; keep the existing $lib imports
      // working via an explicit alias (official migration path).
      alias: {
        $lib: "src/lib",
      },
      compilerOptions: {
        runes: ({ filename }) =>
          filename.split(/[/\\]/).includes("node_modules") ? undefined : true,
      },
      prerender: {
        entries: [],
      },
    }),
  ],
  server: {
    host: "0.0.0.0",
    port: Number(process.env.VITE_PORT) || 5173,
    proxy: {
      "/stats": routerProxy,
      "/metrics": metricsRouterProxy,
      "/v1/models": routerProxy,
    },
  },
});
