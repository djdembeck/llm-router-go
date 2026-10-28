<script lang="ts">
  import type {
    BurstRequest,
    Frame,
    HistoryResponse,
    RangeName,
    SessionsFeed,
  } from "./api";
  import {
    buildLanes,
    fmtLayerValue,
    layerById,
    scaleFor,
    summaryAt,
    tickLabel,
    valueAt,
    type Lane,
    type LayerDef,
    type LayerId,
  } from "./profile";
  import { onMount, untrack } from "svelte";
  import TweenNumber from "./TweenNumber.svelte";
  import { clamp, fmtClock, fmtHHMM, fmtPct, nearestIndex } from "./format";

  let {
    history,
    frames,
    framesVersion,
    layerId,
    range,
    selected,
    singleBackend,
    cursor = $bindable(null),
    requests,
    sessions,
    paused,
    stale,
    theme,
  }: {
    history: HistoryResponse | null;
    frames: Frame[];
    framesVersion: number;
    layerId: LayerId;
    range: RangeName;
    selected: string | null;
    singleBackend: string | null;
    cursor: number | null;
    requests: BurstRequest[];
    sessions: SessionsFeed | null;
    paused: boolean;
    stale: boolean;
    theme: "night" | "day";
  } = $props();

  const layer: LayerDef = $derived(layerById(layerId));
  const lanes: Lane[] = $derived.by(() => {
    void framesVersion;
    return buildLanes(history, frames, layer, singleBackend);
  });
  const singleServerLane = $derived(
    singleBackend !== null && !layer.device && lanes.length === 1,
  );
  const selectedLane = $derived(
    (singleServerLane ? lanes[0] : lanes.find((l) => l.key === selected)) ?? lanes[0] ?? null,
  );
  const summary = $derived.by(() => {
    if (!selectedLane || !history) return { last: null, avg: null, min: null, max: null };
    return summaryAt(selectedLane, history.from, Math.max(history.to, edge));
  });

  let canvas: HTMLCanvasElement | null = $state(null);
  let host: HTMLDivElement | null = $state(null);
  let w = $state(900);
  let h = $state(300);
  let hoverT = $state<number | null>(null);
  let edge = $state(Date.now());
  let animFrom = Date.now();
  let animStart = 0;
  let lastEdgePaint = 0;
  let raf = 0;
  let reduced = false;
  let inView = $state(true);

  const padL = $derived(singleServerLane ? (w < 520 ? 48 : 58) : (w < 520 ? 122 : 150));
  const PAD_R = 14;
  const PAD_T = 20;
  const PAD_B = 26;

  // A fixed sliding window keyed to the live edge. It never depends on the
  // history fetch's alignment, so a background refresh cannot shift the
  // sheet; the edge glides continuously and the series slides under it.
  const laneSpan = $derived(
    range === "5m" ? 300_000
    : range === "1h" ? 3_600_000
    : range === "6h" ? 21_600_000
    : range === "24h" ? 86_400_000
    : range === "7d" ? 604_800_000
    : 2_592_000_000,
  );
  const domain = $derived.by(() => {
    const to = edge;
    return { from: to - laneSpan, to: Math.max(to, to - laneSpan + 1000) };
  });
  const scale = $derived(scaleFor(layer, lanes, domain));

  let plotOpacity = $state(1);
  let rangePrimed = false;
  $effect(() => {
    void range;
    // Fade only when the user changes range; the first paint is not a cue.
    if (!rangePrimed) {
      rangePrimed = true;
      return;
    }
    plotOpacity = 0.6;
    const id = requestAnimationFrame(() => (plotOpacity = 1));
    return () => cancelAnimationFrame(id);
  });

  const readoutT = $derived(cursor ?? hoverT ?? edge);
  const readoutLane = $derived(selectedLane ?? lanes[0] ?? null);
  const readoutV = $derived(readoutLane ? valueAt(readoutLane, readoutT) : null);
  const readoutKv = $derived.by(() => {
    if (singleBackend !== null) return null;
    const kv = lanes.find((l) => l.key === "__fleet");
    if (!kv) return null;
    return valueAt(kv, readoutT);
  });

  const backendRows = $derived.by(() => {
    void framesVersion;
    if (layer.device) return [];
    const from = domain.from;
    const to = Math.max(domain.to, edge);
    return lanes.map((lane) => ({
      lane,
      stats: summaryAt(lane, from, to),
    }));
  });

  const deviceRows = $derived.by(() => {
    const gpus = history?.gpu ?? [];
    return gpus.map((g) => {
      const i = g.t.length ? nearestIndex(g.t, readoutT) : -1;
      const val = (k: keyof typeof g.series): number | null =>
        i >= 0 ? (g.series[k]?.[i] ?? null) : null;
      return {
        key: g.key,
        name: g.name || g.key,
        util: val("utilPct"),
        used: val("memUsedMB"),
        total: val("memTotalMB"),
        temp: val("tempC"),
        power: val("powerW"),
      };
    });
  });

  const hasFrames = $derived.by(() => {
    void framesVersion;
    return frames.length > 0;
  });

  const sliderTimes = $derived(
    singleServerLane
      ? (history?.backends.find((b) => b.name === singleBackend)?.t ?? [])
      : (history?.fleet.t ?? []),
  );
  const sliderIndex = $derived(
    sliderTimes.length ? nearestIndex(sliderTimes, cursor ?? edge) : 0,
  );

  function xOf(t: number, x0: number, x1: number): number {
    return x0 + ((t - domain.from) / (domain.to - domain.from)) * (x1 - x0);
  }

  function cssVar(cs: CSSStyleDeclaration, name: string): string {
    return cs.getPropertyValue(name).trim();
  }

  function rgba(hex: string, alpha: number): string {
    const h = hex.replace("#", "");
    if (h.length !== 6) return `rgba(185,166,132,${alpha})`;
    const r = parseInt(h.slice(0, 2), 16);
    const g = parseInt(h.slice(2, 4), 16);
    const b = parseInt(h.slice(4, 6), 16);
    return `rgba(${r},${g},${b},${alpha})`;
  }

  function layerVar(id: LayerId): string {
    switch (id) {
      case "kv":
        return "--water";
      case "ttft":
        return "--sand";
      case "engineTtft":
        return "--water";
      case "prefill":
        return "--amber";
      case "decode":
        return "--moss";
      case "util":
      case "vram":
        return "--sand-lit";
    }
  }

  function layerColor(id: LayerId): string {
    return `var(${layerVar(id)})`;
  }

  function layerLine(id: LayerId): string {
    return id === "kv" || id === "ttft" ? "var(--sand)" : `var(${layerVar(id)})`;
  }

  function timeTicks(from: number, to: number): number[] {
    const count = w > 1100 ? 6 : w > 720 ? 4 : 3;
    const out: number[] = [];
    for (let i = 0; i <= count; i++) out.push(from + ((to - from) * i) / count);
    return out;
  }

  function draw(): void {
    if (!canvas) return;
    const dpr = Math.min(2, window.devicePixelRatio || 1);
    const cw = Math.max(320, w);
    const ch = Math.max(180, h);
    if (canvas.width !== Math.round(cw * dpr) || canvas.height !== Math.round(ch * dpr)) {
      canvas.width = Math.round(cw * dpr);
      canvas.height = Math.round(ch * dpr);
    }
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cw, ch);

    const cs = getComputedStyle(document.documentElement);
    const ink = cssVar(cs, "--ink");
    const ink2 = cssVar(cs, "--ink-2");
    const ink3 = cssVar(cs, "--ink-3");
    const rule = cssVar(cs, "--rule");
    const rule2 = cssVar(cs, "--rule-2");
    const water = cssVar(cs, "--water");
    const sand = cssVar(cs, "--sand");
    const amber = cssVar(cs, "--amber");
    const moss = cssVar(cs, "--moss");
    const verm = cssVar(cs, "--verm");
    const panel = cssVar(cs, "--panel");
    const layerToken = layerVar(layerId);
    const lineColor = layerToken === "--water" && (layerId === "kv" || layerId === "ttft")
      ? sand
      : cssVar(cs, layerToken);
    const fillColor = cssVar(cs, layerToken);
    const mono = "'Overpass Mono', ui-monospace, monospace";
    const ui = "'Overpass', system-ui, sans-serif";

    const x0 = padL;
    const x1 = cw - PAD_R;
    const y0 = PAD_T;
    const y1 = ch - PAD_B;
    const n = Math.max(1, lanes.length);
    const laneH = (y1 - y0) / n;

    ctx.font = `10px ${mono}`;
    ctx.textBaseline = "middle";

    // time grid
    ctx.strokeStyle = rule;
    ctx.lineWidth = 1;
    const ticks = timeTicks(domain.from, domain.to);
    for (let i = 0; i < ticks.length; i++) {
      const t = ticks[i];
      const x = Math.round(xOf(t, x0, x1)) + 0.5;
      ctx.beginPath();
      ctx.moveTo(x, y0);
      ctx.lineTo(x, y1);
      ctx.stroke();
      ctx.fillStyle = ink3;
      ctx.textAlign = i === ticks.length - 1 ? "right" : "left";
      ctx.fillText(tickLabel(t, range), i === ticks.length - 1 ? x - 3 : x + 4, y1 + 12);
    }

    if (lanes.length === 0) {
      ctx.fillStyle = ink3;
      ctx.textAlign = "center";
      ctx.font = `12px ${ui}`;
      ctx.fillText(
        paused ? "PAUSED — NO FRAME" : "NO HISTORY YET — SAMPLING ONCE PER SECOND",
        (x0 + x1) / 2,
        (y0 + y1) / 2,
      );
      return;
    }

    for (let i = 0; i < lanes.length; i++) {
      const lane = lanes[i];
      const top = y0 + i * laneH;
      const base = top + laneH - 8;
      const laneScale = laneH - 14;
      const dim = !singleServerLane && selected && !lane.isFleet && lane.key !== selected;
      const laneInk = dim ? rgba(ink2, 0.45) : ink2;

      // baseline + scale rules
      ctx.strokeStyle = rule;
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(x0, Math.round(base) + 0.5);
      ctx.lineTo(x1, Math.round(base) + 0.5);
      ctx.stroke();
      for (const tick of scale.ticks) {
        const ty = Math.round(base - (tick.v / scale.max) * laneScale) + 0.5;
        if (ty < top) continue;
        ctx.strokeStyle = rgba(ink3, 0.08);
        ctx.beginPath();
        ctx.moveTo(x0, ty);
        ctx.lineTo(x1, ty);
        ctx.stroke();
        if (i === 0 || laneH > 64) {
          ctx.fillStyle = ink3;
          ctx.textAlign = "right";
          ctx.font = `10px ${mono}`;
          ctx.fillText(tick.label, x0 - 8, ty);
        }
      }

      if (!singleServerLane) {
        ctx.fillStyle = lane.isFleet ? sand : laneInk;
        ctx.textAlign = "right";
        ctx.font = `${lane.isFleet ? "500 " : ""}${laneH > 44 ? "12" : "11"}px ${mono}`;
        const labelMax = Math.max(5, Math.floor((padL - 54) / 6.6));
        ctx.fillText(
          lane.label.length > labelMax
            ? `${lane.label.slice(0, labelMax - 1)}…`
            : lane.label,
          x0 - 46,
          top + 10,
        );
        if (lane.tier != null) {
          ctx.fillStyle = rgba(sand, dim ? 0.4 : 0.85);
          ctx.font = `9px ${mono}`;
          ctx.fillText(`T${lane.tier}`, x0 - 46, top + 24);
        }
      }

      // contiguous segments (nulls break the line; never interpolated)
      let seg: { x: number; y: number }[] = [];
      const flush = () => {
        if (seg.length < 2) {
          seg = [];
          return;
        }
        // fill under the segment
        ctx.beginPath();
        ctx.moveTo(seg[0].x, base);
        for (const p of seg) ctx.lineTo(p.x, p.y);
        ctx.lineTo(seg[seg.length - 1].x, base);
        ctx.closePath();
        ctx.fillStyle = rgba(fillColor, dim ? 0.06 : 0.16);
        ctx.fill();
        // ridge
        ctx.beginPath();
        ctx.moveTo(seg[0].x, seg[0].y);
        for (const p of seg) ctx.lineTo(p.x, p.y);
        ctx.strokeStyle = rgba(lineColor, dim ? 0.35 : 0.95);
        ctx.lineWidth = 1.5;
        ctx.stroke();
        seg = [];
      };

      for (const p of lane.points) {
        if (p.t < domain.from || p.t > domain.to) continue;
        if (p.v == null || !Number.isFinite(p.v)) {
          flush();
          continue;
        }
        const x = xOf(p.t, x0, x1);
        const y = base - clamp(p.v / scale.max, 0, 1) * laneScale;
        seg.push({ x, y });
      }
      flush();

      // hatch across the lane when there is no usable data at all
      const any = lane.points.some((p) => p.v != null);
      if (!any && laneH > 18) {
        ctx.save();
        ctx.beginPath();
        ctx.rect(x0, top, x1 - x0, laneH);
        ctx.clip();
        ctx.strokeStyle = rgba(ink3, 0.16);
        ctx.lineWidth = 1;
        for (let hx = x0 - laneH; hx < x1; hx += 14) {
          ctx.beginPath();
          ctx.moveTo(hx, base);
          ctx.lineTo(hx + laneH, top + 2);
          ctx.stroke();
        }
        ctx.restore();
        ctx.fillStyle = rgba(ink3, 0.62);
        ctx.font = `9px ${mono}`;
        ctx.textAlign = "left";
        ctx.fillText("NO DATA", x0 + 8, top + laneH / 2);
      }

      // request bars: completed + live, 5m only (a detail overlay)
      if (range === "5m" && !lane.isFleet && !lane.isDevice) {
        const now = Date.now();
        for (const r of requests) {
          if (r.backend !== lane.key) continue;
          if (r.status >= 400) continue;
          const start = r.start || r.t - r.durMs;
          const end = r.t;
          if (end < domain.from || start > domain.to) continue;
          const bx0 = xOf(Math.max(start, domain.from), x0, x1);
          const bx1 = xOf(Math.min(end, domain.to), x0, x1);
          if (bx1 - bx0 < 0.5) continue;
          const yy = base - 3;
          const pre = r.ttftMs > 0 ? xOf(Math.min(start + r.ttftMs, end), x0, x1) : bx0;
          ctx.fillStyle = rgba(amber, 0.7);
          ctx.fillRect(bx0, yy, Math.max(0, pre - bx0), 2.5);
          ctx.fillStyle = rgba(moss, 0.55);
          ctx.fillRect(pre, yy, Math.max(0, bx1 - pre), 2.5);
        }
        for (const l of sessions?.live ?? []) {
          if (l.backend !== lane.key) continue;
          const bx0 = xOf(Math.max(l.startMs, domain.from), x0, x1);
          const bx1 = xOf(Math.min(now, domain.to), x0, x1);
          if (bx1 - bx0 < 0.5) continue;
          ctx.save();
          ctx.setLineDash([2, 3]);
          ctx.strokeStyle = rgba(sand, 0.55);
          ctx.beginPath();
          ctx.moveTo(bx0, base - 4.5);
          ctx.lineTo(bx1, base - 4.5);
          ctx.stroke();
          ctx.restore();
          if (l.phase === "streaming") {
            ctx.fillStyle = rgba(moss, 0.7);
            ctx.fillRect(bx0, base - 7, Math.max(0, bx1 - bx0), 2.5);
          } else {
            ctx.fillStyle = rgba(amber, 0.8);
            ctx.fillRect(bx0, base - 7, Math.max(0, bx1 - bx0), 2.5);
          }
        }
        for (const r of requests) {
          if (r.backend !== lane.key || r.status < 400) continue;
          const x = xOf(r.t, x0, x1);
          if (x < x0 || x > x1) continue;
          ctx.fillStyle = verm;
          ctx.fillRect(x - 1, base - 8, 2, 8);
        }
      }

      // live-edge spot height
      const current = lane.current;
      if (current != null) {
        ctx.fillStyle = dim ? rgba(ink3, 0.5) : ink;
        ctx.textAlign = "right";
        ctx.font = `10px ${mono}`;
        ctx.fillText(fmtLayerValue(layer, current), x1 - 2, top + 10);
        const cy = base - clamp(current / scale.max, 0, 1) * laneScale;
        ctx.fillStyle = dim ? rgba(lineColor, 0.4) : lineColor;
        ctx.beginPath();
        ctx.arc(x1 - 4, cy, 1.8, 0, Math.PI * 2);
        ctx.fill();
      }
      // saturation marker: rejections are a capacity figure, so it stays water
      // (the One Failure Rule keeps vermilion for 5xx/failure only)
      if (!lane.isFleet && !lane.isDevice && range === "5m" && laneH > 18) {
        const cut = Date.now() - 5 * 60_000;
        let rej = 0;
        for (const r of requests) {
          if (r.backend === lane.key && r.status >= 400 && r.t >= cut) rej++;
        }
        if (rej > 0) {
          ctx.fillStyle = dim ? rgba(ink3, 0.5) : rgba(water, 0.9);
          ctx.textAlign = "right";
          ctx.font = `9px ${mono}`;
          ctx.fillText(`429 ${rej}`, x1 - 2, top + 21);
        }
      }
    }

    // live edge
    const ex = Math.min(x1 - 0.5, Math.round(xOf(edge, x0, x1)) + 0.5);
    if (ex >= x0 && ex <= x1) {
      ctx.strokeStyle = rule2;
      ctx.beginPath();
      ctx.moveTo(ex, y0);
      ctx.lineTo(ex, y1);
      ctx.stroke();
      ctx.fillStyle = sand;
      ctx.fillRect(ex - 2, y0 - 4, 4, 4);
      ctx.fillStyle = ink3;
      ctx.font = `9px ${mono}`;
      ctx.textAlign = "right";
      ctx.fillText(stale ? "STALE" : paused ? "HELD" : "LIVE", ex - 5, y0 - 6);
    }

    // survey cursor
    if (cursor != null) {
      const cx = Math.round(xOf(cursor, x0, x1)) + 0.5;
      if (cx >= x0 && cx <= x1) {
        ctx.strokeStyle = ink;
        ctx.lineWidth = 1;
        ctx.beginPath();
        ctx.moveTo(cx, y0);
        ctx.lineTo(cx, y1);
        ctx.stroke();
        if (readoutV != null && readoutLane) {
          const idx = lanes.indexOf(readoutLane);
          const top = y0 + idx * laneH;
          const base = top + laneH - 8;
          const cy = base - clamp(readoutV / scale.max, 0, 1) * (laneH - 14);
          ctx.fillStyle = panel;
          ctx.beginPath();
          ctx.arc(cx, cy, 3, 0, Math.PI * 2);
          ctx.fill();
          ctx.strokeStyle = ink;
          ctx.beginPath();
          ctx.arc(cx, cy, 3, 0, Math.PI * 2);
          ctx.stroke();
        }
      }
    }

    // frame
    ctx.strokeStyle = rule2;
    ctx.lineWidth = 1;
    ctx.strokeRect(x0 + 0.5, y0 + 0.5, x1 - x0 - 1, y1 - y0 - 1);
  }

  $effect(() => {
    // dependencies that must trigger a redraw
    void history;
    void framesVersion;
    void layerId;
    void cursor;
    void hoverT;
    void edge;
    void selected;
    void w;
    void h;
    void range;
    void paused;
    void theme;
    void inView;
    if (!inView) return;
    draw();
  });

  $effect(() => {
    if (!host) return;
    const motion = window.matchMedia("(prefers-reduced-motion: reduce)");
    reduced = motion.matches;
    // The canvas snap must track the CSS 0.01ms kill even when the OS
    // preference changes mid-session; a change also triggers a redraw.
    const onMotion = () => (reduced = motion.matches);
    motion.addEventListener("change", onMotion);
    const ro = new ResizeObserver((entries) => {
      const box = entries[0]?.contentRect;
      if (box) {
        w = box.width;
        h = box.height;
      }
    });
    ro.observe(host);
    // Do not run the live-edge loop when the profile is scrolled away; the
    // sheet redraws on data ticks and catches up on return.
    const io = new IntersectionObserver(
      (entries) => {
        inView = entries[0]?.isIntersecting ?? true;
      },
      { threshold: 0 },
    );
    io.observe(host);
    return () => {
      motion.removeEventListener("change", onMotion);
      ro.disconnect();
      io.disconnect();
    };
  });

  // Smooth live edge: the data arrives at 1 s, the edge glides for 500 ms.
  $effect(() => {
    void framesVersion;
    const target = Date.now();
    if (reduced || !inView) {
      edge = target;
      return;
    }
    animFrom = untrack(() => edge);
    animStart = performance.now();
    lastEdgePaint = 0;
    cancelAnimationFrame(raf);
    const step = (now: number) => {
      // One continuous second: the next frame tick restarts exactly where
      // this one ends, so the live edge never pulses or stalls. Paint the
      // slow-moving edge at ~30fps; the canvas never runs at full refresh
      // rate for a 1.6px/s movement.
      const p = Math.min(1, (now - animStart) / 1000);
      if (p >= 1 || now - lastEdgePaint >= 32) {
        edge = animFrom + (target - animFrom) * p;
        lastEdgePaint = now;
      }
      if (p < 1) raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  });

  function onPointer(e: PointerEvent): void {
    if (!canvas) return;
    const rect = canvas.getBoundingClientRect();
    // The canvas backs at a 320px logical minimum; map the CSS pointer into
    // that logical width so hover values stay aligned below 320px.
    const cw = Math.max(320, rect.width);
    const x = ((e.clientX - rect.left) / rect.width) * cw;
    const x0 = padL;
    const x1 = cw - PAD_R;
    if (x < x0 || x > x1) {
      hoverT = null;
      return;
    }
    hoverT = domain.from + ((x - x0) / (x1 - x0)) * (domain.to - domain.from);
  }

  function pin(): void {
    if (hoverT != null) cursor = hoverT;
  }

  function onSlider(e: Event): void {
    const v = Number((e.target as HTMLInputElement).value);
    const t = sliderTimes[v];
    if (t != null) cursor = t;
  }

  function toLive(): void {
    cursor = null;
    hoverT = null;
  }

  onMount(() => {
    const onKey = (e: KeyboardEvent) => {
      if (cursor == null) return;
      if (e.key !== "Escape" && e.key !== "l" && e.key !== "L") return;
      const target = e.target;
      if (
        typeof HTMLElement !== "undefined" &&
        target instanceof HTMLElement &&
        target.closest("input, textarea, select, [contenteditable='true']")
      ) {
        return;
      }
      e.preventDefault();
      toLive();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });
</script>

<div class="profile">
  <div class="profile-head">
    <div class="row wrap">
      <span class="label">{singleBackend !== null ? "SERVER PROFILE" : "FLEET PROFILE"}</span>
      <span class="tag {layer.source}">{layer.source}</span>
      <span class="detail muted">{layer.note}</span>
    </div>
    <div class="row right">
      <span class="readout mono" aria-live="off">
        {#if cursor != null}
          <span class="sand">T {fmtClock(cursor)}</span>
        {:else}
          <span class="muted">{stale ? "stale" : paused ? "held" : "live"} · {fmtClock(readoutT)}</span>
        {/if}
        <span class="sep">/</span>
        <span>{layer.short} {fmtLayerValue(layer, readoutV)}</span>
        {#if readoutKv != null && layerId !== "kv"}
          <span class="sep">/</span>
          <span class="muted" class:companion={cursor != null}>KV {fmtPct(readoutKv)}</span>
        {/if}
      </span>
      {#if cursor != null}
        <button class="btn" onclick={toLive} title="Return to live (L or Esc)">RETURN TO LIVE</button>
      {/if}
    </div>
  </div>

  <div class="profile-plot" bind:this={host}>
    <canvas
      bind:this={canvas}
      aria-hidden="true"
      style:opacity={plotOpacity}
      onpointermove={onPointer}
      onpointerleave={() => (hoverT = null)}
      onclick={pin}
    ></canvas>
    {#if !history && !hasFrames}
      <div class="profile-note">waiting for the first frame…</div>
    {:else if layer.device && lanes.length === 0}
      <div class="profile-note">
        {#if history?.gpuFeed.status === "err"}
          device exporter error — {history.gpuFeed.lastError ?? "unreachable"}
        {:else if history?.gpuFeed.status === "off"}
          device exporter absent — check <span class="mono">GPU_METRICS_URL</span>
        {:else if !history?.gpuFeed.url}
          no device telemetry — set <span class="mono">GPU_METRICS_URL</span> to a DCGM or
          nvidia exporter
        {:else}
          waiting for the device exporter…
        {/if}
      </div>
    {/if}
  </div>

  <div class="profile-foot">
    <div class="summary mono">
      {#key layerId}
        <span class="muted">last</span>
        <TweenNumber value={summary.last} format={(v) => fmtLayerValue(layer, v)} />
        <span class="sep">/</span>
        <span class="muted">avg</span>
        <TweenNumber value={summary.avg} format={(v) => fmtLayerValue(layer, v)} />
        <span class="sep">/</span>
        <span class="muted">min</span>
        <TweenNumber value={summary.min} format={(v) => fmtLayerValue(layer, v)} />
        <span class="sep">/</span>
        <span class="muted">max</span>
        <TweenNumber value={summary.max} format={(v) => fmtLayerValue(layer, v)} />
      {/key}
      {#if singleBackend === null && history?.ttft && history.ttft.n > 0}
        <span class="sep">/</span>
        <span class="muted">router ttft</span> p50 {fmtLayerValue(layerById("ttft"), history.ttft.p50)}
        p95 {fmtLayerValue(layerById("ttft"), history.ttft.p95)}
        <span class="muted">n {history.ttft.n}</span>
      {/if}
    </div>
    <div class="legend">
      <span class="key">
        <i
          class="swatch area"
          style:background={layerColor(layerId)}
          style:border-top-color={layerLine(layerId)}
        ></i>{layer.short}
      </span>
      <span class="key"><i class="swatch amber"></i>prefill</span>
      <span class="key"><i class="swatch moss"></i>decode</span>
      <span class="key"><i class="swatch verm"></i>error</span>
      <span class="key"><i class="swatch hatch"></i>no data</span>
    </div>
  </div>

  {#if !layer.device && backendRows.length}
    <div class="sr-only">
      <table aria-label={`${layer.label} history by backend`}>
        <caption>
          {layer.label} over the visible {range} window; {cursor != null
            ? `cursor at ${fmtClock(cursor)}`
            : "live"}.
        </caption>
        <thead>
          <tr>
            <th>Lane</th>
            <th>Last</th>
            <th>Average</th>
            <th>Minimum</th>
            <th>Maximum</th>
            {#if cursor != null}<th>At cursor</th>{/if}
          </tr>
        </thead>
        <tbody>
          {#each backendRows as row (row.lane.key)}
            <tr>
              <th scope="row">
                {row.lane.isFleet
                  ? "Fleet"
                  : `${row.lane.label}${singleBackend === null && row.lane.tier != null
                      ? ` — ${row.lane.tier === 0 ? "King" : "Subject"} (T${row.lane.tier})`
                      : ""}`}
              </th>
              <td>{fmtLayerValue(layer, row.stats.last)}</td>
              <td>{fmtLayerValue(layer, row.stats.avg)}</td>
              <td>{fmtLayerValue(layer, row.stats.min)}</td>
              <td>{fmtLayerValue(layer, row.stats.max)}</td>
              {#if cursor != null}
                <td>{fmtLayerValue(layer, valueAt(row.lane, readoutT))}</td>
              {/if}
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}

  <div class="sr-only">
    <table aria-label="device history">
      <caption>
        Device history at {cursor != null ? fmtClock(cursor) : "live"} — {layer.label}
      </caption>
      <thead>
        <tr>
          <th>Device</th>
          <th>Utilization</th>
          <th>VRAM used (MiB)</th>
          <th>VRAM total (MiB)</th>
          <th>Temperature (°C)</th>
          <th>Power (W)</th>
        </tr>
      </thead>
      <tbody>
        {#each deviceRows as d (d.key)}
          <tr>
            <td>{d.name}</td>
            <td>{d.util == null ? "—" : `${d.util.toFixed(1)}%`}</td>
            <td>{d.used == null ? "—" : Math.round(d.used)}</td>
            <td>{d.total == null ? "—" : Math.round(d.total)}</td>
            <td>{d.temp == null ? "—" : `${d.temp.toFixed(0)}°C`}</td>
            <td>{d.power == null ? "—" : `${d.power.toFixed(0)}W`}</td>
          </tr>
        {:else}
          <tr><td colspan="6">no device telemetry</td></tr>
        {/each}
      </tbody>
    </table>
  </div>

  <div class="profile-ruler">
    <label class="label" for="survey-time">SURVEY TIME</label>
    <input
      id="survey-time"
      type="range"
      min="0"
      max={Math.max(0, sliderTimes.length - 1)}
      step="1"
      value={sliderIndex}
      disabled={sliderTimes.length < 2}
      oninput={onSlider}
      aria-valuetext={cursor != null
        ? `${fmtHHMM(cursor)} ${layer.short} ${fmtLayerValue(layer, readoutV)}`
        : "live"}
    />
    <span class="ruler-ends mono muted">HISTORY</span>
    <span class="ruler-ends mono muted">LIVE</span>
  </div>
</div>

<style>
  .profile {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .profile-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s3);
    flex-wrap: wrap;
    min-height: 40px;
    padding: 6px var(--s3);
    border-bottom: 1px solid var(--rule);
  }
  .readout {
    font-size: var(--fs-detail);
    color: var(--ink-2);
    white-space: nowrap;
  }
  /* Pinned: the mirror KV is the companion invariant, not a second reading.
     Demote it so TTFT 698ms / KV 698% no longer reads as a duplication. */
  .readout .companion {
    font-size: var(--fs-label);
    color: var(--ink-3);
  }
  .sep {
    color: var(--ink-3);
    padding: 0 6px;
    opacity: 0.7;
  }
  .profile-plot {
    position: relative;
    flex: 1 1 auto;
    min-height: 0;
    min-width: 0;
  }
  /* The 320px paper floor holds only where the owner has not capped the live
     band to the fold; at >=1280 the capped band can be smaller than the stack
     the floor would demand (320 + head 40 + foot 65 + ruler 52), so the floor
     moves to 96px — the plot's minimum printable canvas (axis + lane
     baseline) — and the plot flexes to whatever the band leaves. The session
     list below keeps its 280px step, so the sheet stays coherent. */
  @media (min-width: 1280px) {
    .profile-plot {
      min-height: 96px;
    }
  }
  canvas {
    display: block;
    width: 100%;
    height: 100%;
    cursor: crosshair;
    touch-action: pan-y;
    transition: opacity var(--dur) var(--ease-out);
  }
  .profile-note {
    position: absolute;
    inset: 0;
    display: grid;
    place-items: center;
    padding: var(--s6);
    text-align: center;
    font-size: var(--fs-detail);
    color: var(--ink-3);
    pointer-events: none;
  }
  .profile-foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--s3);
    flex-wrap: wrap;
    padding: 6px var(--s3);
    border-top: 1px solid var(--rule);
  }
  .summary {
    font-size: var(--fs-label);
    color: var(--ink-2);
    white-space: normal;
    overflow-wrap: anywhere;
  }
  .summary .muted {
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }
  .legend {
    display: flex;
    align-items: center;
    gap: var(--s3);
    flex-wrap: wrap;
  }
  .key {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: var(--fs-label);
    letter-spacing: 0.08em;
    text-transform: uppercase;
    color: var(--ink-3);
  }
  .swatch {
    width: 12px;
    height: 8px;
    display: inline-block;
    border: 1px solid var(--rule-2);
  }
  .swatch.area {
    height: 8px;
    border: 0;
    border-top: 2px solid var(--sand);
    opacity: 0.9;
  }
  .swatch.amber {
    background: var(--amber);
    border: 0;
  }
  .swatch.moss {
    background: var(--moss);
    border: 0;
  }
  .swatch.verm {
    background: var(--verm);
    border: 0;
    width: 2px;
    height: 10px;
  }
  .swatch.hatch {
    background: repeating-linear-gradient(
      -45deg,
      var(--rule-2) 0 1px,
      transparent 1px 4px
    );
  }
  .profile-ruler {
    display: grid;
    grid-template-columns: auto 1fr auto auto;
    align-items: center;
    gap: var(--s3);
    padding: 2px var(--s3) 6px;
  }
  .ruler-ends {
    font-size: var(--fs-label);
    letter-spacing: 0.1em;
  }
  @media (max-width: 959px) {
    .profile-plot {
      min-height: 280px;
    }
    .profile-head {
      align-items: flex-start;
    }
    .readout {
      white-space: normal;
    }
  }
  @media (max-width: 639px) {
    .profile-plot {
      min-height: 240px;
    }
    .profile-foot {
      align-items: flex-start;
    }
    .summary {
      white-space: normal;
    }
  }
</style>
