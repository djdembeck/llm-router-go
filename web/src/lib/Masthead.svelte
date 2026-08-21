<script lang="ts">
  import { onMount, tick } from "svelte";
  import { clock } from "./fmt.js";
  import type { GpuMetrics } from "./metrics.js";

  // Masthead strip: name (Archivo italic), live clock, fleet status line
  // (total in-flight, gpu crease-gauge, tier0 inflight), and the
  // gold-foil [DEPLOY]/[FOLD] packet control — the signature interaction.

  interface Props {
    totalInFlight: number;
    totalWaiting: number;
    gpu: GpuMetrics | null;
    tier0Inflight: number;
    /** sum of the King backends' concurrency caps, null when uncapped. */
    tier0Limit: number | null;
    deployed: boolean;
    /** false in narrow mode (the sheet is force-folded; the button is hidden). */
    showPacketControl: boolean;
    onToggle: () => void;
  }

  let {
    totalInFlight,
    totalWaiting,
    gpu,
    tier0Inflight,
    tier0Limit,
    deployed,
    showPacketControl,
    onToggle,
  }: Props = $props();

  let now = $state(Date.now());

  onMount(() => {
    const iv = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(iv);
  });

  // GPU crease-gauge: one skewed segment per budget slot; filled = used
  // (gold), the next pending slot flashes red when the queue is waiting.
  const segs = $derived(
    gpu && gpu.budget > 0
      ? Array.from({ length: gpu.budget }, (_, i) => ({
          i,
          on: i < gpu.used,
          wait: !gpu.active && i === gpu.used && gpu.waiting > 0,
        }))
      : [],
  );
</script>

<header class="masthead">
  <div>
    <h1 class="mast-title display">llm-router<em>-go</em></h1>
    <div class="mast-sub">fleet sheet · gpu-aware inference routing</div>
  </div>

  <div class="mast-mid">
    <div class="status-line">
      <span>in-flight <b>{totalInFlight}</b></span>
      <span class="stat-dot">·</span>
      <span>waiting <b>{totalWaiting}</b></span>
      <span class="stat-dot">·</span>
      <span>tier0 <b>{tier0Inflight}{tier0Limit ? ` / ${tier0Limit}` : ''}</b></span>
      <span class="stat-dot">·</span>
      {#if gpu && gpu.budget > 0}
        <span
          class="gpu-gauge"
          aria-label="gpu budget {gpu.used} of {gpu.budget} used"
          title="gpu budget — {gpu.used} of {gpu.budget} slots held by tier-0 (king) inflight{gpu.waiting > 0 ? ` · +${gpu.waiting} waiting for a slot` : ''}">
          {#each segs as s (s.i)}
            <i class="gpu-seg {s.on ? 'on' : ''} {s.wait ? 'wait' : ''}"></i>
          {/each}
          <span class="gpu-num">{gpu.used}/{gpu.budget} gpu</span>
          {#if gpu.waiting > 0}<span class="gpu-wait">+{gpu.waiting} waiting</span>{/if}
        </span>
      {:else}
        <span class="gpu-none">gpu budget —</span>
      {/if}
    </div>
  </div>

  <div class="mast-right">
    <time class="clock">{clock(now)}</time>
    {#if showPacketControl}
      <button
        class="packet-btn"
        onclick={onToggle}
        aria-pressed={deployed}
        aria-label="{deployed ? 'fold the sheet to packet mode' : 'deploy the sheet'}">
        [{deployed ? 'FOLD' : 'DEPLOY'}]
      </button>
    {/if}
  </div>
</header>
