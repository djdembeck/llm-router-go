<script lang="ts">
  import { onMount } from "svelte";

  // Bespoke canvas trace engine — a scrolling scope, not a stepped chart.
  // Samples are plotted at their SERVER timestamps on a continuous time axis;
  // a requestAnimationFrame loop scrolls the window at frame rate so the line
  // glides instead of jumping every 500ms data tick. The newest third of the
  // visible line is lit (the "live head"), older segments fade into the
  // crease (afterglow). The y-axis eases toward its new scale instead of
  // snapping. devicePixelRatio-aware. One canvas per instance; drawing is
  // trivially cheap (≤120 segments) even with several live traces.
  //
  // prefers-reduced-motion: the continuous scroll is disabled and the trace
  // redraws only on data ticks, with the window anchored to the newest sample.

  interface Props {
    /** newest-last sample values; null = gap (no value that tick) */
    samples: (number | null)[];
    /** server-timestamp (epoch ms) per sample, same length as samples, ascending */
    ts: number[];
    /** null = no limit to draw */
    limit?: number | null;
    /** null = auto-scale to max visible sample */
    yMax?: number | null;
    color?: "gold" | "valley";
    /** overlay second signal (ttft) at low opacity, aligned to the same ts */
    overlay?: (number | null)[] | null;
    /** window shown by the axis, in seconds */
    windowS?: number;
    /** hide the internal crease grid (used by tiny packet traces) */
    bare?: boolean;
  }

  let {
    samples = [],
    ts = [],
    limit = null,
    yMax = null,
    color = "gold",
    overlay = null,
    windowS = 60,
    bare = false,
  }: Props = $props();

  let wrap: HTMLDivElement | undefined = $state();
  let canvas: HTMLCanvasElement | undefined = $state();
  let ctx: CanvasRenderingContext2D | undefined = $state();
  let cssW = 0;
  let cssH = 0;

  const GOLD_DIM = "#8a6f35";
  const GOLD_LIT = "#e4c264";
  const VALLEY_DIM = "#5d7488";
  const VALLEY_LIT = "#cfe0ef";

  // latest props, read by the rAF loop without re-subscribing
  let curSamples: (number | null)[] = [];
  let curTs: number[] = [];
  let curLimit: number | null = null;
  let curYMax: number | null = null;
  let curColor: "gold" | "valley" = "gold";
  let curOverlay: (number | null)[] | null = null;
  let curWindowS = 60;
  let curBare = false;

  let viewRight = 0; // virtual right edge, in sample time (server ms)
  let dispHi = 0; // eased y scale
  let raf = 0;
  let lastFrameMs = 0;
  const reduced =
    typeof matchMedia !== "undefined" &&
    matchMedia("(prefers-reduced-motion: reduce)").matches;

  $effect(() => {
    curWindowS = windowS;
  });
  $effect(() => {
    curBare = bare;
    draw(); // reduced-motion path: tick-driven redraw
  });
  $effect(() => {
    curSamples = samples;
    curTs = ts;
    curLimit = limit;
    curYMax = yMax;
    curColor = color;
    curOverlay = overlay;
  });

  function size() {
    if (!wrap || !canvas) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = wrap.clientWidth;
    const h = wrap.clientHeight;
    if (w === 0 || h === 0) return;
    cssW = w;
    cssH = h;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
    canvas.style.width = w + "px";
    canvas.style.height = h + "px";
    ctx = canvas.getContext("2d") ?? undefined;
    if (ctx) ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    draw();
  }

  function visibleRange(): [number, number] | null {
    const n = curTs.length;
    if (n < 2) return null;
    if (viewRight === 0) viewRight = curTs[n - 1];
    // the right edge creeps forward in real time but never falls behind the
    // newest sample
    viewRight = Math.max(viewRight, curTs[n - 1]);
    const left = viewRight - curWindowS * 1000;
    return [left, viewRight];
  }

  function targetHi(): number {
    let hi = 0;
    for (const v of curSamples) if (v !== null && v > hi) hi = v;
    for (const v of curOverlay ?? []) if (v !== null && v > hi) hi = v;
    if (curYMax !== null && curYMax > 0) hi = Math.max(hi, curYMax);
    if (curLimit !== null && curLimit > 0) hi = Math.max(hi, curLimit);
    return hi * 1.08 || 1;
  }

  function draw() {
    const c = ctx;
    if (!c || cssW === 0) return;
    const W = cssW;
    const H = cssH;
    c.clearRect(0, 0, W, H);

    const range = visibleRange();
    if (!range) {
      if (!curBare) grid(c, 0, 0);
      return;
    }
    const [left, right] = range;
    const x = (t: number) => ((t - left) / (right - left)) * W;

    // eased y scale so the axis glides, not snaps
    const target = targetHi();
    dispHi = dispHi === 0 ? target : dispHi + (target - dispHi) * 0.15;
    const hi = dispHi;
    const y = (v: number) => H - 3 - (v / hi) * (H - 8);

    if (!curBare) grid(c, left, right);

    const dim = curColor === "gold" ? GOLD_DIM : VALLEY_DIM;
    const lit = curColor === "gold" ? GOLD_LIT : VALLEY_LIT;

    const pass = (vals: (number | null)[], overlay: boolean) => {
      const n = vals.length;
      if (n < 2 || curTs.length < 2) return;
      const headAge = 0.3; // newest 30% of the window is lit
      for (let i = 1; i < n; i++) {
        const a = vals[i - 1];
        const b = vals[i];
        const ta = curTs[i - 1];
        const tb = curTs[i];
        if (a === null || b === null) continue;
        if (tb < left || ta > right) continue;
        const age = (right - tb) / (right - left); // 0 = newest, 1 = oldest
        const isLit = age < headAge;
        const recency = 1 - age;
        c.strokeStyle = isLit ? lit : dim;
        c.globalAlpha = (isLit ? 0.95 : 0.1 + 0.55 * recency * recency) * (overlay ? 0.55 : 1);
        c.lineWidth = isLit ? 1.6 : 1;
        c.beginPath();
        c.moveTo(x(ta), y(a));
        c.lineTo(x(tb), y(b));
        c.stroke();
      }
      c.globalAlpha = 1;
    };

    if (curLimit !== null && curLimit > 0) {
      const ly = y(curLimit);
      c.strokeStyle = "rgba(143,169,192,0.45)";
      c.lineWidth = 1;
      c.setLineDash([4, 4]);
      c.beginPath();
      c.moveTo(0, ly + 0.5);
      c.lineTo(W, ly + 0.5);
      c.stroke();
      c.setLineDash([]);
      c.fillStyle = "rgba(143,169,192,0.6)";
      c.font = "9px 'JetBrains Mono', monospace";
      c.textAlign = "right";
      c.fillText(String(curLimit), W - 3, Math.max(9, ly - 3));
    }

    pass(curSamples, false);
    if (curOverlay && curOverlay.length > 1) pass(curOverlay, true);

    // live head: the newest sample, glowing
    const n = curSamples.length;
    const last = curSamples[n - 1];
    if (last !== null && curTs[n - 1] >= left) {
      const hx = x(curTs[n - 1]);
      const hy = y(last);
      c.fillStyle = lit;
      c.globalAlpha = 0.35;
      c.beginPath();
      c.arc(hx, hy, 5, 0, Math.PI * 2);
      c.fill();
      c.globalAlpha = 0.95;
      c.beginPath();
      c.arc(hx, hy, 2, 0, Math.PI * 2);
      c.fill();
      c.globalAlpha = 1;
    }
  }

  // crease grid: verticals are TIME ticks (every 10s, aligned to the server
  // clock) so they scroll with the axis — the sheet reads as a live scope;
  // plus the Miura diagonal seam.
  function grid(g: CanvasRenderingContext2D, left: number, right: number) {
    const W = cssW;
    const H = cssH;
    if (left > 0) {
      g.strokeStyle = "rgba(166,166,160,0.12)";
      g.lineWidth = 1;
      const step = 10000; // 10s ticks
      const t0 = Math.ceil(left / step) * step;
      for (let t = t0; t < right; t += step) {
        const gx = Math.round(((t - left) / (right - left)) * W) + 0.5;
        g.beginPath();
        g.moveTo(gx, 0);
        g.lineTo(gx, H);
        g.stroke();
      }
    } else {
      g.strokeStyle = "rgba(166,166,160,0.12)";
      g.lineWidth = 1;
      for (let gx = 44; gx < W; gx += 44) {
        g.beginPath();
        g.moveTo(gx + 0.5, 0);
        g.lineTo(gx + 0.5, H);
        g.stroke();
      }
    }
    g.strokeStyle = "rgba(143,169,192,0.09)";
    const dw = W * 0.36;
    g.beginPath();
    g.moveTo(0, H);
    g.lineTo(dw, 0);
    g.stroke();
    g.beginPath();
    g.moveTo(W - dw, H);
    g.lineTo(W, 0);
    g.stroke();
  }

  function loop(ms: number) {
    raf = requestAnimationFrame(loop);
    if (lastFrameMs === 0) {
      lastFrameMs = ms;
      return;
    }
    const dt = ms - lastFrameMs;
    lastFrameMs = ms;
    // advance the virtual clock in sample-time: the window scrolls in real
    // time, anchored to the server's sample timestamps (skew-free)
    if (curTs.length > 0) viewRight = Math.max(viewRight + dt, curTs[curTs.length - 1]);
    draw();
  }

  onMount(() => {
    size();
    if (wrap && canvas) {
      const ro = new ResizeObserver(size);
      ro.observe(wrap);
      if (!reduced) raf = requestAnimationFrame(loop);
      return () => {
        ro.disconnect();
        if (raf) cancelAnimationFrame(raf);
      };
    }
  });
</script>

<div class="trace-wrap" bind:this={wrap} aria-hidden="true">
  <canvas bind:this={canvas}></canvas>
</div>

<style>
  .trace-wrap {
    position: absolute;
    inset: 0;
    overflow: hidden;
  }
  canvas {
    display: block;
  }
</style>
