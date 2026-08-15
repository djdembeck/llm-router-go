import { sveltekit } from "@sveltejs/kit/vite";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

// The real router listens on :80 in Docker / bare-binary deployments. When a
// router is up locally, these proxy rules forward its management endpoints to
// the dev server so the store's primary feed (/metrics/stream) works against
// real data. When no router is up, the store falls back to /dev-metrics/stream.
const proxyTarget = process.env.DEV_ROUTER_PROXY || "http://localhost:80";

const routerProxy = {
  target: proxyTarget,
  changeOrigin: true,
};

export default defineConfig({
  plugins: [tailwindcss(), sveltekit()],
  server: {
    host: "0.0.0.0",
    port: Number(process.env.VITE_PORT) || 5173,
    proxy: {
      "/stats": routerProxy,
      "/metrics": routerProxy,
      "/v1/models": routerProxy,
    },
  },
});
