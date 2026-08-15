<script lang="ts">
  import { tick } from "svelte";
  import { clock } from "./fmt.js";
  import type { BurstRequest } from "./metrics.js";

  // The request tape: a horizontally scrolling mono line of the most recent
  // requests — the kept "continuous transcript" discipline. Gold for 200,
  // valley-blue for 429 (capacity rejections), signal-red for 5xx.
  //
  // The scroll is a single CSS keyframes loop (constant px/s), NOT a 60fps
  // rAF: duration is computed once per feed update from the track width so
  // the speed stays constant in px/s no matter how long the line gets.
  // Hovering pauses the loop (animation-play-state) for inspection;
  // prefers-reduced-motion pins it. A per-second spike counter sits in the
  // head. Entries are duplicated into two halves so the loop is seamless.

  interface Props {
    requests: BurstRequest[];
    spikeCount: number;
    live: boolean;
  }

  let { requests, spikeCount, live }: Props = $props();

  const SPEED = 34; // px per second — a calm, readable scroll

  let track: HTMLDivElement | undefined = $state();
  let duration = $state(0);
  let paused = $state(false);
  let counted = 0;

  const newest = $derived(requests.length ? requests[requests.length - 1].t : 0);
  const shown = $derived.by(() => {
    void newest;
    return requests.slice(-40);
  });

  // re-measure only when the entry count shifts — recomputing the duration
  // on every feed update would restart the CSS loop and jump the tape
  $effect(() => {
    const n = shown.length;
    if (Math.abs(n - counted) < 2) return;
    counted = n;
    return void tick().then(() => {
      if (!track) return;
      const half = track.scrollWidth / 2;
      duration = half > 0 ? half / SPEED : 0;
    });
  });

  function statusClass(s: number): string {
    if (s >= 500) return "st-err";
    if (s === 429) return "st-throttle";
    if (s >= 400) return "st-err";
    return "st-ok";
  }

  const dur = (ms: number) =>
    ms >= 10000 ? (ms / 1000).toFixed(1) + "s" : ms.toFixed(0) + "ms";
</script>

<div class="tape-wrap" aria-label="recent request tape">
  <div class="tape-head unskew">
    <span>request tape</span>
    <span class="spikes {spikeCount > 0 ? 'hot' : ''}">spikes {spikeCount}/5s</span>
  </div>
  <div
    class="tape-vp"
    role="presentation"
    onmouseenter={() => (paused = true)}
    onmouseleave={() => (paused = false)}
  >
    {#if shown.length === 0}
      <div class="tape-idle unskew">
        no requests recorded yet — the sheet is quiet
      </div>
    {:else}
      <div
        class="tape-track"
        bind:this={track}
        style:animation-duration="{duration ? duration + 's' : '0s'}"
        class:paused
      >
        {#each [0, 1] as halfI (halfI)}
          <span class="tape-half">
            {#each shown as q (q.t + "-" + q.backend + "-" + halfI)}
              <span class="tape-entry">
                <span class="t">{clock(q.t)}</span>
                <span class="be">{q.backend}</span>
                <span class="pa">{q.path}</span>
                <span class="st {statusClass(q.status)}">{q.status}</span>
                <span class="num">{q.ttftMs > 0 ? q.ttftMs + 'ms' : '—'}</span>
                <span class="num">{dur(q.durMs)}</span>
                <span class="num">~{q.newTokensEst}</span>
                <span class="sep">/</span>
              </span>
            {/each}
          </span>
        {/each}
      </div>
    {/if}
  </div>
  <div
    class="tape-head unskew"
    style="padding: 0 14px 5px; justify-content: flex-start"
  >
    <span>t · backend · path · status · ttft · dur · tok(est)</span>
  </div>
</div>

<style>
  @keyframes tape-scroll {
    from {
      transform: translate3d(0, 0, 0);
    }
    to {
      transform: translate3d(-50%, 0, 0);
    }
  }
  .tape-track {
    animation: tape-scroll linear infinite;
  }
  .tape-track.paused {
    animation-play-state: paused;
  }
  @media (prefers-reduced-motion: reduce) {
    .tape-track {
      animation: none !important;
    }
  }
</style>
