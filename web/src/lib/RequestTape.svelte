<script lang="ts">
  import { onMount, onDestroy, tick } from "svelte";
  import { clock } from "./fmt.js";
  import type { BurstRequest } from "./metrics.js";

  // The request tape: a horizontally scrolling mono line of the most recent
  // requests — the kept "continuous transcript" discipline. Gold for 200,
  // valley-blue for 429 (capacity rejections), signal-red for 5xx (see the
  // red reservation rule: red is for failures, blue is for policy rejections).
  //
  // The scroll is a single CSS keyframes loop (constant px/s), NOT a 60fps
  // rAF: duration is computed once per feed update from the track width so
  // the speed stays constant in px/s no matter how long the line gets.
  // Hovering pauses the loop (animation-play-state) for inspection; a tap
  // does the same on touch (where there is no hover);
  // prefers-reduced-motion pins it. The head carries the field legend and a
  // spike counter with a 60s peak so the evidence does not decay away.

  interface Props {
    requests: BurstRequest[];
    spikeCount: number;
    /** max of spikeCount over the last 60s */
    spikePeak: number;
    live: boolean;
    /** feed dropped: freeze the last transcript in place */
    stale: boolean;
  }

  let { requests, spikeCount, spikePeak, live, stale }: Props = $props();

  const SPEED = 34; // px per second — a calm, readable scroll

  let track: HTMLDivElement | undefined = $state();
  let duration = $state(0);
  let paused = $state(false);
  let only429 = $state(false);
  let counted = 0;
  let resumeTimer: ReturnType<typeof setTimeout> | null = null;

  const newest = $derived(requests.length ? requests[requests.length - 1].t : 0);
  const shown = $derived.by(() => {
    void newest;
    const src = only429 ? requests.filter((q) => q.status >= 429) : requests;
    return src.slice(-40);
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

  // A request is identified by its full field set: multiple requests can
  // complete in the same millisecond (same `t`, even same backend), so
  // keying on t+backend alone collides and Svelte drops the duplicates.
  const key = (q: BurstRequest) =>
    `${q.t}:${q.backend}:${q.status}:${q.ttftMs}:${q.durMs}:${q.newTokensEst}:${q.bytesIn}`;

  // touch has no hover: a touch-down pauses the loop, a touch-up gives the
  // eye a 1.5s grace before it resumes
  function pressStart() {
    if (resumeTimer) clearTimeout(resumeTimer);
    paused = true;
  }
  function pressEnd() {
    if (resumeTimer) clearTimeout(resumeTimer);
    resumeTimer = setTimeout(() => (paused = false), 1500);
  }

  onMount(() => (counted = shown.length));
  onDestroy(() => {
    if (resumeTimer) clearTimeout(resumeTimer);
  });
</script>

<div class="tape-wrap" aria-label="recent request tape">
  <div class="tape-head unskew">
    <span>request tape · t · backend · path · status · ttft · dur · tok(est)</span>
    <span class="head-right">
      {#if shown.length > 0}
        <button
          class="f429"
          aria-pressed={only429}
          onclick={() => (only429 = !only429)}>
          429s+ only
        </button>
      {/if}
      <span class="spikes {spikeCount > 0 ? 'hot' : ''}">
        spikes {spikeCount}/5s{spikePeak > spikeCount ? ` · peak ${spikePeak}` : ''}
      </span>
    </span>
  </div>
  <div
    class="tape-vp"
    role="presentation"
    onmouseenter={() => (paused = true)}
    onmouseleave={() => (paused = false)}
    onpointerdown={pressStart}
    onpointerup={pressEnd}
  >
    {#if shown.length === 0}
      <div class="tape-idle unskew">
        {#if only429}
          no 429/5xx requests recorded — the fleet has not rejected
        {:else if live}
          no requests recorded yet — the sheet is quiet
        {:else}
          feed offline — no requests recorded
        {/if}
      </div>
    {:else}
      <div
        class="tape-track"
        bind:this={track}
        style:animation-duration="{duration ? duration + 's' : '0s'}"
        class:paused
        class:stale
      >
        {#each [0, 1] as halfI (halfI)}
          <span class="tape-half">
            {#each shown as q (key(q) + ":" + halfI)}
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
  .tape-track.paused,
  .tape-track.stale {
    animation-play-state: paused;
  }
  @media (prefers-reduced-motion: reduce) {
    .tape-track {
      animation: none !important;
    }
  }
</style>
